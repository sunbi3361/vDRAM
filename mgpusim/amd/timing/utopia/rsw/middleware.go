// sbin_claude_utopia
package rsw

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/tracing"
)

type middleware struct {
	*Comp
}

// Tick advances every in-flight RestSeg walk and the two ports.
func (m *middleware) Tick() bool {
	madeProgress := false

	madeProgress = m.advanceWalks() || madeProgress
	madeProgress = m.parseFromBottom() || madeProgress
	madeProgress = m.parseFromTop() || madeProgress

	return madeProgress
}

// advanceWalks steps the per-transaction state machine and compacts finished
// transactions, mirroring the GMMU walk loop structure.
func (m *middleware) advanceWalks() bool {
	madeProgress := false
	tmp := m.transactions[:0]

	for i := 0; i < len(m.transactions); i++ {
		trans := &m.transactions[i]

		switch trans.state {
		case stateSFAccess:
			madeProgress = m.advanceSFAccess(trans) || madeProgress
		case stateTARAccess:
			madeProgress = m.advanceTARAccess(trans) || madeProgress
		case stateRespond:
			madeProgress = m.respond(trans) || madeProgress
		case stateForward:
			madeProgress = m.forwardToFlexSeg(trans) || madeProgress
		case stateWaitingFSW, stateFinished:
			// Waiting transactions progress in parseFromBottom.
		}

		if trans.state != stateFinished {
			tmp = append(tmp, *trans)
		}
	}

	m.transactions = tmp

	return madeProgress
}

// advanceSFAccess counts down the Set Filter access. When it completes, the
// SF value decides whether the TAR is consulted at all (utopia.md 4.4).
func (m *middleware) advanceSFAccess(trans *transaction) bool {
	if trans.cycleLeft > 0 {
		trans.cycleLeft--
		return true
	}

	if m.registry.SFCount(int(m.deviceID), trans.req.VAddr) == 0 {
		// The hashed set holds no valid way: the page cannot be in the
		// RestSeg, so the TAR tag match is skipped entirely.
		m.stats.SFFiltered++
		m.stats.FlexSegWalks++
		trans.state = stateForward

		return m.forwardToFlexSeg(trans)
	}

	m.startTARAccess(trans)

	return true
}

// startTARAccess models the TAR lookup. The TAR cache was probed in parallel
// with the SF access (utopia.md 4.5), so on a TAR cache hit only the latency
// not already hidden behind the SF access remains. A TAR cache miss fetches
// the metadata line with the configured memory latency.
func (m *middleware) startTARAccess(trans *transaction) {
	set := m.segmentConfigs()[0].SetOf(trans.req.VAddr)

	if m.tarCache.lookup(set) {
		m.stats.TARCacheHits++
		remaining := m.tarHitLatency - trans.sfLatency
		if remaining < 0 {
			remaining = 0
		}
		trans.cycleLeft = remaining
	} else {
		m.stats.TARCacheMisses++
		m.tarCache.install(set)
		trans.cycleLeft = m.missLatency
	}

	trans.state = stateTARAccess
}

// advanceTARAccess counts down the TAR access, then resolves the walk against
// the authoritative TAR state.
func (m *middleware) advanceTARAccess(trans *transaction) bool {
	if trans.cycleLeft > 0 {
		trans.cycleLeft--
		return true
	}

	pAddr, found := m.registry.Lookup(
		int(m.deviceID), trans.req.PID, trans.req.VAddr)
	if !found {
		// NotInRestSeg: only now may the FlexSeg walk start (utopia.md 4.7).
		m.stats.RSWMisses++
		m.stats.FlexSegWalks++
		trans.state = stateForward

		return m.forwardToFlexSeg(trans)
	}

	m.stats.RSWHits++

	pageSize := m.segmentConfigs()[0].PageSize
	trans.page = vm.Page{
		PID:      trans.req.PID,
		VAddr:    trans.req.VAddr / pageSize * pageSize,
		PAddr:    pAddr,
		PageSize: pageSize,
		Valid:    true,
		DeviceID: m.deviceID,
	}
	trans.state = stateRespond

	return m.respond(trans)
}

// respond returns a resolved RestSeg translation to the L2 TLB, which fills
// it like any walker response.
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

// forwardToFlexSeg hands the translation to the GMMU for the conventional
// page walk and parks the transaction until the response returns.
func (m *middleware) forwardToFlexSeg(trans *transaction) bool {
	if !m.bottomPort.CanSend() {
		return false
	}

	fwd := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.flexSegWalker).
		WithVAddr(trans.req.VAddr).
		WithPID(trans.req.PID).
		WithDeviceID(trans.req.DeviceID).
		WithIsWrite(trans.req.IsWrite).
		Build()

	if err := m.bottomPort.Send(fwd); err != nil {
		return false
	}

	trans.forwardID = fwd.ID
	trans.state = stateWaitingFSW

	return true
}

// parseFromBottom matches GMMU responses with parked transactions and relays
// the translation to the L2 TLB.
func (m *middleware) parseFromBottom() bool {
	item := m.bottomPort.PeekIncoming()
	if item == nil {
		return false
	}

	rsp, ok := item.(*vm.TranslationRsp)
	if !ok {
		log.Panicf("UTU cannot handle bottom message of type %T", item)
	}

	for i := range m.transactions {
		trans := &m.transactions[i]
		if trans.state != stateWaitingFSW || trans.forwardID != rsp.RespondTo {
			continue
		}

		if !m.topPort.CanSend() {
			return false
		}

		m.bottomPort.RetrieveIncoming()

		trans.page = rsp.Page
		trans.state = stateRespond

		return m.respond(trans)
	}

	log.Panicf("UTU received a response for an unknown walk %s", rsp.RespondTo)

	return false
}

// parseFromTop admits new translation requests from the L2 TLB and starts the
// RestSeg walk.
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
		log.Panicf("UTU cannot handle request of type %s", reflect.TypeOf(item))
	}

	m.topPort.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.Comp)

	m.startWalk(req)

	return true
}

// startWalk begins a RestSeg walk, or passes the request straight to the
// GMMU when this GPU has no RestSeg configured.
func (m *middleware) startWalk(req *vm.TranslationReq) {
	configs := m.segmentConfigs()
	if len(configs) == 0 {
		m.stats.Passthrough++
		m.transactions = append(m.transactions, transaction{
			req:   req,
			state: stateForward,
		})

		return
	}

	set := configs[0].SetOf(req.VAddr)

	trans := transaction{
		req:   req,
		state: stateSFAccess,
	}

	if m.sfCache.lookup(set) {
		m.stats.SFCacheHits++
		trans.sfWasHit = true
		trans.sfLatency = m.sfHitLatency
	} else {
		m.stats.SFCacheMisses++
		m.sfCache.install(set)
		trans.sfLatency = m.missLatency
	}
	trans.cycleLeft = trans.sfLatency

	m.transactions = append(m.transactions, trans)
}
