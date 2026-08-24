package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	// sbin_codex (todo 21): the UVM coordinator identity types.
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// sbin_codex: reactive 64 KB eviction with strict cache/PTE/TLB/DMA ordering
// (plan todo 19 of mgpusim-uvm-manager, uvm-manager.md §18, §19, §21.4). The
// victim is selected deterministically by migration-recency LRU (§18.1/§31.2:
// recency updates only on migration/admission, never on hits) among eligible
// GPU-resident regions (not migrating, not pinned, not in an in-progress fault
// or migration transaction, not already EVICTING, not COPY-owned — todo 5).
// The transaction marks the region EVICTING (EVICT_PENDING + ownership slot +
// coalescing entry), issues ONE commandID BlockRange and waits each gate's
// Todo-8 local-watermark ack, then runs the §21.4 order: cache WB+INV (todo
// 13), PTE transition GPU_LOCAL -> INVALID with the generation advance, the
// topology TLB invalidate (todo 14), the D2H of every valid page (todo 16,
// §18.3: the clean D2H is never omitted), the final PTE (INVALID when the
// Access Counter is off, REMOTE when on), the GPU frame free, CPU_RESIDENT,
// the blocked-request replay, and the unblock. Unrelated execution is
// preserved; no GPU-wide restart is used.

// evictionStage is the progress of one eviction transaction.
type evictionStage int

const (
	// evictionStageClaiming waits for the drive to issue the block.
	evictionStageClaiming evictionStage = iota
	// evictionStageBlocking waits for the BlockRange watermark completion.
	evictionStageBlocking
	// evictionStageFlushing waits for the range WB+INV completion.
	evictionStageFlushing
	// evictionStageTransitioning publishes the transition PTE + generation.
	evictionStageTransitioning
	// evictionStageTLBI waits for the 64 KB TLB invalidation.
	evictionStageTLBI
	// evictionStageD2H transfers every valid page to the CPU backing.
	evictionStageD2H
	// evictionStageFinalPTE installs the final INVALID/REMOTE PTE.
	evictionStageFinalPTE
	// evictionStageFreeing returns the GPU frames and marks CPU_RESIDENT.
	evictionStageFreeing
	// evictionStageReplaying waits for the GMMU replay ack.
	evictionStageReplaying
	// evictionStageUnblocking waits for the UnblockRange completion.
	evictionStageUnblocking
	// evictionStageDone retired after the unblock; the next may start.
	evictionStageDone
)

// evictionTransaction is one reactive 64 KB eviction of one region. It is
// created at the victim selection and retires after the unblock completion; a
// region is never selected twice (§18.2). // sbin_codex
type evictionTransaction struct {
	Ticket     uint64 // commandID (global ticket)
	PID        vm.PID
	GPU        int // 1-based GPU ID
	RegionBase uint64
	Key        copyRegionKey
	reg        *ManagedAllocationRegistration // the owning registration (stable)

	// sbin_codex (todo 21): the coordinator identity of the transaction.
	Stamp       uvm.SameModeStamp
	SemanticKey uvm.SemanticRootKey
	root        *uvm.Root

	ReplayToken vm.ReplayToken

	phase   evictionStage
	claimed bool

	blockReqs   []*vm.BlockRange
	flushReqs   []*protocol.UVMCacheRangeFlushReq
	tlbReqs     []*protocol.UVMTLBInvalidateReq
	d2h         *evictionD2HTransfer
	replayReq   *protocol.UVMFaultReplayReq
	unblockReqs []*vm.UnblockRange

	pendingBlocks   int
	pendingFlushes  int
	pendingTLB      int
	pendingUnblocks int

	// migrated marks the reservation moved R -> I (StartMigration); completed
	// marks the reservation released I -> 0 (CompleteMigrationToCPU). An
	// aborted transaction restores the reservation only when it was moved but
	// not completed.
	migrated  bool
	completed bool
	bytes     uint64

	// sbin_codex (todo 20): preEviction marks a projected-occupancy
	// pre-eviction victim (uvm-manager.md §17.1) as opposed to a reactive
	// eviction; it drives the pre-eviction statistics (E term, concurrency,
	// overlap). bytes is recorded at launch so a launched-but-not-started
	// victim counts toward the projected occupancy E term.
	preEviction bool
}

// evictionMiddleware drives reactive eviction transactions. It is wired into
// the driver's middleware list after the migration service so each consumes
// only its own responses. // sbin_codex
type evictionMiddleware struct {
	driver  *Driver
	active  *evictionTransaction
	pending []*evictionTransaction

	// failAfterStage is a test hook: when set, the next transaction aborts
	// instead of entering the stage (injected stage failure).
	failAfterStage evictionStage
}

// intake selects the deterministic LRU victim of pid on gpu and starts its
// eviction transaction, stamped with the coordinator identity (todo 21). An
// error (e.g. no eligible victim) creates nothing. // sbin_codex
func (m *evictionMiddleware) intake(pid vm.PID, gpu int) error {
	tx, err := m.driver.uvm.intakeEviction(pid, gpu)
	if err != nil {
		return err
	}
	m.queue(tx)
	m.enqueueEvictionRoot(tx)
	return nil
}

// intakePreEviction queues a pre-eviction victim transaction created by the
// projected-occupancy admission gate (todo 20); it is driven by the same
// eviction transaction machinery as a reactive eviction. // sbin_codex
func (m *evictionMiddleware) intakePreEviction(tx *evictionTransaction) {
	m.queue(tx)
	m.enqueuePreEvictionRoot(tx)
}

// queue appends a transaction to the active/pending set; there is no fixed
// depth (§17.1: no UVM-side queue-depth limit). // sbin_codex
func (m *evictionMiddleware) queue(tx *evictionTransaction) {
	if m.active == nil {
		m.active = tx
	} else {
		m.pending = append(m.pending, tx)
	}
}

// ProcessCommand reports that the eviction service handles no commands.
// sbin_codex
func (m *evictionMiddleware) ProcessCommand(
	cmd Command,
	queue *CommandQueue,
) bool {
	return false
}

// Tick consumes the transaction's responses and drives the active
// transaction. // sbin_codex
func (m *evictionMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.processIncoming() || madeProgress
	madeProgress = m.driveActive() || madeProgress
	return madeProgress
}

// driveActive starts the active transaction's block; a pending transaction
// becomes active when none is. // sbin_codex
func (m *evictionMiddleware) driveActive() bool {
	tx := m.active
	if tx == nil {
		if len(m.pending) == 0 {
			return false
		}
		tx = m.pending[0]
		m.pending = m.pending[1:]
		m.active = tx
	}
	if tx.phase != evictionStageClaiming {
		return false
	}
	m.advance(tx, evictionStageBlocking)
	return true
}

// advance moves the transaction to the next stage; an injected stage failure
// aborts it instead. // sbin_codex
func (m *evictionMiddleware) advance(
	tx *evictionTransaction,
	next evictionStage,
) {
	if m.failAfterStage == next {
		m.failAfterStage = 0
		m.abort(tx)
		return
	}
	switch next {
	case evictionStageBlocking:
		m.startBlock(tx)
	case evictionStageFlushing:
		m.startFlush(tx)
	case evictionStageTransitioning:
		m.transitionPTE(tx)
	case evictionStageTLBI:
		m.startTLBI(tx)
	case evictionStageD2H:
		m.startD2H(tx)
	case evictionStageFinalPTE:
		m.finalPTE(tx)
	case evictionStageFreeing:
		m.freeFrames(tx)
	case evictionStageReplaying:
		m.startReplay(tx)
	case evictionStageUnblocking:
		m.startUnblock(tx)
	case evictionStageDone:
		m.finish(tx)
	}
}

// startBlock issues the one commandID BlockRange for the 64 KB region; the
// transaction waits the Todo-8 watermark completion before any cache/PTE/DMA
// work. // sbin_codex
func (m *evictionMiddleware) startBlock(tx *evictionTransaction) {
	tx.phase = evictionStageBlocking
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

// startFlush issues the range cache WB+INV over every valid page of the
// region (uvm-manager.md §19.1: Writeback=true, Invalidate=true, Size=64 KB).
// // sbin_codex
func (m *evictionMiddleware) startFlush(tx *evictionTransaction) {
	tx.phase = evictionStageFlushing
	req := m.buildFlushReq(tx)
	tx.flushReqs = append(tx.flushReqs, req)
	tx.pendingFlushes++
	m.driver.requestsToSend = append(m.driver.requestsToSend, req)
}

// buildFlushReq builds the WB+INV range flush: the mask marks the region's
// valid pages and the runs map them to their HBM PAs in VA order. // sbin_codex
func (m *evictionMiddleware) buildFlushReq(
	tx *evictionTransaction,
) *protocol.UVMCacheRangeFlushReq {
	reg := tx.reg
	allocStart, valid := m.regionPages(tx)
	var mask uint64
	runs := make([]cache.PhysicalRun, 0, valid)
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := m.driver.uvm.pageStateLocked(reg, page)
		// Mask bit i covers VA regionBase + i*4KB (todo 5 convention); a
		// misaligned allocation's first valid page is not region-local page 0.
		mask |= uint64(1) << ((reg.Base + page*basePageSize - tx.RegionBase) /
			basePageSize)
		if n := len(runs); n > 0 &&
			runs[n-1].Start+runs[n-1].Length == p.GPUPhysicalPage {
			runs[n-1].Length += basePageSize
		} else {
			runs = append(runs, cache.PhysicalRun{
				Start:  p.GPUPhysicalPage,
				Length: basePageSize,
			})
		}
	}
	return protocol.UVMCacheRangeFlushReqBuilder{}.
		WithSrc(m.driver.gpuPort.AsRemote()).
		WithDst(m.driver.GPUs[tx.GPU-1].AsRemote()).
		WithOperation(cache.UVMCacheRangeFlushWritebackInvalidate).
		WithPID(tx.PID).
		WithVABase(tx.RegionBase).
		WithValidPageMask(mask).
		WithPhysicalRuns(runs).
		Build()
}

// transitionPTE publishes the transition PTE (GPU_LOCAL -> INVALID) after the
// generation advance, moves the reservation R -> I, and transitions the region
// to MIGRATING_TO_CPU (§21.4 step 6). // sbin_codex
func (m *evictionMiddleware) transitionPTE(tx *evictionTransaction) {
	tx.phase = evictionStageTransitioning
	m.driver.uvm.IncrementGeneration()
	reg := tx.reg
	allocStart, valid := m.regionPages(tx)
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		m.driver.memAllocator.UpdatePage(vm.Page{
			PID:      tx.PID,
			VAddr:    reg.Base + page*basePageSize,
			PAddr:    0,
			PageSize: basePageSize,
			Valid:    false,
			Managed:  true,
			Location: vm.MemoryLocationINVALID,
		})
	}
	if err := m.driver.uvm.beginEvictionMigration(
		tx, valid*basePageSize, m.driver.Engine.CurrentTime()); err != nil {
		panic(err)
	}
	m.advance(tx, evictionStageTLBI)
}

// startTLBI issues the one coordinated 64 KB TLB invalidation for the
// GPU_LOCAL -> transition mapping (§21.4 step 7). // sbin_codex
func (m *evictionMiddleware) startTLBI(tx *evictionTransaction) {
	tx.phase = evictionStageTLBI
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

// startD2H transfers every valid page of the region to its CPU backing
// (todo 16; §18.3: the clean D2H is never omitted). // sbin_codex
func (m *evictionMiddleware) startD2H(tx *evictionTransaction) {
	tx.phase = evictionStageD2H
	t, err := m.driver.startEvictionD2H(tx.reg, tx.GPU, tx.RegionBase)
	if err != nil {
		panic(err)
	}
	tx.d2h = t
}

// finalPTE installs the final PTE by mode: INVALID when the Access Counter is
// off, REMOTE (pointing at the CPU backing) when on (§19, §21.4 step 11).
// // sbin_codex
func (m *evictionMiddleware) finalPTE(tx *evictionTransaction) {
	tx.phase = evictionStageFinalPTE
	reg := tx.reg
	allocStart, valid := m.regionPages(tx)
	remote := m.driver.uvm.config.AccessCounter
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		ps := m.driver.uvm.pageStateLocked(reg, page)
		pg := vm.Page{
			PID:      tx.PID,
			VAddr:    reg.Base + page*basePageSize,
			PageSize: basePageSize,
			Managed:  true,
		}
		if remote {
			pg.PAddr = ps.CPUPhysicalPage
			pg.Valid = true
			pg.Location = vm.MemoryLocationCPU_REMOTE
		} else {
			pg.PAddr = 0
			pg.Valid = false
			pg.Location = vm.MemoryLocationINVALID
		}
		m.driver.memAllocator.UpdatePage(pg)
	}
	m.advance(tx, evictionStageFreeing)
}

// freeFrames returns the region's GPU frames, clears the residency, releases
// the reservation (I -> 0), and marks the region CPU_RESIDENT (§21.4 steps
// 12-13). // sbin_codex
func (m *evictionMiddleware) freeFrames(tx *evictionTransaction) {
	tx.phase = evictionStageFreeing
	if err := m.driver.uvm.freeEvictionFrames(
		tx, m.driver.Engine.CurrentTime()); err != nil {
		panic(err)
	}
	m.advance(tx, evictionStageReplaying)
}

// startReplay issues the one GMMU replay for the evicted region; the
// transaction unblocks only after the replay ack. // sbin_codex
func (m *evictionMiddleware) startReplay(tx *evictionTransaction) {
	tx.phase = evictionStageReplaying
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
func (m *evictionMiddleware) startUnblock(tx *evictionTransaction) {
	m.driver.uvm.ReleaseOwnership(tx.Key, tx.Ticket)
	tx.phase = evictionStageUnblocking
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
// coalescing entry is removed, the completion is reported to the
// coordinator, and the ticket queue is woken. // sbin_codex
func (m *evictionMiddleware) finish(tx *evictionTransaction) {
	m.driver.uvm.completeEviction(tx)
	m.reportEvictionRoot(tx)
	tx.phase = evictionStageDone
	m.active = nil
}

// abort retires the transaction at an injected stage failure: the ownership
// slot and coalescing entry are released, the reservation is restored when it
// was moved but not completed, and the ticket queue is woken. The region-state
// authority is preserved — no illegal transition is attempted. // sbin_codex
func (m *evictionMiddleware) abort(tx *evictionTransaction) {
	m.driver.uvm.abortEviction(tx)
	tx.phase = evictionStageDone
	m.active = nil
}

// regionPages returns the allocation page range of the transaction's region.
// sbin_codex
func (m *evictionMiddleware) regionPages(
	tx *evictionTransaction,
) (allocStart, valid uint64) {
	reg := tx.reg
	blockIdx := (BlockForVA(tx.RegionBase) - BlockForVA(reg.Base)) /
		vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (tx.RegionBase - block.StartVA) / subblockSizeBytes
	return (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
}

// processIncoming consumes the responses owned by the active transaction and
// leaves every other message for the next middleware. // sbin_codex
func (m *evictionMiddleware) processIncoming() bool {
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
		case *protocol.UVMCacheRangeFlushRsp:
			if m.processFlushRsp(req) {
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
// block/unblock/D2H requests; a stray completion is rejected. // sbin_codex
func (m *evictionMiddleware) processGeneralRsp(rsp *sim.GeneralRsp) bool {
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
					m.advance(tx, evictionStageFlushing)
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
					m.advance(tx, evictionStageDone)
				}
				return true
			}
		}
	case *protocol.MemCopyD2HReq:
		if tx.phase != evictionStageD2H || tx.d2h == nil {
			return false
		}
		if !tx.d2h.processRsp(rsp) {
			return false
		}
		if tx.d2h.pending == 0 {
			m.advance(tx, evictionStageFinalPTE)
		}
		return true
	}
	return false
}

// processFlushRsp completes the WB+INV phase; a stray ack is rejected.
// sbin_codex
func (m *evictionMiddleware) processFlushRsp(
	rsp *protocol.UVMCacheRangeFlushRsp,
) bool {
	tx := m.active
	if tx == nil || tx.phase != evictionStageFlushing {
		return false
	}
	for i, req := range tx.flushReqs {
		if req.ID == rsp.RspTo {
			tx.flushReqs = append(tx.flushReqs[:i], tx.flushReqs[i+1:]...)
			tx.pendingFlushes--
			if tx.pendingFlushes == 0 {
				m.advance(tx, evictionStageTransitioning)
			}
			return true
		}
	}
	return false
}

// processTLBRsp completes the TLB-invalidation phase; a stray ack is
// rejected. // sbin_codex
func (m *evictionMiddleware) processTLBRsp(
	rsp *protocol.UVMTLBInvalidateRsp,
) bool {
	tx := m.active
	if tx == nil || tx.phase != evictionStageTLBI {
		return false
	}
	for i, req := range tx.tlbReqs {
		if req.ID == rsp.RspTo {
			tx.tlbReqs = append(tx.tlbReqs[:i], tx.tlbReqs[i+1:]...)
			tx.pendingTLB--
			if tx.pendingTLB == 0 {
				m.advance(tx, evictionStageD2H)
			}
			return true
		}
	}
	return false
}

// processReplayRsp unblocks after the exactly-one replay ack; a duplicate or
// stray ack is rejected without double accounting. // sbin_codex
func (m *evictionMiddleware) processReplayRsp(
	rsp *protocol.UVMFaultReplayRsp,
) bool {
	tx := m.active
	if tx == nil || tx.replayReq == nil || rsp.RspTo != tx.replayReq.ID {
		return false
	}
	m.advance(tx, evictionStageUnblocking)
	return true
}

// evictionCandidate is one eligible 64 KB victim candidate. // sbin_codex
type evictionCandidate struct {
	reg               *ManagedAllocationRegistration
	blockIdx          uint64
	regionIdx         uint64
	regionBase        uint64
	lastMigrationTime sim.VTimeInSec
}

// selectEvictionVictimLocked returns the deterministic migration-recency LRU
// victim of pid on gpu: the eligible GPU-resident region with the oldest
// LastMigrationTime, breaking ties to the lower VA. Eligibility (§18.2): GPU
// resident, not migrating, not pinned, not in an in-progress fault/migration
// transaction, not already EVICTING, and not COPY-owned (todo 5). The caller
// must hold the manager lock. // sbin_codex
func (m *UVMManager) selectEvictionVictimLocked(
	pid vm.PID,
	gpu int,
) (*evictionCandidate, error) {
	var best *evictionCandidate
	for _, reg := range m.registrations {
		if reg.PID != pid {
			continue
		}
		for blockIdx, block := range reg.VABlocks {
			for regionIdx, region := range block.SubBlocks {
				if region.State != RegionGPUResident {
					continue
				}
				regionBase := block.StartVA + uint64(regionIdx)*subblockSizeBytes
				key := copyRegionKey{PID: pid, GPU: gpu, RegionBase: regionBase}
				if m.pinned[key] {
					continue
				}
				if e := m.ownershipFor(key); e.OwnerType != OwnershipIdle {
					continue
				}
				if m.faultByKey[key] != nil || m.migrationByKey[key] != nil ||
					m.evictByKey[key] != nil {
					continue
				}
				cand := &evictionCandidate{
					reg:               reg,
					blockIdx:          uint64(blockIdx),
					regionIdx:         uint64(regionIdx),
					regionBase:        regionBase,
					lastMigrationTime: region.LastMigrationTime,
				}
				if best == nil ||
					cand.lastMigrationTime < best.lastMigrationTime ||
					(cand.lastMigrationTime == best.lastMigrationTime &&
						cand.regionBase < best.regionBase) {
					best = cand
				}
			}
		}
	}
	if best == nil {
		return nil, fmt.Errorf(
			"uvm: no eligible eviction victim for pid=%d gpu=%d", pid, gpu)
	}
	return best, nil
}

// intakeEviction selects the deterministic LRU victim, marks the region
// EVICTING (GPU_RESIDENT -> EVICT_PENDING), claims the ownership slot, and
// records the coalescing entry — all under one manager lock, so a region is
// never selected twice. // sbin_codex
func (m *UVMManager) intakeEviction(
	pid vm.PID,
	gpu int,
) (*evictionTransaction, error) {
	m.Lock()
	defer m.Unlock()

	return m.intakeEvictionVictimLocked(pid, gpu)
}

// intakeEvictionVictimLocked selects the deterministic LRU victim, marks the
// region EVICTING (GPU_RESIDENT -> EVICT_PENDING), claims the ownership slot,
// and records the coalescing entry — all under one manager lock, so a region
// is never selected twice. The victim's logical bytes are recorded at launch
// so the projected-occupancy E term counts launched victims (todo 20). The
// caller must hold the manager lock. // sbin_codex
func (m *UVMManager) intakeEvictionVictimLocked(
	pid vm.PID,
	gpu int,
) (*evictionTransaction, error) {
	cand, err := m.selectEvictionVictimLocked(pid, gpu)
	if err != nil {
		return nil, err
	}
	key := copyRegionKey{PID: pid, GPU: gpu, RegionBase: cand.regionBase}
	sm := NewRegionStateMachine(
		RegionContext{
			PID: pid, GPU: gpu, Block: cand.blockIdx, Region: cand.regionIdx,
		},
		cand.reg.VABlocks[cand.blockIdx].SubBlocks[cand.regionIdx])
	if err := sm.Transition(RegionEvictPending, 0); err != nil {
		return nil, err
	}
	tx := &evictionTransaction{
		Ticket:      m.nextTicketLocked(),
		PID:         pid,
		GPU:         gpu,
		RegionBase:  cand.regionBase,
		Key:         key,
		reg:         cand.reg,
		ReplayToken: m.nextReplayTokenLocked(),
		phase:       evictionStageClaiming,
		// sbin_codex (todo 20): the victim's logical bytes at launch (the
		// same value beginEvictionMigration records at the R->I move).
		bytes: m.victimBytesLocked(cand.reg, cand.regionBase),
	}
	e := m.ownershipFor(key)
	e.OwnerType = OwnershipEviction
	e.OwnerID = tx.Ticket
	m.evictByKey[key] = tx
	return tx, nil
}

// victimBytesLocked returns the logical bytes of the 64 KB region containing
// regionBase of reg: every valid page at 4 KB (uvm-manager.md §18.3). The
// caller must hold the manager lock. // sbin_codex
func (m *UVMManager) victimBytesLocked(
	reg *ManagedAllocationRegistration,
	regionBase uint64,
) uint64 {
	blockIdx := (BlockForVA(regionBase) - BlockForVA(reg.Base)) /
		vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (regionBase - block.StartVA) / subblockSizeBytes
	_, valid := (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
	return valid * basePageSize
}

// beginEvictionMigration transitions the region EVICT_PENDING ->
// MIGRATING_TO_CPU and moves the reservation R -> I. // sbin_codex
func (m *UVMManager) beginEvictionMigration(
	tx *evictionTransaction,
	bytes uint64,
	now sim.VTimeInSec,
) error {
	m.Lock()
	defer m.Unlock()

	reg := tx.reg
	if reg == nil {
		return fmt.Errorf("uvm: eviction migration without a registration")
	}
	sm := m.faultRegionMachineLocked(reg, tx.GPU, tx.RegionBase)
	if err := sm.Transition(RegionMigratingToCPU, now); err != nil {
		return err
	}
	m.reservation.StartMigration(bytes)
	tx.migrated = true
	tx.bytes = bytes
	return nil
}

// freeEvictionFrames returns the region's GPU frames, clears the residency and
// the per-page GPU PAs, releases the reservation (I -> 0), and marks the
// region CPU_RESIDENT. // sbin_codex
func (m *UVMManager) freeEvictionFrames(
	tx *evictionTransaction,
	now sim.VTimeInSec,
) error {
	m.Lock()
	defer m.Unlock()

	reg := tx.reg
	if reg == nil {
		return fmt.Errorf("uvm: eviction free without a registration")
	}
	blockIdx := (BlockForVA(tx.RegionBase) - BlockForVA(reg.Base)) /
		vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (tx.RegionBase - block.StartVA) / subblockSizeBytes
	allocStart, valid := (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
	frames := make([]uint64, 0, valid)
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := m.pageStateLocked(reg, page)
		if p.GPUPhysicalPage != 0 {
			frames = append(frames, p.GPUPhysicalPage)
		}
		p.GPUPhysicalPage = 0
		setMaskBit(reg.ResidentMask, page, false)
	}
	if m.frames != nil {
		m.frames.freeMigrationFrames(frames)
	}
	sm := m.faultRegionMachineLocked(reg, tx.GPU, tx.RegionBase)
	if err := sm.Transition(RegionCPUResident, now); err != nil {
		return err
	}
	m.reservation.CompleteMigrationToCPU(tx.bytes)
	tx.completed = true
	return nil
}

// completeEviction retires a completed eviction transaction: the coalescing
// entry is removed and the ticket queue is woken so a waiting copy can claim.
// The ownership slot was already released before the unblock. // sbin_codex
func (m *UVMManager) completeEviction(tx *evictionTransaction) {
	m.Lock()
	defer m.Unlock()

	delete(m.evictByKey, tx.Key)
	// sbin_codex (todo 20): a completed pre-eviction leaves the concurrent
	// pre-eviction set.
	if tx.preEviction {
		m.preEviction.numConcurrentPreEvictions--
	}
	m.reevaluateLocked()
}

// abortEviction retires an eviction at an injected stage failure: the
// coalescing entry and ownership slot are released, the reservation is
// restored when it was moved but not completed, and the ticket queue is woken.
// The region-state authority is preserved — no illegal transition is
// attempted. // sbin_codex
func (m *UVMManager) abortEviction(tx *evictionTransaction) {
	m.Lock()
	defer m.Unlock()

	delete(m.evictByKey, tx.Key)
	if e := m.ownershipFor(tx.Key); e.OwnerID == tx.Ticket {
		e.OwnerType = OwnershipIdle
		e.OwnerID = 0
	}
	// sbin_codex (todo 20): an aborted pre-eviction leaves the concurrent
	// pre-eviction set.
	if tx.preEviction {
		m.preEviction.numConcurrentPreEvictions--
	}
	if tx.migrated && !tx.completed {
		m.reservation.CompleteMigrationToGPU(tx.bytes)
	}
	m.reevaluateLocked()
}
