// sbin_claude_fbt
package fbt

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/tracing"
)

type middleware struct {
	*Comp
}

// Tick advances every in-flight lookup and the two ports.
func (m *middleware) Tick() bool {
	madeProgress := false

	madeProgress = m.advanceLookups() || madeProgress
	madeProgress = m.parseFromBottom() || madeProgress
	madeProgress = m.parseFromTop() || madeProgress

	return madeProgress
}

// advanceLookups steps the per-transaction state machine and compacts
// finished transactions.
func (m *middleware) advanceLookups() bool {
	madeProgress := false
	tmp := m.transactions[:0]

	for i := 0; i < len(m.transactions); i++ {
		trans := &m.transactions[i]

		switch trans.state {
		case stateLookup:
			madeProgress = m.advanceLookup(trans) || madeProgress
		case stateRespond:
			madeProgress = m.respond(trans) || madeProgress
		case stateForward:
			madeProgress = m.forwardToWalker(trans) || madeProgress
		case stateWaitingWalk, stateFinished:
			// Parked transactions progress in parseFromBottom.
		}

		if trans.state != stateFinished {
			tmp = append(tmp, *trans)
		}
	}

	m.transactions = tmp

	return madeProgress
}

// advanceLookup counts down the table access. The latency is charged on both
// outcomes: the lookup happened either way, and a miss only learns so once it
// completes.
func (m *middleware) advanceLookup(trans *transaction) bool {
	if trans.cycleLeft > 0 {
		trans.cycleLeft--

		return true
	}

	page, found := m.table.lookup(trans.req.PID, m.pageIDOf(trans.req.VAddr))
	if !found {
		m.stats.Misses++
		trans.state = stateForward

		return m.forwardToWalker(trans)
	}

	m.stats.Hits++

	trans.page = page
	trans.state = stateRespond

	return m.respond(trans)
}

// respond returns a translation to the L2 TLB, which fills it like any
// walker response.
func (m *middleware) respond(trans *transaction) bool {
	if !m.topPort.CanSend() {
		return false
	}

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(trans.req.Src).
		WithRspTo(trans.req.ID).
		WithPage(trans.page).
		Build()

	if err := m.topPort.Send(rsp); err != nil {
		return false
	}

	trans.state = stateFinished
	tracing.TraceReqComplete(trans.req, m.Comp)

	return true
}

// forwardToWalker hands the translation to the page walker and parks the
// transaction until the response returns.
func (m *middleware) forwardToWalker(trans *transaction) bool {
	if !m.bottomPort.CanSend() {
		return false
	}

	fwd := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.pageWalker).
		WithVAddr(trans.req.VAddr).
		WithPID(trans.req.PID).
		WithDeviceID(trans.req.DeviceID).
		WithIsWrite(trans.req.IsWrite).
		Build()

	if err := m.bottomPort.Send(fwd); err != nil {
		return false
	}

	trans.forwardID = fwd.ID
	trans.state = stateWaitingWalk

	return true
}

// parseFromBottom matches walker responses with parked transactions, fills
// the table and relays the translation to the L2 TLB.
func (m *middleware) parseFromBottom() bool {
	item := m.bottomPort.PeekIncoming()
	if item == nil {
		return false
	}

	rsp, ok := item.(*vm.TranslationRsp)
	if !ok {
		log.Panicf("FBT cannot handle bottom message of type %T", item)
	}

	for i := range m.transactions {
		trans := &m.transactions[i]
		if trans.state != stateWaitingWalk || trans.forwardID != rsp.RespondTo {
			continue
		}

		if !m.topPort.CanSend() {
			return false
		}

		m.bottomPort.RetrieveIncoming()

		m.installPage(rsp.Page)

		trans.page = rsp.Page
		trans.state = stateRespond

		return m.respond(trans)
	}

	log.Panicf("FBT received a response for an unknown walk %s", rsp.RespondTo)

	return false
}

// installPage records a freshly walked mapping. The paper's table gains its
// entry when a page's data enters the virtual caches, which is what a
// completed walk is about to cause.
func (m *middleware) installPage(page vm.Page) {
	if !page.Valid {
		return
	}

	if m.table.install(page.PID, m.pageIDOf(page.VAddr), page) {
		m.stats.Evictions++
	}

	m.stats.Installs++
}

// parseFromTop admits new translation requests from the L2 TLB.
func (m *middleware) parseFromTop() bool {
	if len(m.transactions) >= m.maxReqInFlight {
		return false
	}

	item := m.topPort.PeekIncoming()
	if item == nil {
		return false
	}

	req, ok := item.(*vm.TranslationReq)
	if !ok {
		log.Panicf("FBT cannot handle request of type %s", reflect.TypeOf(item))
	}

	m.topPort.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.Comp)

	m.transactions = append(m.transactions, transaction{
		req:       req,
		state:     stateLookup,
		cycleLeft: m.lookupLatency,
	})

	return true
}
