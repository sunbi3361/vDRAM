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

	m.bottomCommitsThisCycle = 0 // sbin_claude_vc

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

	// Pre-edit code (commented per project convention). One shared pipeline
	// was ticked here:
	//
	// madeProgress = m.responsePipeline.Tick() || madeProgress
	//
	// sbin_claude_vc: every top channel owns a pipeline now.
	for _, channel := range m.topChannels {
		madeProgress = channel.pipeline.Tick() || madeProgress
	}

	madeProgress = m.insertIntoPipeline() || madeProgress

	return madeProgress
}

// get req from port buffer and insert into pipeline
//
// Pre-edit code (commented per project convention). Only the single top port
// was drained:
//
//	for i := 0; i < m.numReqPerCycle; i++ {
//		if !m.responsePipeline.CanAccept() { break }
//		req := m.topPort.RetrieveIncoming()
//		if req == nil { break }
//		m.responsePipeline.Accept(&pipelineTLBReq{req: req.(*vm.TranslationReq)})
//		madeProgress = true
//	}
//
// sbin_claude_vc: each channel is drained into its own pipeline, so a channel
// whose pipeline is backed up does not stop the others from being admitted.
func (m *tlbMiddleware) insertIntoPipeline() bool {
	madeProgress := false

	for _, channel := range m.topChannels {
		for i := 0; i < m.numReqPerCycle; i++ {
			if !channel.pipeline.CanAccept() {
				break
			}

			req := channel.port.RetrieveIncoming()
			if req == nil {
				break
			}

			channel.pipeline.Accept(&pipelineTLBReq{
				req: req.(*vm.TranslationReq),
			})

			madeProgress = true
		}
	}

	return madeProgress
}

func (m *tlbMiddleware) extractFromPipeline() bool {
	madeProgress := false

	// sbin_claude_vc: a lookup that cannot be answered holds up only its own
	// channel. Pre-edit, every channel shared m.responseBuffer, so one
	// unanswerable lookup at its head stalled every other requester too.
	for _, channel := range m.topChannels {
		for i := 0; i < m.numReqPerCycle; i++ {
			item := channel.buffer.Peek()

			if item == nil {
				break
			}

			req := item.(*pipelineTLBReq).req

			ok := m.lookup(req, channel)
			if ok {
				channel.buffer.Pop()

				madeProgress = true
			}
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
	madeProgress = m.respondTop() || madeProgress // sbin_claude_vc

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
	madeProgress = m.respondTop() || madeProgress // sbin_claude_vc

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseBottom() || madeProgress
	}

	madeProgress = m.processPipeline() || madeProgress

	// sbin_claude_vc: answers already queued for a channel must go out before
	// the TLB is considered drained.
	if m.mshr.IsEmpty() && !m.hasPendingTopRsp() &&
		m.bottomPort.PeekIncoming() == nil {
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

// respondTop hands out the answers this cycle allows. With a single channel
// this is the pre-existing responding-MSHR-entry register; with more than one
// channel every channel drains its own queue independently. // sbin_claude_vc
func (m *tlbMiddleware) respondTop() bool {
	madeProgress := false

	if !m.isMultiChannel() {
		for i := 0; i < m.numReqPerCycle; i++ {
			madeProgress = m.respondMSHREntry() || madeProgress
		}

		return madeProgress
	}

	for _, channel := range m.topChannels {
		for i := 0; i < m.numReqPerCycle; i++ {
			madeProgress = m.respondPending(channel) || madeProgress
		}
	}

	return madeProgress
}

// respondPending sends one queued answer out of a channel's own port. A
// channel that cannot send blocks nothing but itself. // sbin_claude_vc
func (m *tlbMiddleware) respondPending(channel *topChannel) bool {
	if len(channel.pending) == 0 {
		return false
	}

	pending := channel.pending[0]
	req := pending.req
	rspToTop := vm.TranslationRspBuilder{}.
		WithSrc(channel.port.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(pending.page).
		Build()

	err := channel.port.Send(rspToTop)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		channel.port.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	channel.pending = channel.pending[1:]

	tracing.TraceReqComplete(req, m.Comp)

	return true
}

// queueChannelResponses splits one completed page walk across the channels
// that are waiting for it, so each waiter is answered on the port - and
// therefore the connection - its request arrived on. // sbin_claude_vc
func (m *tlbMiddleware) queueChannelResponses(entry *mshrEntry, page vm.Page) {
	entry.page = page

	for _, req := range entry.Requests {
		channel := m.channelOf(req)
		channel.pending = append(channel.pending,
			pendingTopRsp{req: req, page: page})
	}
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

		// Pre-edit code (commented per project convention): the cancel was
		// always consumed, because a plain cancel can never fail.
		// m.cancelPort.RetrieveIncoming()
		// m.handleTranslationCancel(cancel)
		//
		// sbin_claude_avatar: an Early-TLB-Fill cancel also answers the
		// other waiters, so it can be held off by the response register and
		// must stay on the port until it lands.
		if !m.handleTranslationCancel(cancel) {
			break
		}

		m.cancelPort.RetrieveIncoming()
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
) bool {
	entry := m.mshr.GetEntry(cancel.PID, cancel.VAddr)
	if entry != nil {
		for i, waiting := range entry.Requests {
			if waiting.ID != cancel.CancelID {
				continue
			}

			// sbin_claude_avatar: an Early TLB Fill carries the validated
			// translation, and refs/avatar.md 5.9 steps 5-7 require filling
			// the shared TLB with it and forwarding it to the other CUs
			// waiting on the same VPN - not throwing it away.
			if cancel.Page.Valid {
				return m.fillFromEarlyTLBFill(entry, i, cancel)
			}

			entry.Requests = append(
				entry.Requests[:i], entry.Requests[i+1:]...)

			if len(entry.Requests) == 0 {
				m.queueBottomCancel(entry)
				// Pre-edit code (commented per project convention):
				// m.mshr.Remove(cancel.PID, cancel.VAddr)
				//
				// sbin_claude_softwalker: a canceled In-TLB entry must
				// return its reserved way to the replacement rotation.
				removed := m.mshr.Remove(cancel.PID, cancel.VAddr)
				if removed.inTLB {
					m.sets[removed.setID].Release(removed.wayID)
				}
			}

			return true
		}
	}

	// The request is still in the lookup pipeline (it cannot arrive after
	// the cancel: both travel the same connection in order), or it already
	// completed; in the latter case this entry stays unused.
	m.pendingCancels[cancel.CancelID] = struct{}{}

	return true
}

// fillFromEarlyTLBFill applies an Avatar Early TLB Fill (refs/avatar.md 5.9
// steps 5-7): the validated page is installed in the shared TLB and handed
// to every other requester coalesced on the same VPN, and only then is the
// now-redundant walk retired. Without this, an EAF discarded the translation
// and each of those CUs had to walk the page table again.
//
// idx names the canceling requester's slot; it was already answered by the
// EAF and must not be answered twice. Returns false when the fill cannot be
// committed this cycle, leaving the cancel on the port to be retried.
// sbin_claude_avatar
func (m *tlbMiddleware) fillFromEarlyTLBFill(
	entry *mshrEntry,
	idx int,
	cancel *vm.TranslationCancelReq,
) bool {
	// Everyone but the canceling requester still needs an answer; it was
	// already served by the EAF and must not be answered twice.
	others := len(entry.Requests) - 1

	// Answering them spends the same commit budget a page-walk response
	// would: one fill per cycle, and the single-channel path has only one
	// response register. Checked before anything is mutated, so a refusal
	// simply leaves the cancel on the port for the next cycle.
	if others > 0 {
		if m.isMultiChannel() {
			if m.bottomCommitsThisCycle > 0 {
				return false
			}
		} else if m.respondingMSHREntry != nil {
			return false
		}
	}

	entry.Requests = append(entry.Requests[:idx], entry.Requests[idx+1:]...)

	m.installFilledPage(entry, cancel.Page)

	if others > 0 {
		if m.isMultiChannel() {
			m.queueChannelResponses(entry, cancel.Page)
			m.bottomCommitsThisCycle++
		} else {
			m.respondingMSHREntry = entry
			entry.page = cancel.Page
		}
	}

	// The walk this entry was waiting on is now redundant for every one of
	// its requesters.
	m.queueBottomCancel(entry)
	m.mshr.Remove(cancel.PID, cancel.VAddr)

	return true
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

// sbin_claude_vc: lookup answers on the channel the request arrived on.
func (m *tlbMiddleware) lookup(
	req *vm.TranslationReq,
	channel *topChannel,
) bool {
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
		return m.handleTranslationHit(req, setID, wayID, page, channel)
	}
	return m.handleTranslationMiss(req)
}

func (m *tlbMiddleware) handleTranslationHit(
	req *vm.TranslationReq,
	setID, wayID int,
	page vm.Page,
	channel *topChannel, // sbin_claude_vc
) bool {
	ok := m.sendRspToTop(req, page, channel)
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

// mshrCanAccept reports whether the MSHR can track a new miss for the
// request. The LATC MSHR accepts a miss that can be compressed into an
// existing group even when every group entry is taken. // sbin_claude_latpc
func (m *tlbMiddleware) mshrCanAccept(req *vm.TranslationReq) bool {
	if latc, ok := m.mshr.(*latcMSHR); ok {
		return latc.CanAccept(req)
	}

	return !m.mshr.IsFull()
}

// mshrAdd tracks a new outstanding miss, compressed when LATC is enabled.
// sbin_claude_latpc
func (m *tlbMiddleware) mshrAdd(req *vm.TranslationReq) *mshrEntry {
	if latc, ok := m.mshr.(*latcMSHR); ok {
		return latc.AddCompressed(req)
	}

	return m.mshr.Add(req.PID, req.VAddr)
}

func (m *tlbMiddleware) handleTranslationMiss(
	req *vm.TranslationReq,
) bool {
	// Pre-edit code (commented per project convention):
	// if m.mshr.IsFull() {
	// 	return false
	// }
	//
	// sbin_claude_softwalker: with the dedicated MSHR full, In-TLB MSHR
	// (SoftWalker, MICRO'25 4.5) repurposes a way of the miss's own set as a
	// temporary MSHR slot instead of refusing the miss.
	inTLB := false
	var setID, wayID int

	if !m.mshrCanAccept(req) {
		if m.mshr.IsFull() && m.inTLBMSHRMax > 0 {
			var ok bool
			setID, wayID, ok = m.pickInTLBWay(req)
			if !ok {
				m.reservationFailureCount++
				return false
			}
			inTLB = true
		} else {
			m.reservationFailureCount++
			return false
		}
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindHardwareResource,
		m.Comp.Name()+".MSHR",
		m.Comp.Name(),
		m.Comp,
	)

	fetched := m.fetchBottom(req, inTLB, setID, wayID)
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

// pickInTLBWay names the way an In-TLB MSHR allocation for req would occupy,
// committing nothing: the reservation happens only after the bottom-side
// fetch is actually sent. Refusal means the mechanism is disabled, the
// global cap is reached, or every way of the miss's set is already reserved
// (the paper's per-set contention). sbin_claude_softwalker
func (m *tlbMiddleware) pickInTLBWay(
	req *vm.TranslationReq,
) (setID, wayID int, ok bool) {
	if m.inTLBMSHRMax == 0 {
		return 0, 0, false
	}

	if m.mshr.InTLBCount() >= m.inTLBMSHRMax {
		m.inTLBRefuseCapFull++
		return 0, 0, false
	}

	setID = m.vAddrToSetID(req.VAddr)

	wayID, ok = m.sets[setID].PeekEvict()
	if !ok {
		m.inTLBRefuseSetFull++
		return 0, 0, false
	}

	return setID, wayID, true
}

// ReservationFailureCount returns how many lookup attempts were blocked
// because the MSHR could not accept the miss - one count per blocked attempt,
// i.e. per stall cycle at the head of a channel. // sbin_claude_latpc
func (c *Comp) ReservationFailureCount() uint64 {
	return c.reservationFailureCount
}

// CompressedMSHREnabled reports whether this TLB uses the LATC compressed
// MSHR. // sbin_claude_latpc
func (c *Comp) CompressedMSHREnabled() bool {
	_, ok := c.mshr.(*latcMSHR)
	return ok
}

// LATCStats returns the compressed MSHR's counters: group entries allocated
// and misses compressed into an existing group. Zeros when LATC is off.
// sbin_claude_latpc
func (c *Comp) LATCStats() (groupsAllocated, coalescedSubentries uint64) {
	latc, ok := c.mshr.(*latcMSHR)
	if !ok {
		return 0, 0
	}

	return latc.groupsAllocated, latc.coalescedSubentries
}

func (m *tlbMiddleware) vAddrToSetID(vAddr uint64) (setID int) {
	return int(vAddr / m.pageSize % uint64(m.numSets))
}

// Pre-edit code (commented per project convention). The answer always left
// through the single top port:
//
//	rsp := vm.TranslationRspBuilder{}.WithSrc(m.topPort.AsRemote())...
//	err := m.topPort.Send(rsp)
//
// sbin_claude_vc: it leaves through the channel the request came in on.
func (m *tlbMiddleware) sendRspToTop(
	req *vm.TranslationReq,
	page vm.Page,
	channel *topChannel,
) bool {
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(channel.port.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		Build()

	err := channel.port.Send(rsp)
	if err == nil {
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(req, m.Comp),
			tracing.MilestoneKindNetworkBusy,
			channel.port.Name(),
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

// Pre-edit signature (commented per project convention):
// func (m *tlbMiddleware) fetchBottom(req *vm.TranslationReq) bool {
//
// sbin_claude_softwalker: the caller decides whether the entry lives in the
// dedicated MSHR or in a repurposed TLB way; the reservation is committed
// only once the bottom-side fetch has actually left.
func (m *tlbMiddleware) fetchBottom(
	req *vm.TranslationReq,
	inTLB bool,
	setID, wayID int,
) bool {
	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		WithIsWrite(req.IsWrite). // sbin_codex: propagate write intent to the GMMU.
		// sbin_claude_latpc: propagate the LATPC group triple so the L2 TLB
		// and the GMMU can batch same-group walks.
		WithGroup(req.GroupID, req.GroupStride, req.GroupIndex).
		// sbin_claude_avatar: the ASU sits below this TLB, so the PC that
		// Avatar's MOD is indexed by only reaches it on the miss this call
		// forwards. req is the request that opened the MSHR entry, which is
		// the miss the paper's MOD probe is attached to (refs/avatar.md 5.3
		// probes on an L1 TLB miss); later requests coalesced onto the same
		// entry never reach the ASU and so never probe or train.
		WithInstPC(req.InstPC).
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

	// Pre-edit code (commented per project convention):
	// mshrEntry := m.mshr.Add(req.PID, req.VAddr)
	//
	// sbin_claude_softwalker
	var mshrEntry *mshrEntry
	if inTLB {
		m.sets[setID].Reserve(wayID)
		mshrEntry = m.mshr.AddInTLB(req.PID, req.VAddr, setID, wayID)
		m.inTLBAllocCount++
	} else {
		mshrEntry = m.mshrAdd(req)
	}
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom

	tracing.TraceReqInitiate(fetchBottom, m.Comp,
		tracing.MsgIDAtReceiver(req, m.Comp))

	return true
}

func (m *tlbMiddleware) parseBottom() bool {
	// Pre-edit code (commented per project convention):
	// if m.respondingMSHREntry != nil {
	// 	return false
	// }
	//
	// sbin_claude_vc: the multi-channel path queues answers per channel
	// instead of parking them in one responding-entry register, because a
	// register shared by both classes lets a stalled class hold up the fills
	// the other class needs. The one-fill-per-cycle rate the register
	// enforced is kept by bottomCommitsThisCycle, checked below.
	if !m.isMultiChannel() && m.respondingMSHREntry != nil {
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

	// sbin_claude_vc: cap the fill rate at the one-per-cycle the
	// responding-entry register used to impose. Nothing has been mutated
	// yet, so returning here simply retries next cycle.
	if m.isMultiChannel() && m.bottomCommitsThisCycle > 0 {
		return false
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
	//
	// sbin_claude_softwalker: an In-TLB entry owns a reserved way already -
	// the fill lands there instead of evicting a second way. A dedicated
	// entry whose set has every way reserved skips installation: the waiters
	// are still answered and the next access re-walks.
	// sbin_claude_avatar: the install block moved into installFilledPage so
	// the Early-TLB-Fill cancel path installs on exactly the same terms.
	m.installFilledPage(mshrEntry, page)

	// Pre-edit code (commented per project convention):
	// m.respondingMSHREntry = mshrEntry
	// mshrEntry.page = page
	if m.isMultiChannel() { // sbin_claude_vc
		m.queueChannelResponses(mshrEntry, page)
		m.bottomCommitsThisCycle++
	} else {
		m.respondingMSHREntry = mshrEntry
		mshrEntry.page = page
	}

	m.mshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	m.bottomPort.RetrieveIncoming()
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, m.Comp)

	return true
}

func (m *tlbMiddleware) visit(setID, wayID int) {
	set := m.sets[setID]
	set.Visit(wayID)
}

// installFilledPage installs a returned translation into the TLB storage. It
// carries the pre-existing parseBottom rules: an In-TLB entry fills its own
// reserved way, a range invalidation that raced the lookup (staleOnFill) or
// a page the admission predicate rejects is answered but never stored, and a
// set with no evictable way is only fatal when In-TLB MSHRs are disabled.
// sbin_claude_avatar
func (m *tlbMiddleware) installFilledPage(entry *mshrEntry, page vm.Page) {
	if entry.inTLB {
		m.fillInTLBEntry(entry, page)
		return
	}

	if !m.pageAdmissionPredicate(page) || entry.staleOnFill { // sbin_codex: admission affects storage only.
		return
	}

	setID := m.vAddrToSetID(page.VAddr)
	set := m.sets[setID]

	wayID, ok := set.Evict()
	if !ok && m.inTLBMSHRMax == 0 { // sbin_claude_softwalker
		panic("failed to evict")
	}

	if ok {
		set.Update(wayID, page)
		set.Visit(wayID)
	}
}

// fillInTLBEntry returns an In-TLB entry's reserved way to the replacement
// rotation and installs the fill into it, unless a raced invalidation or the
// admission predicate forbids installation - then the way is released empty
// and reused naturally from its old LRU position. sbin_claude_softwalker
func (m *tlbMiddleware) fillInTLBEntry(entry *mshrEntry, page vm.Page) {
	set := m.sets[entry.setID]
	set.Release(entry.wayID)

	if m.pageAdmissionPredicate(page) && !entry.staleOnFill {
		set.Update(entry.wayID, page)
		set.Visit(entry.wayID)
	}
}

func (m *tlbMiddleware) handleFlush() bool {
	if m.inflightFlushReq == nil {
		return false
	}

	madeProgress := false

	// sbin_claude_vc: queued per-channel answers count as in-flight work too.
	if m.mshr.IsEmpty() && m.respondingMSHREntry == nil &&
		!m.hasPendingTopRsp() && m.bottomPort.PeekIncoming() == nil {
		madeProgress = m.processTLBFlush() || madeProgress
		return madeProgress
	}

	madeProgress = m.respondTop() || madeProgress // sbin_claude_vc

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

	// sbin_claude_softwalker: reserved In-TLB ways must return to the
	// replacement rotation before their entries are dropped, or the ways
	// would be lost to the eviction rotation forever.
	for _, entry := range m.mshr.AllEntries() {
		if entry.inTLB {
			m.sets[entry.setID].Release(entry.wayID)
		}
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
