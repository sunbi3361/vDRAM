package tlb

import (
	"github.com/sarchlab/akita/v4/mem/vm"
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
	// sbin_claude_avatar: cancels are drained first so a released MSHR entry
	// can be reused within the same cycle's lookups.
	madeProgress = m.processCancels() || madeProgress
	madeProgress = m.sendPendingBottomCancels() || madeProgress
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
	// sbin_claude_avatar: keep draining cancels so canceled requests cannot
	// hold the MSHR open forever.
	madeProgress = m.processCancels() || madeProgress
	madeProgress = m.sendPendingBottomCancels() || madeProgress
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

// processCancels drains the out-of-band cancel port (Avatar EAF,
// refs/avatar.md 5.9). Cancellation is best effort: a request that already
// answered is silently left alone. // sbin_claude_avatar
func (m *tlbMiddleware) processCancels() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		item := m.cancelPort.PeekIncoming()
		if item == nil {
			break
		}

		cancel, ok := item.(*vm.TranslationCancelReq)
		if !ok {
			panic("TLB cancel port can only receive TranslationCancelReq")
		}

		m.cancelPort.RetrieveIncoming()
		m.handleTranslationCancel(cancel)
		madeProgress = true
	}

	return madeProgress
}

// handleTranslationCancel removes the named request from its MSHR entry. An
// entry left without waiters is released - the late walk response is then
// dropped by the pre-existing parseBottom orphan path - and the downstream
// walker is told to abandon the walk. A request that has not reached the
// MSHR yet is remembered and dropped when it emerges from the lookup
// pipeline. // sbin_claude_avatar
func (m *tlbMiddleware) handleTranslationCancel(
	cancel *vm.TranslationCancelReq,
) {
	entry := m.mshr.GetEntry(cancel.PID, cancel.VAddr)
	if entry != nil {
		for i, waiting := range entry.Requests {
			if waiting.ID != cancel.CancelID {
				continue
			}

			entry.Requests = append(
				entry.Requests[:i], entry.Requests[i+1:]...)

			if len(entry.Requests) == 0 {
				m.queueBottomCancel(entry)
				m.mshr.Remove(cancel.PID, cancel.VAddr)
			}

			return
		}
	}

	// The request is still in the lookup pipeline (it cannot arrive after
	// the cancel: both travel the same connection in order), or it already
	// completed; in the latter case this entry stays unused.
	m.pendingCancels[cancel.CancelID] = struct{}{}
}

// queueBottomCancel arranges for the downstream walker to abandon the walk
// that belonged to a fully-canceled MSHR entry. // sbin_claude_avatar
func (m *tlbMiddleware) queueBottomCancel(entry *mshrEntry) {
	if m.walkCancelDst == "" || entry.reqToBottom == nil {
		return
	}

	cancel := vm.TranslationCancelReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.walkCancelDst).
		WithCancelID(entry.reqToBottom.ID).
		WithVAddr(entry.vAddr).
		WithPID(entry.pid).
		Build()

	m.pendingBottomCancels = append(m.pendingBottomCancels, cancel)
}

// sendPendingBottomCancels forwards queued downstream cancels when the
// bottom port allows. // sbin_claude_avatar
func (m *tlbMiddleware) sendPendingBottomCancels() bool {
	madeProgress := false

	for len(m.pendingBottomCancels) > 0 {
		cancel := m.pendingBottomCancels[0]
		if err := m.bottomPort.Send(cancel); err != nil {
			break
		}

		m.pendingBottomCancels = m.pendingBottomCancels[1:]
		madeProgress = true
	}

	return madeProgress
}

func (m *tlbMiddleware) lookup(req *vm.TranslationReq) bool {
	// sbin_claude_avatar: a canceled request is dropped as it emerges from
	// the pipeline - no MSHR entry, no walk, no response. The requester
	// abandoned it (Avatar EAF answered it early).
	if _, canceled := m.pendingCancels[req.ID]; canceled {
		delete(m.pendingCancels, req.ID)
		return true
	}

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

	tracing.TraceReqReceive(req, m.Comp)
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, m.Comp), m.Comp, "mshr-hit")

	return true
}

func (m *tlbMiddleware) fetchBottom(req *vm.TranslationReq) bool {
	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		WithIsWrite(req.IsWrite). // sbin_codex: propagate write intent to the GMMU.
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

	mshrEntry := m.mshr.Add(req.PID, req.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom

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
	// setID := m.vAddrToSetID(page.VAddr) // sbin_codex: pre-edit unconditional admission.
	// set := m.sets[setID]
	// wayID, ok := m.sets[setID].Evict()
	// if !ok { panic("failed to evict") }
	// set.Update(wayID, page)
	// set.Visit(wayID)
	mshrEntry := m.mshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)

	// sbin_codex: a range invalidation that landed while this lookup was in
	// flight makes the returned page stale. Answer the waiters but do not
	// install the entry, so the next access re-walks the page table.
	if m.pageAdmissionPredicate(page) && !mshrEntry.staleOnFill { // sbin_codex: admission affects storage only.
		setID := m.vAddrToSetID(page.VAddr)
		set := m.sets[setID]
		wayID, ok := set.Evict()
		if !ok {
			panic("failed to evict")
		}
		set.Update(wayID, page)
		set.Visit(wayID)
	}

	m.respondingMSHREntry = mshrEntry
	mshrEntry.page = page

	m.mshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	m.bottomPort.RetrieveIncoming()
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, m.Comp)

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
	// sbin_claude_avatar: a flush abandons every in-flight lookup, so the
	// cancel bookkeeping that refers to them is stale too.
	m.pendingCancels = make(map[string]struct{})
	m.pendingBottomCancels = nil

	m.inflightFlushReq = nil
	m.state = tlbStatePause

	return true
}
