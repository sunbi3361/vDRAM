package driver

import (
	"bytes"
	"encoding/binary"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// defaultMemoryCopyMiddleware handles memory copy commands and related
// communication.
type defaultMemoryCopyMiddleware struct {
	driver *Driver

	cyclesPerH2D int
	cyclesPerD2H int
	cyclesLeft   int

	awaitingReqs []sim.Msg

	// sbin_codex: deferred managed D2H state. The data read is delayed until
	// the GPU flush completes and all UVM faults/migrations in the copied
	// range drain, so CPU-resident pages written by the kernel are migrated
	// before their data is read back.
	pendingD2H        *deferredD2H
	pendingD2HFlushed bool
}

type deferredD2H struct {
	cmd   *MemCopyD2HCommand
	queue *CommandQueue
	pid   vm.PID
	start uint64
	size  uint64
}

func (m *defaultMemoryCopyMiddleware) ProcessCommand(
	cmd Command,
	queue *CommandQueue,
) (processed bool) {
	switch cmd := cmd.(type) {
	case *MemCopyH2DCommand:
		return m.processMemCopyH2DCommand(cmd, queue)
	case *MemCopyD2HCommand:
		return m.processMemCopyD2HCommand(cmd, queue)
	}

	return false
}

func (m *defaultMemoryCopyMiddleware) processMemCopyH2DCommand(
	cmd *MemCopyH2DCommand,
	queue *CommandQueue,
) bool {
	if m.needFlushing(queue.Context, cmd.Dst, uint64(binary.Size(cmd.Src))) {
		m.sendFlushRequest(cmd)
	}

	buffer := bytes.NewBuffer(nil)
	err := binary.Write(buffer, binary.LittleEndian, cmd.Src)
	if err != nil {
		panic(err)
	}
	rawBytes := buffer.Bytes()

	offset := uint64(0)
	addr := uint64(cmd.Dst)
	sizeLeft := uint64(len(rawBytes))
	for sizeLeft > 0 {
		page, found := m.driver.pageTable.Find(queue.Context.pid, addr)
		if !found {
			panic("page not found")
		}

		pAddr := page.PAddr + (addr - page.VAddr)
		sizeLeftInPage := page.PageSize - (addr - page.VAddr)
		sizeToCopy := sizeLeftInPage
		if sizeLeft < sizeLeftInPage {
			sizeToCopy = sizeLeft
		}

		// sbin_codex: managed CPU-resident pages copy straight into the host
		// backing frame in globalStorage instead of DMA to a GPU.
		if page.Managed && page.DeviceID == 0 {
			m.driver.globalStorage.Write(pAddr, rawBytes[offset:offset+sizeToCopy])
			sizeLeft -= sizeToCopy
			addr += sizeToCopy
			offset += sizeToCopy
			continue
		}

		gpuID := m.driver.memAllocator.GetDeviceIDByPAddr(pAddr)
		req := protocol.NewMemCopyH2DReq(
			m.driver.gpuPort, m.driver.GPUs[gpuID-1],
			rawBytes[offset:offset+sizeToCopy],
			pAddr)
		cmd.Reqs = append(cmd.Reqs, req)
		m.awaitingReqs = append(m.awaitingReqs, req)
		// m.driver.requestsToSend = append(m.driver.requestsToSend, req)

		sizeLeft -= sizeToCopy
		addr += sizeToCopy
		offset += sizeToCopy

		m.driver.logTaskToGPUInitiate(cmd, req)
	}

	m.cyclesLeft = m.cyclesPerH2D

	// sbin_codex: a fully managed CPU-resident copy created no DMA requests;
	// complete the command immediately instead of waiting for responses.
	//
	// Pre-edit code (commented per project convention):
	// if len(cmd.Reqs) == 0 && len(m.awaitingReqs) == 0 {
	//
	// sbin_claude: awaitingReqs belongs to the middleware, not to this
	// command. It holds whatever an earlier command queued and has not yet
	// handed to the driver's send queue, which happens only once cyclesLeft
	// counts down. A copy that produced no request of its own therefore
	// failed this test whenever another command was still in that window, fell
	// through to queue.IsRunning = true, and waited forever for a response
	// that nothing would ever send: the engine ran out of events with the
	// command queue still marked running. Only this command's own request list
	// can decide whether it has anything to wait for.
	if len(cmd.Reqs) == 0 {
		queue.IsRunning = false
		queue.Dequeue()
		m.driver.logCmdComplete(cmd)
		return true
	}

	queue.IsRunning = true

	return true
}

func (m *defaultMemoryCopyMiddleware) processMemCopyD2HCommand(
	cmd *MemCopyD2HCommand,
	queue *CommandQueue,
) bool {
	size := uint64(binary.Size(cmd.Dst))
	needsFlush := m.needFlushing(queue.Context, cmd.Src, size)
	hasManaged := m.rangeHasManagedPages(queue.Context.pid, uint64(cmd.Src), size)

	// sbin_codex: for a managed range that may hold dirty GPU-cache data,
	// defer the read until the flush completes and the UVM manager drains the
	// faults triggered by the flush write-backs.
	if needsFlush && hasManaged && m.driver.uvm != nil {
		m.sendFlushRequest(cmd)
		queue.Context.removeFreedBuffers()
		cmd.RawData = make([]byte, size)
		m.pendingD2H = &deferredD2H{
			cmd:   cmd,
			queue: queue,
			pid:   queue.Context.pid,
			start: uint64(cmd.Src),
			size:  size,
		}
		m.pendingD2HFlushed = false
		queue.IsRunning = true
		return true
	}

	if needsFlush {
		m.sendFlushRequest(cmd)
		queue.Context.removeFreedBuffers()
	}

	m.readManagedD2H(cmd, queue)
	return true
}

// readManagedD2H performs the byte-level D2H read for a copy command, reading
// CPU-resident managed pages from the host backing and GPU-resident pages via
// DMA from the GPU.
func (m *defaultMemoryCopyMiddleware) readManagedD2H(
	cmd *MemCopyD2HCommand,
	queue *CommandQueue,
) {
	cmd.RawData = make([]byte, binary.Size(cmd.Dst))

	offset := uint64(0)
	addr := uint64(cmd.Src)
	sizeLeft := uint64(len(cmd.RawData))
	for sizeLeft > 0 {
		page, found := m.driver.pageTable.Find(queue.Context.pid, addr)
		if !found {
			panic("page not found")
		}

		pAddr := page.PAddr + (addr - page.VAddr)
		sizeLeftInPage := page.PageSize - (addr - page.VAddr)
		sizeToCopy := sizeLeftInPage
		if sizeLeft < sizeLeftInPage {
			sizeToCopy = sizeLeft
		}

		// sbin_codex: managed CPU-resident pages copy straight out of the host
		// backing frame in globalStorage instead of DMA from a GPU.
		if page.Managed && page.DeviceID == 0 {
			data, _ := m.driver.globalStorage.Read(pAddr, sizeToCopy)
			copy(cmd.RawData[offset:offset+sizeToCopy], data)
			sizeLeft -= sizeToCopy
			addr += sizeToCopy
			offset += sizeToCopy
			continue
		}

		gpuID := m.driver.memAllocator.GetDeviceIDByPAddr(pAddr)
		req := protocol.NewMemCopyD2HReq(
			m.driver.gpuPort, m.driver.GPUs[gpuID-1],
			pAddr, cmd.RawData[offset:offset+sizeToCopy])
		cmd.Reqs = append(cmd.Reqs, req)
		m.awaitingReqs = append(m.awaitingReqs, req)

		sizeLeft -= sizeToCopy
		addr += sizeToCopy
		offset += sizeToCopy

		m.driver.logTaskToGPUInitiate(cmd, req)
	}

	// sbin_codex: a fully managed CPU-resident copy created no DMA requests;
	// complete the command immediately.
	//
	// Pre-edit code (commented per project convention):
	// if len(cmd.Reqs) == 0 && len(m.awaitingReqs) == 0 {
	//
	// sbin_claude: same shared-state bug as the H2D path above.
	if len(cmd.Reqs) == 0 {
		queue.IsRunning = false
		buf := bytes.NewReader(cmd.RawData)
		err := binary.Read(buf, binary.LittleEndian, cmd.Dst)
		if err != nil {
			panic(err)
		}
		queue.Dequeue()
		m.driver.logCmdComplete(cmd)
		return
	}

	m.cyclesLeft = m.cyclesPerD2H

	queue.IsRunning = true
}

func (m *defaultMemoryCopyMiddleware) needFlushing(
	ctx *Context,
	vAddr Ptr,
	size uint64,
) bool {
	startAddr := uint64(vAddr)
	endAddr := uint64(vAddr) + size
	for _, buf := range ctx.buffers {
		bufStartAddr := uint64(buf.vAddr)
		bufEndAddr := uint64(buf.vAddr) + buf.size
		if memRangeOverlap(bufStartAddr, bufEndAddr, startAddr, endAddr) {
			if buf.l2Dirty {
				return true
			}
		}
	}

	return false
}

func memRangeOverlap(
	start1, end1, start2, end2 uint64,
) bool {
	if start1 <= start2 && end1 > start2 {
		return true
	}

	if start1 < end2 && end1 >= end2 {
		return true
	}

	return false
}

func (m *defaultMemoryCopyMiddleware) sendFlushRequest(
	cmd Command,
) {
	for _, gpu := range m.driver.GPUs {
		req := protocol.NewFlushReq(m.driver.gpuPort, gpu)
		// m.driver.requestsToSend = append(m.driver.requestsToSend, req) // sbin_codex: queue writes must be synchronized.
		m.driver.enqueueRequestsToSend(req) // sbin_codex
		cmd.AddReq(req)

		m.driver.logTaskToGPUInitiate(cmd, req)
	}
}

func (m *defaultMemoryCopyMiddleware) Tick() (madeProgress bool) {
	madeProgress = false

	if m.pendingD2H != nil && m.pendingD2HFlushed {
		madeProgress = m.processDeferredD2H() || madeProgress
	}

	if m.cyclesLeft > 0 {
		m.cyclesLeft--
		madeProgress = true
	} else if m.cyclesLeft == 0 {
		// m.driver.requestsToSend = append(m.driver.requestsToSend, m.awaitingReqs...) // sbin_codex: queue writes must be synchronized.
		m.driver.enqueueRequestsToSend(m.awaitingReqs...) // sbin_codex
		m.awaitingReqs = nil
		m.cyclesLeft = -1
		madeProgress = true
	}

	req := m.driver.gpuPort.PeekIncoming()
	if req == nil {
		return madeProgress
	}

	switch req := req.(type) {
	case *sim.GeneralRsp:
		madeProgress = m.processGeneralRsp(req)
	}

	return madeProgress
}

func (m *defaultMemoryCopyMiddleware) processGeneralRsp(
	rsp *sim.GeneralRsp,
) bool {
	madeProgress := false
	originalReq := rsp.OriginalReq

	// sbin_codex: a MemCopy the UVM manager issued for a migration answers to
	// the UVM manager, never to a user command.
	if m.driver.ClaimUVMDMAReturn(originalReq) {
		return true
	}

	switch originalReq := originalReq.(type) {
	case *protocol.FlushReq:
		madeProgress = m.processFlushReturn(originalReq)
	case *protocol.MemCopyH2DReq:
		madeProgress = m.processMemCopyH2DReturn(originalReq)
	case *protocol.MemCopyD2HReq:
		madeProgress = m.processMemCopyD2HReturn(originalReq)
	}

	return madeProgress
}

func (m *defaultMemoryCopyMiddleware) processMemCopyH2DReturn(
	req *protocol.MemCopyH2DReq,
) bool {
	m.driver.gpuPort.RetrieveIncoming()

	m.driver.logTaskToGPUClear(req)

	cmd, cmdQueue := m.driver.findCommandByReq(req)

	copyCmd := cmd.(*MemCopyH2DCommand)
	newReqs := make([]sim.Msg, 0, len(copyCmd.Reqs)-1)
	for _, r := range copyCmd.GetReqs() {
		if r != req {
			newReqs = append(newReqs, r)
		}
	}
	copyCmd.Reqs = newReqs

	if len(copyCmd.Reqs) == 0 {
		cmdQueue.IsRunning = false
		cmdQueue.Dequeue()

		m.driver.logCmdComplete(cmd)
	}

	return true
}

func (m *defaultMemoryCopyMiddleware) processMemCopyD2HReturn(
	req *protocol.MemCopyD2HReq,
) bool {
	m.driver.gpuPort.RetrieveIncoming()

	m.driver.logTaskToGPUClear(req)

	cmd, cmdQueue := m.driver.findCommandByReq(req)

	copyCmd := cmd.(*MemCopyD2HCommand)
	copyCmd.RemoveReq(req)

	if len(copyCmd.Reqs) == 0 {
		cmdQueue.IsRunning = false
		buf := bytes.NewReader(copyCmd.RawData)
		err := binary.Read(buf, binary.LittleEndian, copyCmd.Dst)
		if err != nil {
			panic(err)
		}

		cmdQueue.Dequeue()

		m.driver.logCmdComplete(copyCmd)
	}

	return true
}

func (m *defaultMemoryCopyMiddleware) processFlushReturn(
	req *protocol.FlushReq,
) bool {
	m.driver.gpuPort.RetrieveIncoming()

	m.driver.logTaskToGPUClear(req)

	// Pre-edit code (commented per project convention):
	// cmd, _ := m.driver.findCommandByReq(req)
	cmd, cmdQueue := m.driver.findCommandByReq(req) // sbin_claude

	cmd.RemoveReq(req)

	// Pre-edit code (commented per project convention):
	// // sbin_codex: a deferred managed D2H becomes readable once every flush
	// // request of its command has returned.
	// if m.pendingD2H != nil && cmd == m.pendingD2H.cmd &&
	// 	len(cmd.GetReqs()) == 0 {
	// 	m.pendingD2HFlushed = true
	// }
	m.settleFlushedCommand(cmd, cmdQueue) // sbin_claude

	m.driver.logTaskToGPUClear(req)

	return true
}

// settleFlushedCommand decides what a copy command still owes once one of its
// flush requests has come back.
//
// sbin_claude: the case that was missing is a command whose flush request was
// its only request. A copy whose range is entirely managed and CPU-resident
// issues no DMA at all - it writes straight into globalStorage - so when it
// also needs a flush, cmd.Reqs holds nothing but that flush. The early
// completion at issue time cannot fire, because the list is not empty yet, and
// the copy-return handlers never run, because there is no copy request. The
// command therefore sat at the head of its queue with IsRunning set, and the
// simulation ran out of events with the benchmark still waiting on it. Nothing
// but this path is left to retire it.
func (m *defaultMemoryCopyMiddleware) settleFlushedCommand(
	cmd Command,
	queue *CommandQueue,
) {
	if len(cmd.GetReqs()) != 0 {
		// Copy requests are still outstanding; their returns finish it.
		return
	}

	// sbin_codex: a deferred managed D2H becomes readable once every flush
	// request of its command has returned.
	if m.pendingD2H != nil && cmd == m.pendingD2H.cmd {
		m.pendingD2HFlushed = true
		return
	}

	if d2h, ok := cmd.(*MemCopyD2HCommand); ok {
		// The bytes were gathered into RawData at issue time.
		buf := bytes.NewReader(d2h.RawData)
		if err := binary.Read(buf, binary.LittleEndian, d2h.Dst); err != nil {
			panic(err)
		}
	}

	queue.IsRunning = false
	queue.Dequeue()
	m.driver.logCmdComplete(cmd)
}

// processDeferredD2H performs the data read of a deferred managed D2H once the
// flush completed and no UVM fault or migration remains outstanding in the
// copied range.
func (m *defaultMemoryCopyMiddleware) processDeferredD2H() bool {
	d := m.pendingD2H
	if m.driver.uvm.hasPendingWorkInRange(d.pid, d.start, d.size) {
		return false
	}
	m.pendingD2H = nil
	m.pendingD2HFlushed = false
	m.readManagedD2H(d.cmd, d.queue)
	return true
}

// rangeHasManagedPages reports whether any page in [start, start+size) belongs
// to a managed allocation.
func (m *defaultMemoryCopyMiddleware) rangeHasManagedPages(
	pid vm.PID,
	start, size uint64,
) bool {
	addr := start
	end := start + size
	for addr < end {
		page, found := m.driver.pageTable.Find(pid, addr)
		if !found {
			panic("page not found")
		}
		if page.Managed {
			return true
		}
		next := page.VAddr + page.PageSize
		if next <= addr {
			return false
		}
		addr = next
	}
	return false
}
