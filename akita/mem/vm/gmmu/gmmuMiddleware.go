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
		madeProgress = m.parseFromTop() || madeProgress
	}

	return madeProgress
}

// sbin_codex: advance independent cache lookups and latency countdowns.
func (m *middleware) walkPageTable() bool {
	numActiveTranslations := len(m.walkingTranslations)
	madeProgress := false
	tmp := m.walkingTranslations[:0]

	for i := 0; i < numActiveTranslations; i++ {
		trans := &m.walkingTranslations[i]
		if trans.waitingOnUVM { // sbin_codex: skip translations parked on a UVM fault.
			tmp = append(tmp, *trans)
			continue
		}
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
	if page.Managed && m.needsUVMFault(page) {
		return m.sendUVMFault(walkingIndex)
	}

	return m.doPageWalkHit(walkingIndex)
}

// needsUVMFault reports whether a managed page translation must be gated on a
// UVM page fault instead of being returned.
func (m *middleware) needsUVMFault(page vm.Page) bool {
	if page.IsMigrating {
		return true
	}
	// CPU-resident managed page not yet remotely accessible: fault.
	if page.DeviceID == 0 && !page.RemoteAccessible {
		return true
	}
	// CPU-resident but remotely accessible: remote access, no fault.
	return false
}

// sendUVMFault forwards a managed-page fault to the driver UVM manager and
// parks the translation transaction.
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
	req.WaitRequestID = trans.req.ID

	if err := m.uvmPort.Send(req); err != nil {
		return false
	}
	trans.waitingOnUVM = true
	return true
}

// processUVMFaultRsp completes translations parked on a UVM page fault once
// the driver reports the fault serviced.
func (m *middleware) processUVMFaultRsp() bool {
	if m.uvmPort == nil {
		return false
	}
	rsp := m.uvmPort.PeekIncoming()
	if rsp == nil {
		return false
	}
	uvmRsp, ok := rsp.(*vm.PageFaultRsp)
	if !ok {
		return false
	}

	for i := range m.walkingTranslations {
		trans := &m.walkingTranslations[i]
		if !trans.waitingOnUVM || trans.req.ID != uvmRsp.RespondTo {
			continue
		}
		if !m.topPort.CanSend() {
			return false
		}
		page, found := m.pageTable.Find(trans.req.PID, trans.req.VAddr)
		if !found {
			panic("page not found after UVM fault")
		}
		trans.page = page
		m.uvmPort.RetrieveIncoming()
		trans.waitingOnUVM = false
		m.doPageWalkHit(i)
		return true
	}
	return false
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
func (m *middleware) parseFromTop() bool {
	if len(m.walkingTranslations) >= m.maxRequestsInFlight {
		return false
	}

	req := m.topPort.RetrieveIncoming()
	if req == nil {
		return false
	}

	tracing.TraceReqReceive(req, m.Comp)

	switch req := req.(type) {
	case *vm.TranslationReq:
		m.startWalking(req)
	default:
		log.Panicf("GMMU cannot handle request of type %s", reflect.TypeOf(req))
	}

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
