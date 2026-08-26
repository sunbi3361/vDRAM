package gmmu

import (
	"log"
	"reflect"
	"strconv"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/pagewalkcache"
	"github.com/sarchlab/akita/v4/tracing"
)

type middleware struct {
	*Comp
}

// Tick defines how the gmmu update state each cycle
func (m *middleware) Tick() bool {
	// sbin_codex: paused and flushing GMMUs retain all in-flight state.
	if m.state == gmmuStatePause || m.state == gmmuStateFlush {
		return false
	}

	madeProgress := false

	madeProgress = m.walkPageTable() || madeProgress
	madeProgress = m.parseFromPageWalkCache() || madeProgress
	madeProgress = m.processUVMFaultRsp() || madeProgress
	if m.state == gmmuStateEnable { // sbin_codex: drain retires work without admitting more.
		// sbin_claude_avatar: cancels land before admission so a cancel
		// processed this cycle can drop a request retrieved this cycle.
		madeProgress = m.processCancelPort() || madeProgress
		madeProgress = m.parseFromTop() || madeProgress
	}

	return madeProgress
}

// processCancelPort drains out-of-band walk cancels (Avatar EAF). A cancel
// whose request already occupies a walker slot is ignored - the walk
// finishes and its response is dropped upstream. Otherwise the request is
// still queued in the top port and its ID is remembered so parseFromTop
// drops it at retrieve time. // sbin_claude_avatar
func (m *middleware) processCancelPort() bool {
	madeProgress := false

	for {
		item := m.cancelPort.PeekIncoming()
		if item == nil {
			return madeProgress
		}

		cancel, ok := item.(*vm.TranslationCancelReq)
		if !ok {
			log.Panicf("GMMU cancel port cannot handle message of type %T",
				item)
		}

		m.cancelPort.RetrieveIncoming()

		if !m.isWalking(cancel.CancelID) {
			m.canceledReqs[cancel.CancelID] = struct{}{}
		}

		madeProgress = true
	}
}

// isWalking reports whether the named request already holds a walker slot.
// sbin_claude_avatar
func (m *middleware) isWalking(reqID string) bool {
	for i := range m.walkingTranslations {
		if m.walkingTranslations[i].req.ID == reqID {
			return true
		}
	}

	return false
}

// sbin_codex: advance independent cache lookups and latency countdowns.
func (m *middleware) walkPageTable() bool {
	numActiveTranslations := len(m.walkingTranslations)
	madeProgress := false
	tmp := m.walkingTranslations[:0]

	for i := 0; i < numActiveTranslations; i++ {
		trans := &m.walkingTranslations[i]
		// Pre-edit code (commented per AGENTS.md convention). Parked faults
		// used to stay in this slice and be skipped every cycle:
		// if trans.waitingOnUVM {
		// 	tmp = append(tmp, *trans)
		// 	continue
		// }
		switch trans.state {
		case newTransaction:
			madeProgress = m.sendToPageWalkCache(i) || madeProgress
		case pageWalkCacheDone:
			madeProgress = m.advancePageWalk(i) || madeProgress
		case fillingPageWalkCache:
			madeProgress = m.fillPageWalkCache(i) || madeProgress
		case pageWalkComplete:
			madeProgress = m.finalizePageWalk(i) || madeProgress
		}
		if trans.state != transactionFinished {
			tmp = append(tmp, *trans)
		}
	}

	m.walkingTranslations = tmp
	return madeProgress
}

// sbin_codex: pagewalkcache uses its own typed LookupRsp protocol.
func (m *middleware) parseFromPageWalkCache() bool {
	item := m.pageWalkCachePort.PeekIncoming()
	if item == nil {
		return false
	}

	rsp, ok := item.(*pagewalkcache.LookupRsp)
	if !ok {
		log.Panicf("GMMU cannot handle page-walk-cache message of type %T", item)
	}

	m.pageWalkCachePort.RetrieveIncoming()
	return m.handlePageWalkCacheResponse(rsp)
}

// sbin_codex: issue one aggregate lookup for all page-walk-cache levels.
func (m *middleware) sendToPageWalkCache(i int) bool {
	trans := &m.walkingTranslations[i]
	if trans.state != newTransaction {
		panic("this state shouldn't be here!")
	}
	if !m.pageWalkCachePort.CanSend() {
		return false
	}

	lookup := pagewalkcache.LookupReqBuilder{}.
		WithSrc(m.pageWalkCachePort.AsRemote()).
		WithDst(m.pageWalkCache.AsRemote()).
		WithPID(trans.req.PID).
		WithVAddr(trans.req.VAddr).
		Build()
	if err := m.pageWalkCachePort.Send(lookup); err != nil {
		return false
	}

	trans.msgID = lookup.ID
	trans.state = sentToPageWalkCache
	return true
}

// sbin_codex: aggregate lookup latency is followed by the remaining walk.
func (m *middleware) advancePageWalk(i int) bool {
	trans := &m.walkingTranslations[i]
	if trans.state != pageWalkCacheDone {
		panic("this state shouldn't be here!")
	}
	if trans.cycleLeft > 0 {
		trans.cycleLeft--
		return true
	}

	trans.state = fillingPageWalkCache
	return m.fillPageWalkCache(i)
}

// sbin_codex: install every cacheable level resolved by the modeled walk.
func (m *middleware) fillPageWalkCache(i int) bool {
	trans := &m.walkingTranslations[i]
	madeProgress := false
	for trans.fillLevel >= lowestPageWalkCacheLevel {
		if !m.pageWalkCachePort.CanSend() {
			return madeProgress
		}
		fill := pagewalkcache.FillReqBuilder{}.
			WithSrc(m.pageWalkCachePort.AsRemote()).
			WithDst(m.pageWalkCache.AsRemote()).
			WithPID(trans.req.PID).
			WithVAddr(trans.req.VAddr).
			WithLevel(trans.fillLevel).
			Build()
		if err := m.pageWalkCachePort.Send(fill); err != nil {
			return madeProgress
		}
		trans.fillLevel--
		madeProgress = true
	}
	trans.state = pageWalkComplete
	return true
}

// sbin_codex: one response reports the deepest cache hit; GMMU then walks only
// the uncached lower levels and fills cacheable levels reached by that walk.
func (m *middleware) handlePageWalkCacheResponse(
	rsp *pagewalkcache.LookupRsp,
) bool {
	for i := range m.walkingTranslations {
		trans := &m.walkingTranslations[i]
		if trans.msgID != rsp.RspTo || trans.state != sentToPageWalkCache {
			continue
		}

		if rsp.Hit {
			if rsp.Level < lowestPageWalkCacheLevel || rsp.Level >= pageTableLevels {
				panic("page-walk-cache returned an invalid hit level")
			}
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.req, m.Comp),
				m.Comp,
				"pwc-hit-level"+strconv.Itoa(rsp.Level),
			)

			trans.level = rsp.Level - 1
			trans.cycleLeft = uint64(trans.level+1) * uint64(m.latency)
			trans.fillLevel = trans.level
			trans.state = pageWalkCacheDone

			return true
		}

		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.req, m.Comp),
			m.Comp,
			"pwc-miss-level"+strconv.Itoa(trans.level),
		)
		trans.cycleLeft = uint64(trans.level+1) * uint64(m.latency)
		trans.fillLevel = trans.level
		trans.state = pageWalkCacheDone
		return true
	}

	return false
}

func (m *middleware) finalizePageWalk(
	walkingIndex int,
) bool {
	req := m.walkingTranslations[walkingIndex].req
	page, found := m.pageTable.Find(req.PID, req.VAddr)

	if !found {
		panic("page not found")
	}

	m.walkingTranslations[walkingIndex].page = page

	// sbin_codex: UVM demand-fault gating for managed pages.
	if page.Managed && m.needsUVMFault(page, req) {
		return m.sendUVMFault(walkingIndex)
	}

	// if page.Managed && page.DeviceID == 0 && page.RemoteAccessible { // sbin_codex
	// 	m.countRemoteAccess(page, req)
	// }

	return m.doPageWalkHit(walkingIndex)
}

// needsUVMFault reports whether a managed page translation must be gated on a
// UVM page fault instead of being returned. Every access to a
// remotely-accessible CPU-resident page, including writes, is served as a
// remote access; migration is driven by the access-counter threshold. // sbin_codex
func (m *middleware) needsUVMFault(page vm.Page, req *vm.TranslationReq) bool {
	if page.IsMigrating {
		return true
	}
	// CPU-resident managed page not yet remotely accessible: fault.
	if page.DeviceID == 0 && !page.RemoteAccessible {
		return true
	}
	// sbin_codex: writes to remotely-accessible pages are served remotely;
	// migration is driven by the PCIe accesscounter threshold.
	//
	// Old immediate-write gating, preserved for reference:
	// if page.DeviceID == 0 && req.IsWrite {
	// 	return true
	// }
	// CPU-resident and remotely accessible: remote access, no fault.
	return false
}

// func (m *middleware) countRemoteAccess(page vm.Page, req *vm.TranslationReq) {} // sbin_codex
// func (m *middleware) resetAccessCounter(pid vm.PID, regionBase uint64) {} // sbin_codex

// sendUVMFault forwards a managed-page fault to the GPU control endpoint and
// moves the translation into the replay queue. The transaction leaves
// walkingTranslations so a fault storm cannot consume every page-walk slot.
// sbin_codex
func (m *middleware) sendUVMFault(walkingIndex int) bool {
	if m.uvmPort == nil || m.UVMServiceProvider == "" {
		panic("GMMU encountered a managed-page fault without a UVM service provider")
	}
	if !m.uvmPort.CanSend() {
		return false
	}

	trans := &m.walkingTranslations[walkingIndex]
	req := vm.NewPageFaultReq(m.uvmPort.AsRemote(), m.UVMServiceProvider)
	req.PID = trans.req.PID
	req.VAddr = trans.req.VAddr
	req.DeviceID = trans.req.DeviceID
	req.IsWrite = trans.req.IsWrite
	req.WaitRequestID = trans.req.ID

	if err := m.uvmPort.Send(req); err != nil {
		return false
	}

	m.replayQueue = append(m.replayQueue, *trans)
	trans.state = transactionFinished

	return true
}

// processUVMFaultRsp releases translations that were stalled on an unresolved
// UVM mapping. Two forms are accepted: a per-request PageFaultRsp and the
// region-scoped UVMFaultReplayReq that the driver issues once a 64KB service
// transaction becomes replayable. // sbin_codex
func (m *middleware) processUVMFaultRsp() bool {
	if m.uvmPort == nil {
		return false
	}

	msg := m.uvmPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *vm.PageFaultRsp:
		return m.replayOneRequest(msg)
	case *vm.UVMFaultReplayReq:
		return m.replayRange(msg)
	default:
		return false
	}
}

// replayOneRequest re-runs the translation of a single stalled request.
func (m *middleware) replayOneRequest(rsp *vm.PageFaultRsp) bool {
	for i := range m.replayQueue {
		if m.replayQueue[i].req.ID != rsp.RespondTo {
			continue
		}

		if !m.topPort.CanSend() {
			return false
		}

		trans := m.replayQueue[i]
		m.replayQueue = append(m.replayQueue[:i], m.replayQueue[i+1:]...)
		m.uvmPort.RetrieveIncoming()

		// A per-request response answers this exact request, so it is
		// completed without re-checking the mapping. // sbin_codex
		m.resumeTranslation(trans, true, "")

		return true
	}

	// The request is unknown; drop the message so the port does not deadlock.
	m.uvmPort.RetrieveIncoming()

	return true
}

// replayRange re-runs every stalled translation inside the serviced region.
// One replay command releases the whole coalesced fault batch.
func (m *middleware) replayRange(req *vm.UVMFaultReplayReq) bool {
	end := req.StartVA + req.Size

	for i := range m.replayQueue {
		trans := m.replayQueue[i]
		if trans.req.PID != req.PID || trans.refaultedBy == req.ID {
			continue
		}

		if trans.req.VAddr < req.StartVA || trans.req.VAddr >= end {
			continue
		}

		if !m.topPort.CanSend() {
			return false
		}

		m.replayQueue = append(m.replayQueue[:i], m.replayQueue[i+1:]...)
		m.resumeTranslation(trans, req.Refused, req.ID)

		return true
	}

	m.uvmPort.RetrieveIncoming()

	return true
}

// resumeTranslation re-reads the page table and completes a released
// translation.
//
// The mapping is re-checked rather than trusted. A replay names a range, not a
// request, so it can reach a translation whose page has since been parked
// again — an eviction that started right behind the admission is enough. Such
// a translation faults once more instead of being answered with a mapping that
// is on its way out. A refused replay is the exception: the driver has said the
// region will not become GPU-local, so the request must complete against host
// memory instead of faulting in a loop that nothing would break. // sbin_codex
func (m *middleware) resumeTranslation(
	trans transaction,
	completeUnchecked bool,
	replayID string,
) {
	page, found := m.pageTable.Find(trans.req.PID, trans.req.VAddr)
	if !found {
		panic("page not found after UVM fault")
	}

	trans.page = page
	trans.state = pageWalkComplete

	m.walkingTranslations = append(m.walkingTranslations, trans)
	index := len(m.walkingTranslations) - 1

	// Pre-edit code (commented per AGENTS.md convention):
	// m.doPageWalkHit(len(m.walkingTranslations) - 1)
	if !completeUnchecked && page.Managed && m.needsUVMFault(page, trans.req) {
		m.walkingTranslations[index].refaultedBy = replayID
		m.sendUVMFault(index)
	} else {
		m.doPageWalkHit(index)
	}

	tmp := m.walkingTranslations[:0]
	for _, t := range m.walkingTranslations {
		if t.state != transactionFinished {
			tmp = append(tmp, t)
		}
	}

	m.walkingTranslations = tmp
}

func (m *middleware) doPageWalkHit(
	walkingIndex int,
) bool {
	if !m.topPort.CanSend() {
		return false
	}

	walking := m.walkingTranslations[walkingIndex]
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(walking.req.Src).
		WithRspTo(walking.req.ID).
		WithPage(walking.page).
		Build()

	if err := m.topPort.Send(rsp); err != nil {
		return false
	}
	m.walkingTranslations[walkingIndex].state = transactionFinished // sbin_codex

	tracing.TraceReqComplete(walking.req, m.Comp)

	return true
}

// Pre-edit code (commented per project convention). Admission used to be
// gated on walker slots before looking at the message:
// func (m *middleware) parseFromTop() bool {
// 	if len(m.walkingTranslations) >= m.maxRequestsInFlight {
// 		return false
// 	}
//
// 	req := m.topPort.RetrieveIncoming()
// 	if req == nil {
// 		return false
// 	}
//
// 	tracing.TraceReqReceive(req, m.Comp)
//
// 	switch req := req.(type) {
// 	case *vm.TranslationReq:
// 		m.startWalking(req)
// 	default:
// 		log.Panicf("GMMU cannot handle request of type %s", reflect.TypeOf(req))
// 	}
//
// 	return true
// }
//
// sbin_claude_avatar: a canceled request is dropped at the queue head even
// when every walker slot is busy - it needs no slot, and leaving it would
// clog the queue behind it. The drop happens before TraceReqReceive, so
// canceled walks never enter the gmmu_translation_count/inflight metrics.
func (m *middleware) parseFromTop() bool {
	item := m.topPort.PeekIncoming()
	if item == nil {
		return false
	}

	req, ok := item.(*vm.TranslationReq)
	if !ok {
		log.Panicf("GMMU cannot handle request of type %s",
			reflect.TypeOf(item))
	}

	if _, canceled := m.canceledReqs[req.ID]; canceled {
		delete(m.canceledReqs, req.ID)
		m.topPort.RetrieveIncoming()

		return true
	}

	if len(m.walkingTranslations) >= m.maxRequestsInFlight {
		return false
	}

	m.topPort.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.Comp)
	m.startWalking(req)

	return true
}

func (m *middleware) startWalking(req *vm.TranslationReq) {
	// sbin_codex: initialize a root-level lookup; cache misses set latency.
	trans := transaction{
		req:       req,
		level:     pageTableLevels - 1,
		fillLevel: -1,
		msgID:     "invalid",
		state:     newTransaction,
	}

	m.walkingTranslations = append(m.walkingTranslations, trans)
}
