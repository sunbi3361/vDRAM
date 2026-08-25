package driver

import (
	"time"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	// sbin_codex (todo 21): the UVM coordinator identity types.
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
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

	// sbin_codex (todo 21): the coordinator identity of the transaction —
	// the same-mode stamp and the semantic root key (from the routed
	// envelope), and the enqueued coordinator root.
	Stamp       uvm.SameModeStamp
	SemanticKey uvm.SemanticRootKey
	root        *uvm.Root

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

	// sbin_codex (todo 17): prefetchPages are the TBN-selected prefetch pages
	// of this transaction's migration (uvm-manager.md §11.8), set by
	// recomputeTBN at service time. Their GPU residency carries the
	// prefetched-provenance mark (§11.11).
	prefetchPages []uint64
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
// transaction or creates and enqueues the region's first transaction. A new
// transaction is stamped with the coordinator identity from the routed
// envelope and enqueued as a delivered root (todo 21). // sbin_codex
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
		m.enqueueFaultRoot(tx, req)
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
	// sbin_codex (todo 22): the one update point of
	// fault_service_latency_total (§27); ideal mode charges zero so the
	// ideal latency rows are zero.
	m.driver.uvm.recordFaultServiceLatency(sim.VTimeInSec(float64(latency) / float64(time.Second)))
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
	case faultPhaseWaitingCapacity:
		// sbin_codex (todo 20): retry the admission — in-flight pre-evictions
		// free capacity and frames; the retry re-runs the projected-occupancy
		// gate with the same missing pages.
		// sbin_codex (todo 25): a failed retry reports no progress so the
		// driver stops busy-waiting while the admission waits for capacity
		// (the eviction's completions re-trigger the retry); the engine can
		// then reach quiescence between stimuli.
		m.driver.uvm.recordCapacityWait()
		m.startMigration(tx, tx.missingPages)
		return tx.phase == faultPhaseMigrating
	}
	return false
}

// service re-reads the demand residency from the current masks: a fully
// local demand issues zero DMA/PTE/TLB work and replays; a partial demand
// recomputes TBN from the current masks and migrates only the missing pages
// plus the TBN-selected prefetch pages. // sbin_codex
// sbin_codex (todo 17): the migration set now comes from recomputeTBN, which
// recomputes the NVIDIA-style TBN selection over the current occupancy masks
// (uvm-manager.md §11): the missing demand pages plus the actual prefetch
// pages of the selected region, with resident/in-flight duplicates
// suppressed.
//
//	func (m *faultServiceMiddleware) service(tx *faultTransaction) {
//		missing := m.driver.uvm.missingDemandPages(tx)
//		if len(missing) == 0 {
//			m.startReplay(tx)
//			return
//		}
//		m.startMigration(tx, missing)
//	}
func (m *faultServiceMiddleware) service(tx *faultTransaction) {
	missing := m.driver.uvm.recomputeTBN(tx)
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
// sbin_codex (todo 20): a hard capacity/frame shortage queues the admission
// (faultPhaseWaitingCapacity) instead of panicking; the pre-eviction victims
// launched by the admission gate are handed to the eviction service either
// way, so the H2D and the D2H run concurrently.
func (m *faultServiceMiddleware) startMigration(
	tx *faultTransaction,
	missing []uint64,
) {
	tx.missingPages = missing
	plan, err := m.driver.uvm.prepareFaultMigration(tx, missing)
	if err != nil {
		// sbin_codex (todo 20): drive the pre-eviction victims (they free
		// capacity/frames) and queue the admission for the retry.
		if plan != nil {
			for _, v := range plan.PreEvictions {
				m.driver.uvmEviction.intakePreEviction(v)
			}
		}
		m.driver.uvm.recordCapacityWait()
		tx.plan = nil
		tx.phase = faultPhaseWaitingCapacity
		return
	}
	for _, v := range plan.PreEvictions {
		m.driver.uvmEviction.intakePreEviction(v)
	}
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
// sbin_codex (todo 17): after the fault region's admission completion, the
// TBN-prefetched regions touched by the migration publish GPU_RESIDENT
// (completePrefetchRegions) and the prefetched pages carry the
// prefetched-provenance mark (markPrefetched, §11.11).
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
	// sbin_codex (todo 17): TBN-prefetched region completion and provenance
	// marking.
	m.driver.uvm.markPrefetched(tx)
	if err := m.driver.uvm.completePrefetchRegions(
		tx, m.driver.Engine.CurrentTime()); err != nil {
		panic(err)
	}
	// sbin_codex (todo 22): the one update points of the §27 H2D migration
	// counters — num_cpu_to_gpu_migrations / bytes_cpu_to_gpu and the
	// demand/prefetch breakdown (num_demand_migrations / bytes_demand_migrated
	// / num_prefetch_migrations / bytes_prefetched) — and of
	// num_local_pte_installs (the GPU_LOCAL PTE publications). The demand
	// bytes are the migration set minus the TBN prefetch component
	// (tx.prefetchPages).
	m.driver.uvm.recordCPUToGPUMigration(tx.plan.TotalBytes)
	prefetchBytes := uint64(len(tx.prefetchPages)) * basePageSize
	m.driver.uvm.recordDemandMigration(tx.plan.TotalBytes - prefetchBytes)
	if prefetchBytes > 0 {
		m.driver.uvm.recordPrefetchMigration(prefetchBytes)
	}
	m.driver.uvm.recordLocalPTEInstalls(uint64(len(pages)))
	// sbin_codex (todo 25): §21.2/§21.5 — INVALID -> GPU_LOCAL (Access
	// Counter off) requires NO TLB invalidation: invalid/non-resident
	// translations are not cached in the TLB hierarchy, so the required
	// path is DMA H2D -> PTE install -> Fault Replay. Only REMOTE ->
	// GPU_LOCAL (Access Counter on) invalidates the cached REMOTE
	// translation. The Access Counter mode determines the PTE state
	// deterministically (INVALID vs CPU_REMOTE at allocation and after
	// eviction).
	// m.startTLBI(tx)
	if m.driver.uvm.config.AccessCounter {
		m.startTLBI(tx)
		return
	}
	m.startReplay(tx)
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
	// sbin_codex (todo 22): the one update point of
	// num_uvm_tlb_range_invalidations (§27).
	m.driver.uvm.recordUVMTLBRangeInvalidation()
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
// entry and ownership slot are released, the completion is reported to the
// coordinator, and the next FIFO transaction may start. // sbin_codex
func (m *faultServiceMiddleware) completeFault(tx *faultTransaction) {
	m.driver.uvm.completeFault(tx)
	m.reportFaultRoot(tx)
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
			// sbin_codex (todo 18): consume only the fault transaction's own
			// TLB ack; a concurrent migration transaction's ack is left for
			// the migration middleware.
			if m.processTLBRsp(req) {
				m.driver.gpuPort.RetrieveIncoming()
				madeProgress = true
				continue
			}
		case *protocol.UVMFaultReplayRsp:
			// sbin_codex (todo 18): consume only the fault transaction's own
			// replay ack; a concurrent migration transaction's ack is left
			// for the migration middleware.
			if m.processReplayRsp(req) {
				m.driver.gpuPort.RetrieveIncoming()
				madeProgress = true
				continue
			}
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

// processTLBRsp completes the TLB-invalidation phase. It returns whether the
// ack belonged to the active transaction; a stray ack (e.g. another
// middleware's transaction) is rejected and left in the port. // sbin_codex
func (m *faultServiceMiddleware) processTLBRsp(
	rsp *protocol.UVMTLBInvalidateRsp,
) bool {
	tx := m.active
	if tx == nil || tx.phase != faultPhaseTLBI {
		return false
	}
	for i, req := range tx.tlbReqs {
		if req.ID == rsp.RspTo {
			tx.tlbReqs = append(tx.tlbReqs[:i], tx.tlbReqs[i+1:]...)
			tx.pendingTLB--
			if tx.pendingTLB == 0 {
				m.startReplay(tx)
			}
			return true
		}
	}
	return false
}

// processReplayRsp completes the transaction after the exactly-one replay
// ack. It returns whether the ack belonged to the active transaction; a
// duplicate or stray ack is rejected without double accounting. // sbin_codex
func (m *faultServiceMiddleware) processReplayRsp(
	rsp *protocol.UVMFaultReplayRsp,
) bool {
	tx := m.active
	if tx == nil || tx.replayReq == nil || rsp.RspTo != tx.replayReq.ID {
		return false
	}
	m.completeFault(tx)
	return true
}
