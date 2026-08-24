package driver

// sbin_codex: the driver-side UVM coordinator wiring (plan todo 21 of
// mgpusim-uvm-manager). The timing-neutral handlers (fault service, AC/write
// migration, reactive eviction) sit behind ONE coordinator: the intake seams
// stamp each generated root (kernelLaunchOrdinal, sourceBuildOrdinal,
// sourceLocalSequence) with the semantic key components from the routed
// envelope, enqueue the delivered root, and report the completion; the
// driver's Tick runs the one secondary-event serialized drain. The
// coordinator records the causal trace DAG and enforces the
// duplicate-transition-epoch rule; ideal mode zeroes the UVM control latency
// while preserving every functional event and counter.

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// stampFromEnvelope builds the same-mode stamp of a routed root.
func stampFromEnvelope(
	launch, build, seq uint64,
) uvm.SameModeStamp {
	return uvm.SameModeStamp{
		KernelLaunchOrdinal: launch,
		SourceBuildOrdinal:  build,
		SourceLocalSequence: seq,
	}
}

// faultSemanticKey builds the semantic root key of a fault-service root from
// the routed envelope.
func faultSemanticKey(
	req *protocol.PageFaultReq,
) uvm.SemanticRootKey {
	return uvm.SemanticRootKey{
		KernelLaunchOrdinal:     req.KernelLaunchOrdinal,
		SourceComponentStableID: req.SourceComponentStableID,
		OriginKind:              uvm.OriginFaultService,
		PID:                     req.PID,
		GPU:                     req.GPU,
		RegionBase:              SubBlockStartVA(req.VAddr),
		AccessKind:              req.AccessType,
		ProgramCommandOrdinal:   req.ProgramCommandOrdinal,
	}
}

// migrationSemanticKey builds the semantic root key of an AC/write migration
// service root from the routed notification.
func migrationSemanticKey(
	notif *protocol.AccessCounterNotification,
) uvm.SemanticRootKey {
	return uvm.SemanticRootKey{
		KernelLaunchOrdinal:     notif.KernelLaunchOrdinal,
		SourceComponentStableID: notif.SourceComponentStableID,
		OriginKind:              uvm.OriginAccessCounterMigration,
		PID:                     notif.PID,
		GPU:                     notif.GPU,
		RegionBase:              SubBlockStartVA(notif.VAddr),
		AccessKind:              notif.AccessKind,
		ProgramCommandOrdinal:   notif.ProgramCommandOrdinal,
	}
}

// evictionSemanticKey builds the semantic root key of a reactive eviction
// service root (the driver policy is the source).
func evictionSemanticKey(
	d *Driver, pid vm.PID, gpu int, regionBase uint64,
) uvm.SemanticRootKey {
	return uvm.SemanticRootKey{
		KernelLaunchOrdinal:     d.uvmKernelLaunchOrdinal,
		SourceComponentStableID: "driver",
		OriginKind:              uvm.OriginReactiveEviction,
		PID:                     pid,
		GPU:                     gpu,
		RegionBase:              regionBase,
		AccessKind:              vm.AccessKindWrite,
		ProgramCommandOrdinal:   0,
	}
}

// observedAccessProvenance builds the observed-access provenance of a
// delivered demand.
func observedAccessProvenance(
	pid vm.PID, gpu int, regionBase uint64, kind vm.AccessKind,
) string {
	return fmt.Sprintf("observed-access:pid=%d,gpu=%d,region=%#x,kind=%d",
		pid, gpu, regionBase, kind)
}

// enqueueFaultRoot stamps the fault transaction and enqueues its root with
// the coordinator.
func (m *faultServiceMiddleware) enqueueFaultRoot(
	tx *faultTransaction, req *protocol.PageFaultReq,
) {
	d := m.driver
	tx.Stamp = stampFromEnvelope(req.KernelLaunchOrdinal,
		req.SourceBuildOrdinal, req.SourceLocalSequence)
	tx.SemanticKey = faultSemanticKey(req)
	provenance := observedAccessProvenance(req.PID, req.GPU,
		tx.SemanticKey.RegionBase, req.AccessType)
	d.uvmCoordinator.RegisterProvenance(provenance)
	tx.root = &uvm.Root{
		SemanticKey:   tx.SemanticKey,
		Stamp:         tx.Stamp,
		Operation:     "fault-service",
		CurrentVTime:  d.Engine.CurrentTime(),
		OperationOrdinal: 1,
		Provenance:    provenance,
	}
	d.uvmCoordinator.Enqueue(tx.root)
}

// reportFaultRoot reports the fault service completion to the coordinator.
func (m *faultServiceMiddleware) reportFaultRoot(tx *faultTransaction) {
	d := m.driver
	if d.uvmCoordinator == nil || tx.root == nil {
		return
	}
	bytes := uint64(0)
	if tx.plan != nil {
		bytes = tx.plan.TotalBytes
	}
	d.uvmCoordinator.Complete(tx.root, "GPU_RESIDENT", "replayed", bytes,
		d.Engine.CurrentTime())
}

// enqueueMigrationRoot stamps the migration transaction and enqueues its
// root with the coordinator.
func (m *migrationMiddleware) enqueueMigrationRoot(
	tx *migrationTransaction, notif *protocol.AccessCounterNotification,
) {
	d := m.driver
	tx.Stamp = stampFromEnvelope(notif.KernelLaunchOrdinal,
		notif.SourceBuildOrdinal, notif.SourceLocalSequence)
	tx.SemanticKey = migrationSemanticKey(notif)
	provenance := observedAccessProvenance(notif.PID, notif.GPU,
		tx.SemanticKey.RegionBase, notif.AccessKind)
	d.uvmCoordinator.RegisterProvenance(provenance)
	tx.root = &uvm.Root{
		SemanticKey:   tx.SemanticKey,
		Stamp:         tx.Stamp,
		Operation:     "access-counter-migration",
		CurrentVTime:  d.Engine.CurrentTime(),
		OperationOrdinal: 1,
		Provenance:    provenance,
	}
	d.uvmCoordinator.Enqueue(tx.root)
}

// reportMigrationRoot reports the migration completion to the coordinator.
func (m *migrationMiddleware) reportMigrationRoot(tx *migrationTransaction) {
	d := m.driver
	if d.uvmCoordinator == nil || tx.root == nil {
		return
	}
	bytes := uint64(0)
	if tx.plan != nil {
		bytes = tx.plan.TotalBytes
	}
	d.uvmCoordinator.Complete(tx.root, "GPU_RESIDENT", "migrated", bytes,
		d.Engine.CurrentTime())
}

// enqueueEvictionRoot stamps the eviction transaction and enqueues its root
// with the coordinator.
func (m *evictionMiddleware) enqueueEvictionRoot(
	tx *evictionTransaction,
) {
	m.enqueueEvictionRootKind(tx, uvm.OriginReactiveEviction,
		"driver-policy:capacity-shortfall")
}

// enqueuePreEvictionRoot stamps a pre-eviction victim transaction and
// enqueues its root with the coordinator.
func (m *evictionMiddleware) enqueuePreEvictionRoot(
	tx *evictionTransaction,
) {
	m.enqueueEvictionRootKind(tx, uvm.OriginPreEviction,
		"driver-policy:pre-eviction")
}

// enqueueEvictionRootKind stamps the eviction transaction with the service
// kind and enqueues its root with the coordinator.
func (m *evictionMiddleware) enqueueEvictionRootKind(
	tx *evictionTransaction,
	kind uvm.OriginKind,
	provenance string,
) {
	d := m.driver
	tx.Stamp = stampFromEnvelope(d.uvmKernelLaunchOrdinal, 0,
		d.nextUVMDriverSequence())
	tx.SemanticKey = evictionSemanticKey(d, tx.PID, tx.GPU, tx.RegionBase)
	tx.SemanticKey.OriginKind = kind
	d.uvmCoordinator.RegisterProvenance(provenance)
	tx.root = &uvm.Root{
		SemanticKey:     tx.SemanticKey,
		Stamp:           tx.Stamp,
		Operation:       "reactive-eviction",
		CurrentVTime:    d.Engine.CurrentTime(),
		OperationOrdinal: 1,
		Provenance:      provenance,
	}
	d.uvmCoordinator.Enqueue(tx.root)
}

// reportEvictionRoot reports the eviction completion to the coordinator.
func (m *evictionMiddleware) reportEvictionRoot(tx *evictionTransaction) {
	d := m.driver
	if d.uvmCoordinator == nil || tx.root == nil {
		return
	}
	d.uvmCoordinator.Complete(tx.root, "CPU_RESIDENT", "evicted", tx.bytes,
		d.Engine.CurrentTime())
}

// nextUVMDriverSequence returns the next driver-local sequence (the local
// tie-break of the driver-generated roots).
func (d *Driver) nextUVMDriverSequence() uint64 {
	d.uvmDriverSequence++
	return d.uvmDriverSequence
}

// drainUVMCoordinator runs the one secondary-event serialized drain of the
// UVM coordinator (a no-op when UVM is disabled).
func (d *Driver) drainUVMCoordinator() bool {
	if d.uvmCoordinator == nil {
		return false
	}
	return d.uvmCoordinator.Drain(d.Engine.CurrentTime()) > 0
}

// uvmCoordinatorLatency returns the total UVM control latency recorded by
// the coordinator (zero in ideal mode; test accessor).
func (d *Driver) uvmCoordinatorLatency() sim.VTimeInSec {
	if d.uvmCoordinator == nil {
		return 0
	}
	return d.uvmCoordinator.TotalLatency()
}