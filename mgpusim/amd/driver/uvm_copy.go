package driver

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: residency-neutral managed H2D initialization and D2H readback
// (plan todo 5 of mgpusim-uvm-manager, uvm-manager.md §3, §19.1, §23.2).
// A managed copy obtains one global monotonically ticketed request, claims
// every (PID, GPU, regionBase) key as one COPY transaction under one manager
// lock (all or none), blocks all locations and waits the watermark acks,
// transfers data per location (CPU_REMOTE/INVALID -> CPU backing; GPU_LOCAL
// H2D -> WB+INV then HBM overwrite; D2H -> writeback-only then HBM read),
// then releases all keys atomically, unblocks, and wakes the ticket queue.
// Residency, counters, and global controls are never touched.

// copyPhase is the progress of one managed copy transaction.
type copyPhase int

const (
	// copyPhaseWaiting claims keys (the copy may be enqueued by ticket).
	copyPhaseWaiting copyPhase = iota
	// copyPhaseBlocking waits for the BlockRange watermark acks.
	copyPhaseBlocking
	// copyPhaseFlushing waits for the range WB+INV / writeback-only acks.
	copyPhaseFlushing
	// copyPhaseData transfers bytes (HBM DMA + CPU backing).
	copyPhaseData
	// copyPhaseUnblocking waits for the UnblockRange acks.
	copyPhaseUnblocking
	// copyPhaseDone releases, wakes, and completes the command.
	copyPhaseDone
)

// copyTransaction is one residency-neutral managed H2D/D2H copy. It owns all
// its keys or none; while waiting it holds no key. // sbin_codex
type copyTransaction struct {
	Ticket  uint64 // global monotonic ticket
	PID     vm.PID
	GPU     int // 1-based GPU ID
	StartVA uint64
	Size    uint64
	IsH2D   bool
	Data    []byte // H2D source bytes / D2H destination bytes
	Keys    []copyRegionKey

	cmd   Command
	queue *CommandQueue

	phase    copyPhase
	claimed  bool
	enqueued bool

	blockReqs   []*vm.BlockRange
	unblockReqs []*vm.UnblockRange
	flushReqs   []*protocol.UVMCacheRangeFlushReq
	dmaReqs     []sim.Msg

	pendingBlocks   int
	pendingFlushes  int
	pendingDMA      int
	pendingUnblocks int
}

// distinctGPUs returns the sorted distinct GPU IDs of the copy's keys.
func (tx *copyTransaction) distinctGPUs() []int {
	seen := make(map[int]bool)
	for _, key := range tx.Keys {
		seen[key.GPU] = true
	}
	gpus := make([]int, 0, len(seen))
	for gpu := range seen {
		gpus = append(gpus, gpu)
	}
	sort.Ints(gpus)
	return gpus
}

// managedMemoryCopyMiddleware drives managed H2D/D2H copies. It is owned by
// the default memory-copy middleware, which branches copies by allocation.
// // sbin_codex
type managedMemoryCopyMiddleware struct {
	driver *Driver
	copies []*copyTransaction
}

// tryProcess handles a copy command whose span is fully managed. handled is
// false when the command is not a managed copy and must fall through to the
// default path; a mixed/gapped span is rejected with a panic. // sbin_codex
func (m *managedMemoryCopyMiddleware) tryProcess(
	cmd Command,
	queue *CommandQueue,
) (processed, handled bool) {
	switch cmd := cmd.(type) {
	case *MemCopyH2DCommand:
		size := uint64(binary.Size(cmd.Src))
		managed, err := m.driver.uvm.classifySpan(
			queue.Context.pid, uint64(cmd.Dst), size)
		if err != nil {
			panic(err)
		}
		if !managed {
			return false, false
		}
		return m.processH2D(cmd, queue), true
	case *MemCopyD2HCommand:
		size := uint64(binary.Size(cmd.Dst))
		managed, err := m.driver.uvm.classifySpan(
			queue.Context.pid, uint64(cmd.Src), size)
		if err != nil {
			panic(err)
		}
		if !managed {
			return false, false
		}
		return m.processD2H(cmd, queue), true
	}
	return false, false
}

// processH2D serializes the host source and starts a managed H2D copy.
func (m *managedMemoryCopyMiddleware) processH2D(
	cmd *MemCopyH2DCommand,
	queue *CommandQueue,
) bool {
	buffer := bytes.NewBuffer(nil)
	err := binary.Write(buffer, binary.LittleEndian, cmd.Src)
	if err != nil {
		panic(err)
	}
	return m.startCopy(queue, uint64(cmd.Dst), uint64(buffer.Len()),
		true, buffer.Bytes(), cmd)
}

// processD2H allocates the host destination and starts a managed D2H copy.
func (m *managedMemoryCopyMiddleware) processD2H(
	cmd *MemCopyD2HCommand,
	queue *CommandQueue,
) bool {
	size := uint64(binary.Size(cmd.Dst))
	return m.startCopy(queue, uint64(cmd.Src), size, false,
		make([]byte, size), cmd)
}

// startCopy builds the transaction, obtains its global ticket, and claims all
// keys atomically; on success it blocks immediately, otherwise it enqueues
// once by ticket and waits. // sbin_codex
func (m *managedMemoryCopyMiddleware) startCopy(
	queue *CommandQueue,
	startVA, size uint64,
	isH2D bool,
	data []byte,
	cmd Command,
) bool {
	tx := &copyTransaction{
		Ticket:  m.driver.uvm.NextCopyTicket(),
		PID:     queue.Context.pid,
		GPU:     queue.GPUID,
		StartVA: startVA,
		Size:    size,
		IsH2D:   isH2D,
		Data:    data,
		cmd:     cmd,
		queue:   queue,
		phase:   copyPhaseWaiting,
	}
	tx.Keys = m.driver.uvm.copyKeysForSpan(tx.PID, tx.GPU, startVA, size)
	if m.driver.uvm.claimCopy(tx) {
		m.sendBlocks(tx)
		tx.phase = copyPhaseBlocking
	} else {
		m.driver.uvm.enqueueCopy(tx)
	}
	m.copies = append(m.copies, tx)
	queue.IsRunning = true
	return true
}

// sendBlocks blocks every GPU covered by the keys for the whole span. // sbin_codex
func (m *managedMemoryCopyMiddleware) sendBlocks(tx *copyTransaction) {
	for _, gpu := range tx.distinctGPUs() {
		req := &vm.BlockRange{
			CommandID: tx.Ticket,
			PID:       tx.PID,
			StartVA:   tx.StartVA,
			Size:      tx.Size,
		}
		req.ID = sim.GetIDGenerator().Generate()
		req.Src = m.driver.gpuPort.AsRemote()
		req.Dst = m.driver.GPUs[gpu-1].AsRemote()
		tx.blockReqs = append(tx.blockReqs, req)
		tx.pendingBlocks++
		m.driver.requestsToSend = append(m.driver.requestsToSend, req)
	}
}

// Tick consumes the middleware's responses and drives every active copy. // sbin_codex
func (m *managedMemoryCopyMiddleware) Tick() (madeProgress bool) {
	madeProgress = false
	madeProgress = m.processIncoming() || madeProgress
	for _, tx := range m.copies {
		madeProgress = m.drive(tx) || madeProgress
	}
	m.removeDoneCopies()
	return madeProgress
}

// processIncoming consumes the responses owned by active copies and leaves
// every other message for the default middleware. // sbin_codex
func (m *managedMemoryCopyMiddleware) processIncoming() bool {
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
		}
		break
	}
	return madeProgress
}

// processGeneralRsp matches a completion to its copy transaction. // sbin_codex
func (m *managedMemoryCopyMiddleware) processGeneralRsp(rsp *sim.GeneralRsp) bool {
	switch originalReq := rsp.OriginalReq.(type) {
	case *vm.BlockRange:
		tx := m.findTxByBlockReq(originalReq)
		if tx == nil {
			return false
		}
		tx.pendingBlocks--
		if tx.pendingBlocks == 0 {
			m.advance(tx)
		}
		return true
	case *vm.UnblockRange:
		tx := m.findTxByUnblockReq(originalReq)
		if tx == nil {
			return false
		}
		tx.pendingUnblocks--
		if tx.pendingUnblocks == 0 {
			m.finishCopy(tx)
		}
		return true
	case *protocol.MemCopyH2DReq, *protocol.MemCopyD2HReq:
		tx := m.findTxByDMAReq(originalReq)
		if tx == nil {
			return false
		}
		tx.pendingDMA--
		m.removeDMAReq(tx, originalReq)
		if tx.pendingDMA == 0 {
			m.advance(tx)
		}
		return true
	}
	return false
}

// processFlushRsp matches a range-flush completion to its copy transaction. // sbin_codex
func (m *managedMemoryCopyMiddleware) processFlushRsp(
	rsp *protocol.UVMCacheRangeFlushRsp,
) bool {
	for _, tx := range m.copies {
		for i, req := range tx.flushReqs {
			if req.ID == rsp.RspTo {
				tx.flushReqs = append(tx.flushReqs[:i], tx.flushReqs[i+1:]...)
				tx.pendingFlushes--
				if tx.pendingFlushes == 0 {
					m.advance(tx)
				}
				return true
			}
		}
	}
	return false
}

// advance moves the transaction to its next phase after a completion. // sbin_codex
func (m *managedMemoryCopyMiddleware) advance(tx *copyTransaction) {
	switch tx.phase {
	case copyPhaseBlocking:
		m.startFlush(tx)
	case copyPhaseFlushing:
		m.startData(tx)
	case copyPhaseData:
		m.startUnblock(tx)
	}
}

// drive starts a claimed waiting copy by blocking its locations. // sbin_codex
func (m *managedMemoryCopyMiddleware) drive(tx *copyTransaction) bool {
	if tx.phase != copyPhaseWaiting || !tx.claimed {
		return false
	}
	m.sendBlocks(tx)
	tx.phase = copyPhaseBlocking
	return true
}

// startFlush issues the per-region range cache operation for the resident
// pages of the span; without resident pages it proceeds straight to the data
// phase. // sbin_codex
func (m *managedMemoryCopyMiddleware) startFlush(tx *copyTransaction) {
	reqs := m.buildFlushReqs(tx)
	if len(reqs) == 0 {
		m.startData(tx)
		return
	}
	tx.flushReqs = reqs
	tx.pendingFlushes = len(reqs)
	tx.phase = copyPhaseFlushing
	for _, req := range reqs {
		m.driver.requestsToSend = append(m.driver.requestsToSend, req)
	}
}

// startData transfers the span bytes: HBM pages through the DMA engine,
// CPU-backed pages directly to/from the CPU backing. // sbin_codex
func (m *managedMemoryCopyMiddleware) startData(tx *copyTransaction) {
	tx.phase = copyPhaseData
	if tx.IsH2D {
		m.startDataH2D(tx)
	} else {
		m.startDataD2H(tx)
	}
	if tx.pendingDMA == 0 {
		m.startUnblock(tx)
	}
}

// startDataH2D writes every span page: HBM overwrite via MemCopyH2DReq for
// GPU-resident pages, CPU backing write for CPU_REMOTE/INVALID pages. // sbin_codex
func (m *managedMemoryCopyMiddleware) startDataH2D(tx *copyTransaction) {
	m.driver.uvm.forEachSpanPage(tx.PID, tx.StartVA, tx.Size,
		func(va, offset, length uint64, info managedPageInfo) {
			if info.Resident {
				req := protocol.NewMemCopyH2DReq(
					m.driver.gpuPort, m.driver.GPUs[tx.GPU-1],
					tx.Data[offset:offset+length], info.GPUPhysicalPage)
				tx.dmaReqs = append(tx.dmaReqs, req)
				tx.pendingDMA++
				m.driver.requestsToSend = append(m.driver.requestsToSend, req)
			} else {
				m.driver.globalStorage.Write(
					info.CPUPhysicalPage, tx.Data[offset:offset+length])
			}
		})
}

// startDataD2H reads every span page: HBM read via MemCopyD2HReq for
// GPU-resident pages, CPU backing read for CPU_REMOTE/INVALID pages. // sbin_codex
func (m *managedMemoryCopyMiddleware) startDataD2H(tx *copyTransaction) {
	m.driver.uvm.forEachSpanPage(tx.PID, tx.StartVA, tx.Size,
		func(va, offset, length uint64, info managedPageInfo) {
			if info.Resident {
				req := protocol.NewMemCopyD2HReq(
					m.driver.gpuPort, m.driver.GPUs[tx.GPU-1],
					info.GPUPhysicalPage, tx.Data[offset:offset+length])
				tx.dmaReqs = append(tx.dmaReqs, req)
				tx.pendingDMA++
				m.driver.requestsToSend = append(m.driver.requestsToSend, req)
			} else {
				data, err := m.driver.globalStorage.Read(
					info.CPUPhysicalPage, length)
				if err != nil {
					panic(err)
				}
				copy(tx.Data[offset:offset+length], data)
			}
		})
}

// buildFlushReqs builds one range cache operation per 64 KB region of the
// span that has GPU-resident pages: WRITEBACK_INVALIDATE for H2D overwrite,
// WRITEBACK_ONLY for D2H readback. The mask marks the resident pages and the
// runs map them to their HBM PAs in VA order. // sbin_codex
func (m *managedMemoryCopyMiddleware) buildFlushReqs(
	tx *copyTransaction,
) []*protocol.UVMCacheRangeFlushReq {
	op := cache.UVMCacheRangeFlushWritebackInvalidate
	if !tx.IsH2D {
		op = cache.UVMCacheRangeFlushWritebackOnly
	}

	type regionFlush struct {
		mask uint64
		runs []cache.PhysicalRun
	}
	regions := make(map[uint64]*regionFlush)
	m.driver.uvm.forEachSpanPage(tx.PID, tx.StartVA, tx.Size,
		func(va, offset, length uint64, info managedPageInfo) {
			if !info.Resident {
				return
			}
			regionBase := SubBlockStartVA(va)
			rf := regions[regionBase]
			if rf == nil {
				rf = &regionFlush{}
				regions[regionBase] = rf
			}
			rf.mask |= uint64(1) << ((va - regionBase) / basePageSize)
			if n := len(rf.runs); n > 0 &&
				rf.runs[n-1].Start+rf.runs[n-1].Length == info.GPUPhysicalPage {
				rf.runs[n-1].Length += basePageSize
			} else {
				rf.runs = append(rf.runs, cache.PhysicalRun{
					Start:  info.GPUPhysicalPage,
					Length: basePageSize,
				})
			}
		})

	bases := make([]uint64, 0, len(regions))
	for base := range regions {
		bases = append(bases, base)
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })

	reqs := make([]*protocol.UVMCacheRangeFlushReq, 0, len(bases))
	for _, base := range bases {
		rf := regions[base]
		req := protocol.UVMCacheRangeFlushReqBuilder{}.
			WithSrc(m.driver.gpuPort.AsRemote()).
			WithDst(m.driver.GPUs[tx.GPU-1].AsRemote()).
			WithOperation(op).
			WithPID(tx.PID).
			WithVABase(base).
			WithValidPageMask(rf.mask).
			WithPhysicalRuns(rf.runs).
			Build()
		reqs = append(reqs, req)
	}
	return reqs
}

// startUnblock releases all keys atomically, then unblocks every blocked GPU. // sbin_codex
func (m *managedMemoryCopyMiddleware) startUnblock(tx *copyTransaction) {
	m.driver.uvm.releaseCopyKeys(tx, false)
	tx.phase = copyPhaseUnblocking
	for _, gpu := range tx.distinctGPUs() {
		req := &vm.UnblockRange{
			CommandID: tx.Ticket,
			PID:       tx.PID,
			StartVA:   tx.StartVA,
			Size:      tx.Size,
		}
		req.ID = sim.GetIDGenerator().Generate()
		req.Src = m.driver.gpuPort.AsRemote()
		req.Dst = m.driver.GPUs[gpu-1].AsRemote()
		tx.unblockReqs = append(tx.unblockReqs, req)
		tx.pendingUnblocks++
		m.driver.requestsToSend = append(m.driver.requestsToSend, req)
	}
}

// finishCopy wakes the ticket queue, materializes the D2H destination, and
// completes the command. // sbin_codex
func (m *managedMemoryCopyMiddleware) finishCopy(tx *copyTransaction) {
	m.driver.uvm.wakeTickets()
	if !tx.IsH2D {
		buf := bytes.NewReader(tx.Data)
		err := binary.Read(buf, binary.LittleEndian, tx.cmd.(*MemCopyD2HCommand).Dst)
		if err != nil {
			panic(err)
		}
	}
	tx.queue.IsRunning = false
	tx.queue.Dequeue()
	m.driver.logCmdComplete(tx.cmd)
	tx.phase = copyPhaseDone
}

// removeDoneCopies drops completed transactions from the active list. // sbin_codex
func (m *managedMemoryCopyMiddleware) removeDoneCopies() {
	remaining := m.copies[:0]
	for _, tx := range m.copies {
		if tx.phase != copyPhaseDone {
			remaining = append(remaining, tx)
		}
	}
	m.copies = remaining
}

// findTxByBlockReq locates the copy that sent req. // sbin_codex
func (m *managedMemoryCopyMiddleware) findTxByBlockReq(
	req *vm.BlockRange,
) *copyTransaction {
	for _, tx := range m.copies {
		for _, r := range tx.blockReqs {
			if r == req {
				return tx
			}
		}
	}
	return nil
}

// findTxByUnblockReq locates the copy that sent req. // sbin_codex
func (m *managedMemoryCopyMiddleware) findTxByUnblockReq(
	req *vm.UnblockRange,
) *copyTransaction {
	for _, tx := range m.copies {
		for _, r := range tx.unblockReqs {
			if r == req {
				return tx
			}
		}
	}
	return nil
}

// findTxByDMAReq locates the copy that sent req. // sbin_codex
func (m *managedMemoryCopyMiddleware) findTxByDMAReq(req sim.Msg) *copyTransaction {
	for _, tx := range m.copies {
		for _, r := range tx.dmaReqs {
			if r == req {
				return tx
			}
		}
	}
	return nil
}

// removeDMAReq drops a completed DMA request from its transaction. // sbin_codex
func (m *managedMemoryCopyMiddleware) removeDMAReq(tx *copyTransaction, req sim.Msg) {
	for i, r := range tx.dmaReqs {
		if r == req {
			tx.dmaReqs = append(tx.dmaReqs[:i], tx.dmaReqs[i+1:]...)
			return
		}
	}
}
