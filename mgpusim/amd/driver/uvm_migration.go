package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: Access-Counter and remote-write migration transactions (plan
// todo 18 of mgpusim-uvm-manager, uvm-manager.md §15, §16, §21.3). A threshold
// AccessCounterNotification or a remote-write trigger becomes one migration
// transaction for its 64 KB region: the transaction claims the shared
// ownership slot, issues a commandID BlockRange and waits the Todo-8 watermark
// completion (only <=watermark old-remote reads committed before each gate's
// ack may finish; >watermark accesses retain), transfers the region's missing
// pages H2D through the existing DMA engine, publishes the PTEs
// REMOTE -> GPU_LOCAL, issues the mandatory 64 KB TLB invalidate (§21.2), marks
// the region GPU_RESIDENT (recency updated on admission only, §31.2), replays
// the blocked accesses, and unblocks. A notification while the region is in
// FAULT_PENDING / MIGRATING_TO_GPU (the driver-side states covering §16's
// FAULT_HANDLING / PREFETCHING_TO_GPU) is ignored — no additional transaction,
// no duplicate DMA. The parked remote write is never committed to host (§15).

// migrationTrigger classifies why a migration transaction was created.
type migrationTrigger int

const (
	// migrationTriggerAccessCounter is a threshold AccessCounterNotification.
	migrationTriggerAccessCounter migrationTrigger = iota
	// migrationTriggerRemoteWrite is a GPU write to a CPU_REMOTE mapping.
	migrationTriggerRemoteWrite
)

// migrationPhase is the progress of one AC/write migration transaction.
type migrationPhase int

const (
	// migrationPhaseClaiming waits for its ownership slot (e.g. a copy).
	migrationPhaseClaiming migrationPhase = iota
	// migrationPhaseBlocking waits for the BlockRange watermark completion.
	migrationPhaseBlocking
	// migrationPhaseMigrating transfers the region's missing pages H2D.
	migrationPhaseMigrating
	// migrationPhaseTLBI waits for the mandatory 64 KB TLB invalidation.
	migrationPhaseTLBI
	// migrationPhaseReplaying waits for the GMMU replay ack.
	migrationPhaseReplaying
	// migrationPhaseUnblocking waits for the UnblockRange completion.
	migrationPhaseUnblocking
	// migrationPhaseDone retired after the unblock; the next may start.
	migrationPhaseDone
)

// migrationTransaction is one AC/write migration of one 64 KB region. It is
// created at the first notification/write trigger of the region and retires
// after the unblock completion; a later trigger for the same region is
// suppressed (§16). // sbin_codex
type migrationTransaction struct {
	Ticket     uint64 // commandID (global ticket)
	PID        vm.PID
	GPU        int // 1-based GPU ID
	RegionBase uint64
	Key        copyRegionKey
	Trigger    migrationTrigger
	reg        *ManagedAllocationRegistration // the owning registration (stable)

	DemandPages []uint64 // allocation page indices of the 64 KB region
	ReplayToken vm.ReplayToken

	phase   migrationPhase
	claimed bool

	blockReqs   []*vm.BlockRange
	unblockReqs []*vm.UnblockRange
	plan        *migrationPlan
	dmaReqs     []sim.Msg
	tlbReqs     []*protocol.UVMTLBInvalidateReq
	replayReq   *protocol.UVMFaultReplayReq

	pendingBlocks   int
	pendingDMA      int
	pendingTLB      int
	pendingUnblocks int
}

// migrationMiddleware drives AC/write migration transactions. It is wired
// into the driver's middleware list after the fault service so each consumes
// only its own responses. // sbin_codex
type migrationMiddleware struct {
	driver  *Driver
	active  *migrationTransaction
	pending []*migrationTransaction
}

// intakeNotification consumes one threshold AccessCounterNotification from
// the CP and turns it into a migration transaction. // sbin_codex
func (m *migrationMiddleware) intakeNotification(
	notif *protocol.AccessCounterNotification,
) bool {
	return m.intake(notif.PID, notif.GPU, notif.VAddr,
		migrationTriggerAccessCounter)
}

// intakeRemoteWrite consumes a remote-write migration trigger (the GPU-side
// parked write; the driver policy migrates the region before the write may
// complete against GPU-local memory, §15). // sbin_codex
func (m *migrationMiddleware) intakeRemoteWrite(
	pid vm.PID,
	gpu int,
	vaddr uint64,
) bool {
	return m.intake(pid, gpu, vaddr, migrationTriggerRemoteWrite)
}

// intake creates the region's migration transaction; a suppressed trigger
// (region already being brought to the GPU, or a transaction already in
// flight) is consumed without creating anything (§16). // sbin_codex
func (m *migrationMiddleware) intake(
	pid vm.PID,
	gpu int,
	vaddr uint64,
	trigger migrationTrigger,
) bool {
	tx, err := m.driver.uvm.intakeMigration(
		pid, gpu, vaddr, trigger, m.driver.Engine.CurrentTime())
	if err != nil {
		panic(err)
	}
	if tx == nil {
		return true // suppressed / coalesced
	}
	if m.active == nil {
		m.active = tx
	} else {
		m.pending = append(m.pending, tx)
	}
	return true
}

// ProcessCommand reports that the migration service handles no commands.
// sbin_codex
func (m *migrationMiddleware) ProcessCommand(
	cmd Command,
	queue *CommandQueue,
) bool {
	return false
}

// Tick consumes the transaction's responses and drives the active
// transaction. // sbin_codex
func (m *migrationMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.processIncoming() || madeProgress
	madeProgress = m.driveActive() || madeProgress
	return madeProgress
}

// driveActive claims the ownership slot of the active transaction and issues
// the commandID block; a busy slot is retried on a later tick. // sbin_codex
func (m *migrationMiddleware) driveActive() bool {
	tx := m.active
	if tx == nil {
		if len(m.pending) == 0 {
			return false
		}
		tx = m.pending[0]
		m.pending = m.pending[1:]
		m.active = tx
	}
	if tx.phase != migrationPhaseClaiming {
		return false
	}
	if !m.driver.uvm.AcquireOwnership(tx.Key, OwnershipMigration, tx.Ticket) {
		return false
	}
	tx.claimed = true
	m.startBlock(tx)
	return true
}

// startBlock issues the one commandID BlockRange for the 64 KB region; the
// transaction waits the Todo-8 watermark completion before any H2D. // sbin_codex
func (m *migrationMiddleware) startBlock(tx *migrationTransaction) {
	tx.phase = migrationPhaseBlocking
	req := &vm.BlockRange{
		CommandID: tx.Ticket,
		PID:       tx.PID,
		StartVA:   tx.RegionBase,
		Size:      subblockSizeBytes,
	}
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = m.driver.gpuPort.AsRemote()
	req.Dst = m.driver.GPUs[tx.GPU-1].AsRemote()
	tx.blockReqs = append(tx.blockReqs, req)
	tx.pendingBlocks++
	m.driver.requestsToSend = append(m.driver.requestsToSend, req)
}

// startMigration transfers the region's missing pages H2D: the manager
// reserves the admission, allocates the destination GPU frames, marks the
// pages in flight, and forms the maximal runs; the service emits ONE
// MemCopyH2DReq per run (uvm-manager.md §23.1.2). // sbin_codex
// sbin_codex (todo 20): the pre-eviction victims launched by the admission
// gate are handed to the eviction service so the D2H runs concurrently with
// this H2D.
func (m *migrationMiddleware) startMigration(tx *migrationTransaction) {
	missing := m.driver.uvm.missingPages(tx.reg, tx.DemandPages)
	plan, err := m.driver.uvm.prepareMigrationPages(tx.reg, tx.GPU, missing)
	if err != nil {
		// sbin_codex (todo 20): drive any pre-eviction victims launched by
		// the gate before the (unchanged) hard-shortage panic.
		if plan != nil {
			for _, v := range plan.PreEvictions {
				m.driver.uvmEviction.intakePreEviction(v)
			}
		}
		panic(err)
	}
	for _, v := range plan.PreEvictions {
		m.driver.uvmEviction.intakePreEviction(v)
	}
	tx.plan = plan
	tx.phase = migrationPhaseMigrating
	for _, run := range plan.Runs {
		data, err := m.driver.globalStorage.Read(run.SrcStart, run.Bytes)
		if err != nil {
			panic(err)
		}
		req := protocol.NewMemCopyH2DReq(
			m.driver.gpuPort, m.driver.GPUs[tx.GPU-1], data, run.DstStart)
		tx.dmaReqs = append(tx.dmaReqs, req)
		tx.pendingDMA++
		m.driver.requestsToSend = append(m.driver.requestsToSend, req)
	}
}

// publish publishes GPU residency and the REMOTE -> GPU_LOCAL PTEs for the
// migrated pages, then issues the mandatory 64 KB TLB invalidation (§21.2:
// the previous REMOTE translation may be cached in the L2 TLB). // sbin_codex
func (m *migrationMiddleware) publish(tx *migrationTransaction) {
	pages, err := m.driver.uvm.commitMigrationPages(tx.reg, tx.plan)
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
	m.startTLBI(tx)
}

// startTLBI issues the one coordinated 64 KB TLB invalidation for the
// REMOTE -> GPU_LOCAL transition. // sbin_codex
func (m *migrationMiddleware) startTLBI(tx *migrationTransaction) {
	tx.phase = migrationPhaseTLBI
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

// markResident completes the admission after the TLB ack: the region
// transitions to GPU_RESIDENT (recency updated on admission only, §31.2) and
// the reservation commits; the blocked accesses are then replayed. // sbin_codex
func (m *migrationMiddleware) markResident(tx *migrationTransaction) {
	if err := m.driver.uvm.completeMigrationAdmission(
		tx.reg, tx.GPU, tx.RegionBase, tx.plan.TotalBytes,
		m.driver.Engine.CurrentTime()); err != nil {
		panic(err)
	}
	m.startReplay(tx)
}

// startReplay issues the one GMMU replay for the serviced region; the
// transaction unblocks only after the replay ack. // sbin_codex
func (m *migrationMiddleware) startReplay(tx *migrationTransaction) {
	tx.phase = migrationPhaseReplaying
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

// startUnblock releases the ownership slot (no wake) and issues the
// UnblockRange so the retained accesses may resume. // sbin_codex
func (m *migrationMiddleware) startUnblock(tx *migrationTransaction) {
	m.driver.uvm.ReleaseOwnership(tx.Key, tx.Ticket)
	tx.phase = migrationPhaseUnblocking
	req := &vm.UnblockRange{
		CommandID: tx.Ticket,
		PID:       tx.PID,
		StartVA:   tx.RegionBase,
		Size:      subblockSizeBytes,
	}
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = m.driver.gpuPort.AsRemote()
	req.Dst = m.driver.GPUs[tx.GPU-1].AsRemote()
	tx.unblockReqs = append(tx.unblockReqs, req)
	tx.pendingUnblocks++
	m.driver.requestsToSend = append(m.driver.requestsToSend, req)
}

// finish retires the transaction after the unblock completion: the
// coalescing entry is removed and the ticket queue is woken. // sbin_codex
func (m *migrationMiddleware) finish(tx *migrationTransaction) {
	m.driver.uvm.completeMigration(tx)
	tx.phase = migrationPhaseDone
	m.active = nil
}

// processIncoming consumes the responses owned by the active transaction and
// leaves every other message for the next middleware. // sbin_codex
func (m *migrationMiddleware) processIncoming() bool {
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
			if m.processTLBRsp(req) {
				m.driver.gpuPort.RetrieveIncoming()
				madeProgress = true
				continue
			}
		case *protocol.UVMFaultReplayRsp:
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

// processGeneralRsp matches a completion to the active transaction's
// block/unblock/DMA requests; a stray completion is rejected. // sbin_codex
func (m *migrationMiddleware) processGeneralRsp(rsp *sim.GeneralRsp) bool {
	tx := m.active
	if tx == nil {
		return false
	}
	switch originalReq := rsp.OriginalReq.(type) {
	case *vm.BlockRange:
		for i, r := range tx.blockReqs {
			if r == originalReq {
				tx.blockReqs = append(tx.blockReqs[:i], tx.blockReqs[i+1:]...)
				tx.pendingBlocks--
				if tx.pendingBlocks == 0 {
					m.startMigration(tx)
				}
				return true
			}
		}
	case *vm.UnblockRange:
		for i, r := range tx.unblockReqs {
			if r == originalReq {
				tx.unblockReqs = append(tx.unblockReqs[:i], tx.unblockReqs[i+1:]...)
				tx.pendingUnblocks--
				if tx.pendingUnblocks == 0 {
					m.finish(tx)
				}
				return true
			}
		}
	case *protocol.MemCopyH2DReq:
		for i, r := range tx.dmaReqs {
			if r == originalReq {
				tx.dmaReqs = append(tx.dmaReqs[:i], tx.dmaReqs[i+1:]...)
				tx.pendingDMA--
				if tx.pendingDMA == 0 {
					m.publish(tx)
				}
				return true
			}
		}
	}
	return false
}

// processTLBRsp completes the TLB-invalidation phase; a stray ack is
// rejected. // sbin_codex
func (m *migrationMiddleware) processTLBRsp(
	rsp *protocol.UVMTLBInvalidateRsp,
) bool {
	tx := m.active
	if tx == nil {
		return false
	}
	for i, req := range tx.tlbReqs {
		if req.ID == rsp.RspTo {
			tx.tlbReqs = append(tx.tlbReqs[:i], tx.tlbReqs[i+1:]...)
			tx.pendingTLB--
			if tx.pendingTLB == 0 {
				m.markResident(tx)
			}
			return true
		}
	}
	return false
}

// processReplayRsp unblocks after the exactly-one replay ack; a duplicate or
// stray ack is rejected without double accounting. // sbin_codex
func (m *migrationMiddleware) processReplayRsp(
	rsp *protocol.UVMFaultReplayRsp,
) bool {
	tx := m.active
	if tx == nil || tx.replayReq == nil || rsp.RspTo != tx.replayReq.ID {
		return false
	}
	m.startUnblock(tx)
	return true
}
