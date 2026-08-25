package driver

// sbin_codex: driver-side UVM coordinator wiring tests (plan todo 21 of
// mgpusim-uvm-manager). These plain Go tests prove the timing-neutral
// handlers sit behind ONE coordinator: the intake seams stamp each generated
// root (kernelLaunchOrdinal, sourceBuildOrdinal, sourceLocalSequence) with
// the semantic key components from the routed envelope, both modes enqueue
// delivered roots and use one secondary-event serialized drain, the causal
// trace DAG records the fault service, the sourceLocalSequence is excluded
// from cross-mode identity, and ideal mode zeroes the fault latency while
// the logical migration bytes are still counted.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// buildCoordinatorDriver builds a real driver (real allocator, real page
// tables, real UVM manager, coordinator wired) with the FIFO fault service.
func buildCoordinatorDriver(t *testing.T, ideal bool) (
	*Driver, *faultServiceMiddleware,
) {
	t.Helper()

	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	cfg.Ideal = ideal

	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)
	gpuTables := []vm.PageTable{vm.NewPageTable(12), vm.NewPageTable(12)}

	d := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGPUPageTables(gpuTables).
		WithGlobalStorage(mem.NewStorage(8 * mem.GB)).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	gpuPort := sim.NewPort(d, 1, 1, "TestGPU")
	d.RegisterGPU(gpuPort, DeviceProperties{CUCount: 4, DRAMSize: 4 * mem.GB})

	if d.uvmCoordinator == nil {
		t.Fatal("the UVM coordinator must be wired when UVM is enabled")
	}
	return d, d.uvmFault
}

// intakeStampedFault delivers one raw PageFaultReq stamped with the
// coordinator identity (as the CP routing middleware would).
func intakeStampedFault(
	t *testing.T, d *Driver, pid vm.PID, gpu int, vaddr uint64, seq uint64,
) {
	t.Helper()

	req := protocol.PageFaultReqBuilder{}.
		WithSrc(d.gpuPort.AsRemote()).
		WithDst(d.gpuPort.AsRemote()).
		WithPID(pid).
		WithGPU(gpu).
		WithVAddr(vaddr).
		WithAccessType(vm.AccessKindRead).
		WithFaultPendingToken(vm.FaultPendingToken(1)).
		WithKernelLaunchOrdinal(3).
		WithSourceBuildOrdinal(0).
		WithSourceLocalSequence(seq).
		WithSourceComponentStableID("gmmu0").
		WithProgramCommandOrdinal(3).
		Build()
	if err := d.gpuPort.Deliver(req); err != nil {
		t.Fatalf("Deliver PageFaultReq: %v", err)
	}
	if !d.processReturnReq() {
		t.Fatalf("processReturnReq did not consume the fault at %#x", vaddr)
	}
}

// driveZeroWorkFault drives one fault whose demand is already GPU-resident
// (zero DMA/PTE/TLB work, one replay) to completion and drains the
// coordinator.
func driveZeroWorkFault(
	t *testing.T, d *Driver, mw *faultServiceMiddleware, tx *faultTransaction,
) {
	t.Helper()

	if !mw.Tick() {
		t.Fatal("tick did not start the FIFO head")
	}
	mw.Handle(tx.latencyEvent)
	mw.Tick()
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("service requests = %d, want 1 replay", len(reqs))
	}
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("service request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay)
	mw.Tick()
	d.Tick() // the one secondary-event serialized drain
}

// TestUVMNormalAndIdealCoordinator proves both modes run the same functional
// state machine through the same handlers behind one coordinator: identical
// fault counters, identical trace nodes, normal fault latency, zero ideal
// UVM latency.
func TestUVMNormalAndIdealCoordinator(t *testing.T) {
	run := func(ideal bool) (*Driver, *faultServiceMiddleware) {
		d, mw := buildCoordinatorDriver(t, ideal)
		ctx := d.Init()
		pid := ctx.pid
		ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
		reg := d.uvm.registrations[0]
		intakeStampedFault(t, d, pid, 1, uint64(ptr), 1)
		// The demand becomes resident while the transaction is queued (the
		// TBN effect): the 50% occupancy does not expand, so the service
		// issues zero DMA/PTE/TLB work and replays.
		makeRegionResident(t, d, reg, 0, 0)
		tx := mw.queue[0]
		driveZeroWorkFault(t, d, mw, tx)
		return d, mw
	}

	normal, _ := run(false)
	ideal, _ := run(true)

	// The functional counters are identical across the modes.
	if normal.uvm.RawPageFaultCount() != ideal.uvm.RawPageFaultCount() {
		t.Fatalf("raw fault count = %d (normal) vs %d (ideal)",
			normal.uvm.RawPageFaultCount(), ideal.uvm.RawPageFaultCount())
	}
	if normal.uvm.UniqueFaultServiceCount() !=
		ideal.uvm.UniqueFaultServiceCount() {
		t.Fatalf("unique count = %d (normal) vs %d (ideal)",
			normal.uvm.UniqueFaultServiceCount(),
			ideal.uvm.UniqueFaultServiceCount())
	}

	// The causal trace DAG records the fault service identically.
	tn := normal.uvmCoordinator.Trace().Node(
		normal.uvmFault.activeRootKey())
	ti := ideal.uvmCoordinator.Trace().Node(
		ideal.uvmFault.activeRootKey())
	if tn == nil || ti == nil {
		t.Fatal("the trace must record the fault service in both modes")
	}
	if tn.Operation != "fault-service" || tn.Result != "replayed" ||
		tn.State != "GPU_RESIDENT" {
		t.Fatalf("normal trace node = %+v", tn)
	}
	if ti.Operation != "fault-service" || ti.Result != "replayed" ||
		ti.State != "GPU_RESIDENT" {
		t.Fatalf("ideal trace node = %+v", ti)
	}

	// Normal mode charges the modeled fault latency; ideal zeroes it.
	if ideal.uvmCoordinator.TotalLatency() != 0 {
		t.Fatalf("ideal coordinator latency = %v, want 0",
			ideal.uvmCoordinator.TotalLatency())
	}
	if normal.uvmCoordinator.Mode() != uvm.ModeNormal ||
		ideal.uvmCoordinator.Mode() != uvm.ModeIdeal {
		t.Fatal("the coordinator mode must follow the UVM config")
	}
}

// activeRootKey returns the trace key of the last completed fault-service
// root (test helper).
func (m *faultServiceMiddleware) activeRootKey() string {
	if len(m.driver.uvmCoordinator.Trace().Nodes()) == 0 {
		return ""
	}
	nodes := m.driver.uvmCoordinator.Trace().Nodes()
	return nodes[len(nodes)-1].Key
}

// TestUVMSemanticRootIdentity proves the intake stamps each generated root
// with (kernelLaunchOrdinal, sourceBuildOrdinal, sourceLocalSequence) and
// the semantic key components from the routed envelope; the
// sourceLocalSequence is excluded from cross-mode identity.
func TestUVMSemanticRootIdentity(t *testing.T) {
	// Two drivers (the normal and ideal modes) service the same demand with
	// different local sequences: the identity is the semantic key.
	d1, mw1 := buildCoordinatorDriver(t, false)
	d2, mw2 := buildCoordinatorDriver(t, true)
	ctx1 := d1.Init()
	ctx2 := d2.InitWithExistingPID(ctx1)
	ptr1 := d1.AllocateManagedMemory(ctx1, 64*mem.KB)
	ptr2 := d2.AllocateManagedMemory(ctx2, 64*mem.KB)
	intakeStampedFault(t, d1, ctx1.pid, 1, uint64(ptr1), 1)
	intakeStampedFault(t, d2, ctx2.pid, 1, uint64(ptr2), 9)
	txA := mw1.queue[0]
	txB := mw2.queue[0]

	wantStamp := uvm.SameModeStamp{
		KernelLaunchOrdinal: 3,
		SourceBuildOrdinal:  0,
		SourceLocalSequence: 1,
	}
	if txA.Stamp != wantStamp {
		t.Fatalf("stamp = %+v, want %+v", txA.Stamp, wantStamp)
	}
	if txA.Stamp == txB.Stamp {
		t.Fatal("the sourceLocalSequence tie-break must differ")
	}
	if txA.SemanticKey != txB.SemanticKey {
		t.Fatalf("semantic keys differ: %+v vs %+v",
			txA.SemanticKey, txB.SemanticKey)
	}
	if txA.SemanticKey.OriginKind != uvm.OriginFaultService ||
		txA.SemanticKey.SourceComponentStableID != "gmmu0" ||
		txA.SemanticKey.KernelLaunchOrdinal != 3 ||
		txA.SemanticKey.ProgramCommandOrdinal != 3 {
		t.Fatalf("semantic key = %+v", txA.SemanticKey)
	}
	if txA.SemanticKey.RegionBase != 0 {
		t.Fatalf("region base = %#x, want 0 (the 64 KB region of the fault)",
			txA.SemanticKey.RegionBase)
	}
}

// TestUVMIdealDMAAccounting proves ideal mode zeroes the UVM fault latency
// while the logical migration bytes are still counted identically.
func TestUVMIdealDMAAccounting(t *testing.T) {
	run := func(ideal bool) (*Driver, uint64, sim.VTimeInSec) {
		d, mw := buildCoordinatorDriver(t, ideal)
		ctx := d.Init()
		pid := ctx.pid
		ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
		intakeStampedFault(t, d, pid, 1, uint64(ptr), 1)
		tx := mw.queue[0]

		if !mw.Tick() {
			t.Fatal("tick did not start the FIFO head")
		}
		latency := tx.latencyEvent.Time()
		mw.Handle(tx.latencyEvent)
		mw.Tick()
		reqs := drainRequests(d)
		var bytes uint64
		for _, r := range reqs {
			if h2d, ok := r.(*protocol.MemCopyH2DReq); ok {
				bytes += uint64(len(h2d.SrcBuffer))
			}
		}
		if bytes == 0 {
			t.Fatal("the migration must issue H2D DMA")
		}
		for _, r := range reqs {
			if h2d, ok := r.(*protocol.MemCopyH2DReq); ok {
				deliverGeneralRsp(t, d, h2d)
			}
		}
		mw.Tick()
		reqs = drainRequests(d)
		// sbin_codex (todo 25): §21.2 — the AC-off fault migration is
		// INVALID -> GPU_LOCAL, which needs no TLB invalidation; the replay
		// follows the DMA directly.
		// var tlbReq *protocol.UVMTLBInvalidateReq
		// for _, r := range reqs {
		// 	if tlb, ok := r.(*protocol.UVMTLBInvalidateReq); ok {
		// 		tlbReq = tlb
		// 	}
		// }
		// if tlbReq == nil {
		// 	t.Fatal("post-DMA requests must include the TLB invalidate")
		// }
		// deliverTLBAck(t, d, tlbReq)
		// mw.Tick()
		// reqs = drainRequests(d)
		var replayReq *protocol.UVMFaultReplayReq
		for _, r := range reqs {
			if replay, ok := r.(*protocol.UVMFaultReplayReq); ok {
				replayReq = replay
			}
		}
		if replayReq == nil {
			t.Fatal("post-DMA requests must include the replay")
		}
		deliverReplayAck(t, d, replayReq)
		mw.Tick()
		d.Tick()
		return d, bytes, latency
	}

	normal, normalBytes, normalLatency := run(false)
	ideal, idealBytes, idealLatency := run(true)

	if normalBytes != idealBytes {
		t.Fatalf("logical migration bytes = %d (normal) vs %d (ideal)",
			normalBytes, idealBytes)
	}
	if normalBytes != 64*mem.KB {
		t.Fatalf("logical migration bytes = %d, want 64KB", normalBytes)
	}
	if normalLatency <= 0 {
		t.Fatalf("normal fault latency = %v, want > 0", normalLatency)
	}
	if idealLatency != 0 {
		t.Fatalf("ideal fault latency = %v, want 0", idealLatency)
	}
	if normal.uvmCoordinator.TotalBytes() != ideal.uvmCoordinator.TotalBytes() {
		t.Fatalf("coordinator bytes = %d (normal) vs %d (ideal)",
			normal.uvmCoordinator.TotalBytes(),
			ideal.uvmCoordinator.TotalBytes())
	}
	if ideal.uvmCoordinator.TotalLatency() != 0 {
		t.Fatalf("ideal coordinator latency = %v, want 0",
			ideal.uvmCoordinator.TotalLatency())
	}
}