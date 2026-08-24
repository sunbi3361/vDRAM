package tlb

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/tracing"
)

type pipelineTLBReq struct {
	req *vm.TranslationReq
}

func (r *pipelineTLBReq) TaskID() string {
	return r.req.ID
}

type tlbMiddleware struct {
	*Comp
}

func (m *tlbMiddleware) Tick() bool {
	madeProgress := false

	switch m.state {
	case tlbStateDrain:
		madeProgress = m.handleDrain() || madeProgress
	case tlbStatePause:
		return false
	case tlbStateFlush:
		madeProgress = m.handleFlush() || madeProgress
	default:
		madeProgress = m.handleEnable() || madeProgress
	}
	return madeProgress
}

func (m *tlbMiddleware) processPipeline() bool {
	madeProgress := false

	madeProgress = m.extractFromPipeline() || madeProgress

	madeProgress = m.responsePipeline.Tick() || madeProgress

	madeProgress = m.insertIntoPipeline() || madeProgress

	return madeProgress
}

// get req from port buffer and insert into pipeline
func (m *tlbMiddleware) insertIntoPipeline() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		if !m.responsePipeline.CanAccept() {
			break
		}

		req := m.topPort.RetrieveIncoming()
		if req == nil {
			break
		}

		m.responsePipeline.Accept(&pipelineTLBReq{
			req: req.(*vm.TranslationReq),
		})

		madeProgress = true
	}

	return madeProgress
}

func (m *tlbMiddleware) extractFromPipeline() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		item := m.responseBuffer.Peek()

		if item == nil {
			break
		}

		req := item.(*pipelineTLBReq).req

		ok := m.lookup(req)
		if ok {
			m.responseBuffer.Pop()

			madeProgress = true
		}
	}

	return madeProgress
}

func (m *tlbMiddleware) handleEnable() bool {
	madeProgress := false
	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.respondMSHREntry() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseBottom() || madeProgress
	}

	madeProgress = m.processPipeline() || madeProgress

	return madeProgress
}

func (m *tlbMiddleware) handleDrain() bool {
	madeProgress := false
	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.respondMSHREntry() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseBottom() || madeProgress
	}

	madeProgress = m.processPipeline() || madeProgress

	if m.mshr.IsEmpty() && m.bottomPort.PeekIncoming() == nil {
		m.state = tlbStatePause
		tracing.AddMilestone(
			m.Comp.Name()+".drain",
			tracing.MilestoneKindHardwareResource,
			m.Comp.Name()+".MSHR",
			m.Comp.Name(),
			m.Comp,
		)
	}

	return madeProgress
}

func (m *tlbMiddleware) respondMSHREntry() bool {
	if m.respondingMSHREntry == nil {
		return false
	}
	mshrEntry := m.respondingMSHREntry
	page := mshrEntry.page
	req := mshrEntry.Requests[0]
	rspToTop := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		Build()

	err := m.topPort.Send(rspToTop)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.topPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	mshrEntry.Requests = mshrEntry.Requests[1:]
	if len(mshrEntry.Requests) == 0 {
		m.respondingMSHREntry = nil
	}

	tracing.TraceReqComplete(req, m.Comp)

	return true
}

func (m *tlbMiddleware) lookup(req *vm.TranslationReq) bool {
	mshrEntry := m.mshr.GetEntry(req.PID, req.VAddr)
	if mshrEntry != nil {
		return m.processTLBMSHRHit(mshrEntry, req)
	}
	setID := m.vAddrToSetID(req.VAddr)
	set := m.sets[setID]
	wayID, page, found := set.Lookup(req.PID, req.VAddr)

	if found && page.Valid {
		return m.handleTranslationHit(req, setID, wayID, page)
	}
	return m.handleTranslationMiss(req)
}

func (m *tlbMiddleware) handleTranslationHit(
	req *vm.TranslationReq,
	setID, wayID int,
	page vm.Page,
) bool {
	ok := m.sendRspToTop(req, page)
	if !ok {
		return false
	}
	m.visit(setID, wayID)

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindData,
		m.Comp.Name()+".Sets",
		m.Comp.Name(),
		m.Comp,
	)

	tracing.TraceReqReceive(req, m.Comp)
	tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, m.Comp), m.Comp, "hit")
	tracing.TraceReqComplete(req, m.Comp)

	return true
}

func (m *tlbMiddleware) handleTranslationMiss(
	req *vm.TranslationReq,
) bool {
	if m.mshr.IsFull() {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindHardwareResource,
		m.Comp.Name()+".MSHR",
		m.Comp.Name(),
		m.Comp,
	)

	fetched := m.fetchBottom(req)
	if fetched {
		tracing.TraceReqReceive(req, m.Comp)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, m.Comp),
			m.Comp,
			"miss",
		)

		return true
	}
	return false
}

func (m *tlbMiddleware) vAddrToSetID(vAddr uint64) (setID int) {
	return int(vAddr / m.pageSize % uint64(m.numSets))
}

func (m *tlbMiddleware) sendRspToTop(
	req *vm.TranslationReq,
	page vm.Page,
) bool {
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		Build()

	err := m.topPort.Send(rsp)
	if err == nil {
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(req, m.Comp),
			tracing.MilestoneKindNetworkBusy,
			m.topPort.Name(),
			m.Comp.Name(),
			m.Comp,
		)
	}
	return err == nil
}

func (m *tlbMiddleware) processTLBMSHRHit(
	mshrEntry *mshrEntry,
	req *vm.TranslationReq,
) bool {
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	// sbin_codex: leaf data translation points count late MSHR waiters;
	// shared L2 never counts its own forwarding request.
	if m.isLeafDataTranslationPoint {
		mshrEntry.waiterDelta.LateMSHRWaiters++
	}

	// sbin_codex: UVM fault-pending replay (plan todo 7 of
	// mgpusim-uvm-manager). A request that hits a fault-pending entry
	// re-fetches from the bottom so the replay can complete the translation;
	// the original waiters stay retained in the MSHR until the valid response
	// arrives. The append is rolled back on failure so a retry does not
	// double-count the waiter.
	if mshrEntry.faultPending {
		ok := m.refetchBottom(mshrEntry, req)
		if !ok {
			mshrEntry.Requests = mshrEntry.Requests[:len(mshrEntry.Requests)-1]
			if m.isLeafDataTranslationPoint {
				mshrEntry.waiterDelta.LateMSHRWaiters--
			}
			return false
		}
	}

	tracing.TraceReqReceive(req, m.Comp)
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, m.Comp), m.Comp, "mshr-hit")

	return true
}

// refetchBottom re-issues the translation request for a fault-pending MSHR
// entry. The re-fetch carries the GMMU fault-pending token (the request's own
// token when present, otherwise the token recorded on the entry) so the
// translation provider can match it to the pending fault transaction. // sbin_codex
func (m *tlbMiddleware) refetchBottom(
	mshrEntry *mshrEntry,
	req *vm.TranslationReq,
) bool {
	token := req.FaultPendingToken
	if token == 0 {
		token = mshrEntry.faultPendingToken
	}
	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		WithFaultPendingToken(token).
		WithWaiterDelta(mshrEntry.waiterDelta).
		Build()

	err := m.bottomPort.Send(fetchBottom)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.bottomPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	mshrEntry.reqToBottom = fetchBottom

	tracing.TraceReqInitiate(fetchBottom, m.Comp,
		tracing.MsgIDAtReceiver(req, m.Comp))

	return true
}

func (m *tlbMiddleware) fetchBottom(req *vm.TranslationReq) bool {
	// sbin_codex: UVM waiter accounting and fault-pending token propagation
	// (plan todo 7 of mgpusim-uvm-manager). Leaf data translation points
	// report the initial waiter count (1 for the first request); shared L2
	// propagates the delta it received and never counts its own forwarding
	// request.
	delta := req.WaiterDelta
	if m.isLeafDataTranslationPoint {
		delta = vm.WaiterDelta{InitialWaiters: 1}
	}
	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		WithFaultPendingToken(req.FaultPendingToken). // sbin_codex
		WithWaiterDelta(delta).                       // sbin_codex
		Build()

	// fetchBottom := vm.TranslationReqBuilder{}. // sbin_codex: pre-edit
	// 	WithSrc(m.bottomPort.AsRemote()).
	// 	WithDst(m.addressMapper.Find(req.VAddr)).
	// 	WithPID(req.PID).
	// 	WithVAddr(req.VAddr).
	// 	WithDeviceID(req.DeviceID).
	// 	Build()

	err := m.bottomPort.Send(fetchBottom)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.bottomPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	mshrEntry := m.mshr.Add(req.PID, req.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom
	mshrEntry.waiterDelta = delta // sbin_codex

	tracing.TraceReqInitiate(fetchBottom, m.Comp,
		tracing.MsgIDAtReceiver(req, m.Comp))

	return true
}

func (m *tlbMiddleware) parseBottom() bool {
	if m.respondingMSHREntry != nil {
		return false
	}
	item := m.bottomPort.PeekIncoming()
	if item == nil {
		return false
	}

	rsp := item.(*vm.TranslationRsp)
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(rsp, m.Comp),
		tracing.MilestoneKindData,
		m.bottomPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)
	page := rsp.Page

	mshrEntryPresent := m.mshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	if !mshrEntryPresent {
		m.bottomPort.RetrieveIncoming()
		return true
	}
	mshrEntry := m.mshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)

	// sbin_codex: UVM fault-pending handling (plan todo 7 of
	// mgpusim-uvm-manager). A fault-pending response propagates the GMMU
	// token and the original waiter counts up, retains the MSHR entry, and
	// never installs an entry in the set.
	if rsp.FaultPendingToken != 0 && !rsp.Page.Valid {
		if mshrEntry.faultPending {
			m.bottomPort.RetrieveIncoming()
			return true
		}
		ok := m.sendFaultPendingRspToTop(mshrEntry, rsp)
		if !ok {
			return false
		}
		mshrEntry.faultPending = true
		mshrEntry.faultPendingToken = rsp.FaultPendingToken
		m.bottomPort.RetrieveIncoming()
		return true
	}

	// sbin_codex: reject invalid managed translations without negative caching
	// (uvm-manager.md §600-610). The MSHR retains the original waiters; no
	// invalid entry is installed in the set.
	if !page.Valid &&
		(page.Managed || page.Location != vm.MemoryLocationUNMANAGED) {
		m.bottomPort.RetrieveIncoming()
		return true
	}

	setID := m.vAddrToSetID(page.VAddr)
	set := m.sets[setID]
	wayID, ok := m.sets[setID].Evict()

	if !ok {
		panic("failed to evict")
	}

	set.Update(wayID, page)
	set.Visit(wayID)

	m.respondingMSHREntry = mshrEntry
	mshrEntry.page = page

	m.mshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	m.bottomPort.RetrieveIncoming()
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, m.Comp)

	return true
}

// sendFaultPendingRspToTop propagates the fault-pending response to the first
// retained waiter, carrying the GMMU token and the original waiter counts
// observed at this translation point. The MSHR entry stays retained. // sbin_codex
func (m *tlbMiddleware) sendFaultPendingRspToTop(
	mshrEntry *mshrEntry,
	rsp *vm.TranslationRsp,
) bool {
	req := mshrEntry.Requests[0]
	rspToTop := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(rsp.Page).
		WithLocation(rsp.Location).
		WithFaultPendingToken(rsp.FaultPendingToken).
		WithWaiterDelta(mshrEntry.waiterDelta).
		Build()

	err := m.topPort.Send(rspToTop)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(rsp, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.topPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	return true
}

func (m *tlbMiddleware) visit(setID, wayID int) {
	set := m.sets[setID]
	set.Visit(wayID)
}

func (m *tlbMiddleware) handleFlush() bool {
	if m.inflightFlushReq == nil {
		return false
	}

	madeProgress := false

	if m.mshr.IsEmpty() && m.respondingMSHREntry == nil && m.bottomPort.PeekIncoming() == nil {
		madeProgress = m.processTLBFlush() || madeProgress
		return madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.respondMSHREntry() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseBottom() || madeProgress
	}

	madeProgress = m.processPipeline() || madeProgress

	return madeProgress
}

func (m *tlbMiddleware) processTLBFlush() bool {
	req := m.inflightFlushReq

	rsp := FlushRspBuilder{}.
		WithSrc(m.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	err := m.controlPort.Send(rsp)
	if err != nil {
		return false
	}
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.controlPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	for _, vAddr := range req.VAddr {
		setID := m.vAddrToSetID(vAddr)
		set := m.sets[setID]
		wayID, page, found := set.Lookup(req.PID, vAddr)

		if !found {
			continue
		}
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(req, m.Comp),
			tracing.MilestoneKindDependency,
			m.Comp.Name()+".Sets",
			m.Comp.Name(),
			m.Comp,
		)
		page.Valid = false
		set.Update(wayID, page)
	}

	m.mshr.Reset()

	m.inflightFlushReq = nil
	m.state = tlbStatePause

	return true
}

// sbin_codex: handleTLBInvalidate processes a UVM range TLB invalidation
// (plan todo 14 of mgpusim-uvm-manager). Every set entry for the matching
// PID/ASID whose covered VA range overlaps the requested region is
// invalidated; unrelated entries, MSHRs, and in-flight progress are
// untouched. The TLB state never changes (no pause), and exactly one
// aggregated response is returned (uvm-manager.md §21.1).
func (c *Comp) handleTLBInvalidate(req *UVMTLBInvalidateReq) bool {
	if !c.controlPort.CanSend() {
		return false
	}
	rsp := UVMTLBInvalidateRspBuilder{}.
		WithSrc(c.controlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()
	if err := c.controlPort.Send(rsp); err != nil {
		return false
	}
	for _, set := range c.sets {
		internal.InvalidateRange(set, req.PID, req.StartVA, req.Size)
	}
	c.controlPort.RetrieveIncoming()

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, c),
		tracing.MilestoneKindNetworkBusy,
		c.controlPort.Name(),
		c.Name(),
		c,
	)

	return true
}
