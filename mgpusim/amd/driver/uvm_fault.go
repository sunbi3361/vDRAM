package driver

import (
	"time"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: FIFO fault service, 64 KB coalescing, and scheduled software
// latency (plan todo 15 of mgpusim-uvm-manager, uvm-manager.md §8.3, §8.4,
// §10). The driver consumes raw 4 KB fault requests, coalesces them per
// (PID, GPU, aligned 64 KB region), enqueues one unique fault-service
// transaction per region in FIFO order, and services exactly one transaction
// at a time: charge the configured software latency as a scheduled event,
// re-read residency from the current masks, migrate only the missing pages
// (zero DMA/PTE/TLB work when the demand is already local), and complete the
// transaction only after the GMMU replay ack.

// faultWaiter is one raw 4 KB fault request coalesced into a transaction.
// sbin_codex
type faultWaiter struct {
	VAddr             uint64
	FaultPendingToken vm.FaultPendingToken
}

// faultTransaction is one unique 64 KB fault-service transaction. It is
// created at the first raw fault of its region (the unique count is charged
// there), waits FIFO, and is serviced when it reaches the head. // sbin_codex
type faultTransaction struct {
	Ticket      uint64
	PID         vm.PID
	GPU         int
	RegionBase  uint64
	Key         copyRegionKey
	DemandPages []uint64 // allocation page indices of the 64 KB demand region
	Waiters     []*faultWaiter
	ReplayToken vm.ReplayToken

	reg *ManagedAllocationRegistration // the owning registration (stable)

	phase        faultPhase
	claimed      bool
	latencyDone  bool
	latencyEvent *faultLatencyEvent

	missingPages []uint64 // pages this transaction migrates
	// sbin_codex (todo 16): the maximal-run migration plan of this
	// transaction (reservation + destination frames + runs).
	plan       *migrationPlan
	dmaReqs    []sim.Msg
	pendingDMA int
	tlbReqs    []*protocol.UVMTLBInvalidateReq
	pendingTLB int
	replayReq  *protocol.UVMFaultReplayReq
}

// faultMigrationPage is one page a fault service transfers. // sbin_codex
type faultMigrationPage struct {
	PageVA  uint64
	CPUPage uint64
	GPUPage uint64
}

// faultMigratedPage is one page a fault service published as GPU-resident.
// sbin_codex
type faultMigratedPage struct {
	PageVA  uint64
	GPUPage uint64
}

// faultLatencyEvent fires when a transaction's software latency completes.
// sbin_codex
type faultLatencyEvent struct {
	sim.EventBase
	tx *faultTransaction
}

// faultServiceMiddleware owns the FIFO fault queue, the single active
// transaction, and the scheduled-latency service. It is wired into the
// driver's middleware list before the copy middleware so it consumes its own
// DMA/replay responses first. // sbin_codex
type faultServiceMiddleware struct {
	driver *Driver

	queue               []*faultTransaction
	active              *faultTransaction
	latencyChargedCount int

	// sbin_codex (todo 16): test hook — when > 0, the next startMigration
	// emits only failAfterRuns runs' requests and then rolls the migration
	// back (the injected-failure rollback contract).
	failAfterRuns int
}

// intake consumes one raw PageFaultReq: it coalesces into the region's live
// transaction or creates and enqueues the region's first transaction. // sbin_codex
func (m *faultServiceMiddleware) intake(req *protocol.PageFaultReq) bool {
	tx, isNew, err := m.driver.uvm.intakePageFault(req.PID, req.GPU, req.VAddr)
	if err != nil {
		panic(err)
	}
	tx.Waiters = append(tx.Waiters, &faultWaiter{
		VAddr:             req.VAddr,
		FaultPendingToken: req.FaultPendingToken,
	})
	if isNew {
		m.queue = append(m.queue, tx)
	}
	return true
}

// ProcessCommand reports that the fault service handles no commands. // sbin_codex
func (m *faultServiceMiddleware) ProcessCommand(
	cmd Command,
	queue *CommandQueue,
) bool {
	return false
}

// Tick consumes the service responses and drives the FIFO queue. // sbin_codex
func (m *faultServiceMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.processIncoming() || madeProgress
	madeProgress = m.startNext() || madeProgress
	madeProgress = m.driveActive() || madeProgress
	return madeProgress
}

// Handle fires a scheduled fault-latency event: the transaction's latency is
// done and the driver is kicked to service it. // sbin_codex
func (m *faultServiceMiddleware) Handle(e sim.Event) error {
	switch evt := e.(type) {
	case *faultLatencyEvent:
		if evt.tx.phase == faultPhaseLatency {
			evt.tx.latencyDone = true
		}
		m.driver.TickLater()
	}
	return nil
}

// startNext promotes the FIFO head to active when no transaction is active
// and charges its one configured software latency. // sbin_codex
func (m *faultServiceMiddleware) startNext() bool {
	if m.active != nil || len(m.queue) == 0 {
		return false
	}
	tx := m.queue[0]
	m.queue = m.queue[1:]
	m.active = tx
	m.chargeLatency(tx)
	return true
}

// chargeLatency schedules the transaction's one software-latency event. The
// unique count was charged at creation; the latency is charged once here. // sbin_codex
func (m *faultServiceMiddleware) chargeLatency(tx *faultTransaction) {
	tx.phase = faultPhaseLatency
	latency := m.driver.uvm.config.FaultHandlingLatency
	if m.driver.uvm.config.Ideal {
		latency = 0
	}
	now := m.driver.Engine.CurrentTime()
	evt := &faultLatencyEvent{tx: tx}
	evt.EventBase = *sim.NewEventBase(
		now+sim.VTimeInSec(float64(latency)/float64(time.Second)), m)
	tx.latencyEvent = evt
	m.driver.Engine.Schedule(evt)
	m.latencyChargedCount++
}

// driveActive advances the active transaction: after the latency event it
// claims the ownership slot and runs the service; a busy slot (e.g. a copy)
// is retried on a later tick. // sbin_codex
func (m *faultServiceMiddleware) driveActive() bool {
	tx := m.active
	if tx == nil {
		return false
	}
	switch tx.phase {
	case faultPhaseLatency:
		if !tx.latencyDone {
			return false
		}
		// sbin_codex (todo 16): a claimed transaction (e.g. after a rollback
		// retry) does not re-acquire its own ownership slot.
		if !tx.claimed {
			if !m.driver.uvm.AcquireOwnership(tx.Key, OwnershipFault, tx.Ticket) {
				tx.phase = faultPhaseClaiming
				return false
			}
			tx.claimed = true
		}
		m.service(tx)
		return true
	case faultPhaseClaiming:
		if !tx.claimed {
			if !m.driver.uvm.AcquireOwnership(tx.Key, OwnershipFault, tx.Ticket) {
				return false
			}
			tx.claimed = true
		}
		m.service(tx)
		return true
	}
	return false
}

// service re-reads the demand residency from the current masks: a fully
// local demand issues zero DMA/PTE/TLB work and replays; a partial demand
// recomputes the missing pages and migrates only those. // sbin_codex
func (m *faultServiceMiddleware) service(tx *faultTransaction) {
	missing := m.driver.uvm.missingDemandPages(tx)
	if len(missing) == 0 {
		m.startReplay(tx)
		return
	}
	m.startMigration(tx, missing)
}

// startMigration transfers only the missing pages: the manager reserves the
// admission, allocates the destination GPU frames, marks the pages in flight,
// and forms the maximal runs; the service emits ONE MemCopyH2DReq per run
// (uvm-manager.md §23.1.2). // sbin_codex
func (m *faultServiceMiddleware) startMigration(
	tx *faultTransaction,
	missing []uint64,
) {
	plan, err := m.driver.uvm.prepareFaultMigration(tx, missing)
	if err != nil {
		panic(err)
	}
	tx.missingPages = missing
	tx.plan = plan
	tx.phase = faultPhaseMigrating
	emitted := 0
	for _, run := range plan.Runs {
		// sbin_codex (todo 16): test hook — an injected failure after the
		// first runs rolls the migration back (rollback contract).
		if m.failAfterRuns > 0 && emitted >= m.failAfterRuns {
			m.failAfterRuns = 0
			m.rollbackMigration(tx)
			return
		}
		data, err := m.driver.globalStorage.Read(run.SrcStart, run.Bytes)
		if err != nil {
			panic(err)
		}
		req := protocol.NewMemCopyH2DReq(
			m.driver.gpuPort, m.driver.GPUs[tx.GPU-1], data, run.DstStart)
		tx.dmaReqs = append(tx.dmaReqs, req)
		tx.pendingDMA++
		m.driver.requestsToSend = append(m.driver.requestsToSend, req)
		emitted++
	}
	if err := m.driver.uvm.beginFaultMigration(
		tx, plan.TotalBytes, m.driver.Engine.CurrentTime()); err != nil {
		panic(err)
	}
}

// rollbackMigration releases the reservation and frames of a failed migration
// exactly once and clears the in-flight marks; the transaction retries its
// service on the next tick. // sbin_codex
func (m *faultServiceMiddleware) rollbackMigration(tx *faultTransaction) {
	m.driver.uvm.rollbackFaultMigration(tx, tx.plan)
	tx.plan = nil
	tx.dmaReqs = nil
	tx.pendingDMA = 0
	tx.phase = faultPhaseLatency
	tx.latencyDone = true
}

// completeMigration publishes residency and GPU PTEs for the migrated pages,
// commits the admission, and starts the 64 KB TLB invalidation. // sbin_codex
func (m *faultServiceMiddleware) completeMigration(tx *faultTransaction) {
	pages, err := m.driver.uvm.commitFaultMigration(tx)
	if err != nil {
		panic(err)
	}
	for _, p := range pages {
		m.driver.memAllocator.UpdatePage(vm.Page{
			PID:      tx.PID,
			VAddr:    p.PageVA,
			PAddr:    p.GPUPage,
			PageSize: basePageSize,
			Valid:    true,
			Managed:  true,
			Location: vm.MemoryLocationGPU_LOCAL,
		})
	}
	if err := m.driver.uvm.completeFaultMigration(
		tx, uint64(len(tx.missingPages))*basePageSize,
		m.driver.Engine.CurrentTime()); err != nil {
		panic(err)
	}
	m.startTLBI(tx)
}

// startTLBI issues the one coordinated 64 KB TLB invalidation for the
// REMOTE -> GPU_LOCAL transition. // sbin_codex
func (m *faultServiceMiddleware) startTLBI(tx *faultTransaction) {
	tx.phase = faultPhaseTLBI
	req := protocol.UVMTLBInvalidateReqBuilder{}.
		WithSrc(m.driver.gpuPort.AsRemote()).
		WithDst(m.driver.GPUs[tx.GPU-1].AsRemote()).
		WithPID(tx.PID).
		WithStartVA(tx.RegionBase).
		WithSize(subblockSizeBytes).
		Build()
	tx.tlbReqs = append(tx.tlbReqs, req)
	tx.pendingTLB++
	m.driver.requestsToSend = append(m.driver.requestsToSend, req)
}

// startReplay issues the one GMMU replay for the serviced region; the
// transaction completes only after the replay ack. // sbin_codex
func (m *faultServiceMiddleware) startReplay(tx *faultTransaction) {
	tx.phase = faultPhaseReplaying
	req := protocol.UVMFaultReplayReqBuilder{}.
		WithSrc(m.driver.gpuPort.AsRemote()).
		WithDst(m.driver.GPUs[tx.GPU-1].AsRemote()).
		WithPID(tx.PID).
		WithGPU(tx.GPU).
		WithStartVA(tx.RegionBase).
		WithSize(subblockSizeBytes).
		WithReplayToken(tx.ReplayToken).
		Build()
	tx.replayReq = req
	m.driver.requestsToSend = append(m.driver.requestsToSend, req)
}

// completeFault retires the transaction after the replay ack: the coalescing
// entry and ownership slot are released and the next FIFO transaction may
// start. // sbin_codex
func (m *faultServiceMiddleware) completeFault(tx *faultTransaction) {
	m.driver.uvm.completeFault(tx)
	tx.phase = faultPhaseDone
	m.active = nil
}

// processIncoming consumes the responses owned by the active transaction and
// rejects stray completions without double accounting. // sbin_codex
func (m *faultServiceMiddleware) processIncoming() bool {
	madeProgress := false
	for {
		req := m.driver.gpuPort.PeekIncoming()
		if req == nil {
			break
		}
		switch req := req.(type) {
		case *sim.GeneralRsp:
			if m.processGeneralRsp(req) {
				m.driver.gpuPort.RetrieveIncoming()
				madeProgress = true
				continue
			}
		case *protocol.UVMTLBInvalidateRsp:
			m.driver.gpuPort.RetrieveIncoming()
			m.processTLBRsp(req)
			madeProgress = true
			continue
		case *protocol.UVMFaultReplayRsp:
			m.driver.gpuPort.RetrieveIncoming()
			m.processReplayRsp(req)
			madeProgress = true
			continue
		}
		break
	}
	return madeProgress
}

// processGeneralRsp matches a DMA completion to the active transaction. // sbin_codex
func (m *faultServiceMiddleware) processGeneralRsp(rsp *sim.GeneralRsp) bool {
	originalReq, ok := rsp.OriginalReq.(*protocol.MemCopyH2DReq)
	if !ok {
		return false
	}
	tx := m.active
	if tx == nil || tx.phase != faultPhaseMigrating {
		return false
	}
	for i, r := range tx.dmaReqs {
		if r == originalReq {
			tx.dmaReqs = append(tx.dmaReqs[:i], tx.dmaReqs[i+1:]...)
			tx.pendingDMA--
			if tx.pendingDMA == 0 {
				m.completeMigration(tx)
			}
			return true
		}
	}
	return false
}

// processTLBRsp completes the TLB-invalidation phase; a stray ack is
// rejected. // sbin_codex
func (m *faultServiceMiddleware) processTLBRsp(rsp *protocol.UVMTLBInvalidateRsp) {
	tx := m.active
	if tx == nil || tx.phase != faultPhaseTLBI {
		return
	}
	for i, req := range tx.tlbReqs {
		if req.ID == rsp.RspTo {
			tx.tlbReqs = append(tx.tlbReqs[:i], tx.tlbReqs[i+1:]...)
			tx.pendingTLB--
			if tx.pendingTLB == 0 {
				m.startReplay(tx)
			}
			return
		}
	}
}

// processReplayRsp completes the transaction after the exactly-one replay
// ack; a duplicate or stray ack is rejected without double accounting. // sbin_codex
func (m *faultServiceMiddleware) processReplayRsp(rsp *protocol.UVMFaultReplayRsp) {
	tx := m.active
	if tx == nil || tx.replayReq == nil || rsp.RspTo != tx.replayReq.ID {
		return
	}
	m.completeFault(tx)
}
