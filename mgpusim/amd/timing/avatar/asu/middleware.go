// sbin_claude_avatar
package asu

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

type middleware struct {
	*Comp
}

// Tick advances every in-flight miss and the three ports. The forward leg
// and the speculation leg of a transaction progress independently
// (refs 5.12).
func (m *middleware) Tick() bool {
	madeProgress := false

	madeProgress = m.advanceTransactions() || madeProgress
	// sbin_claude_avatar v3: no validation port to drain - the speculative
	// access is the requester's own demand access (refs 5.3, 5.6).
	madeProgress = m.parseFromBottom() || madeProgress
	madeProgress = m.parseFromTop() || madeProgress

	return madeProgress
}

// advanceTransactions steps both legs of every transaction and compacts the
// finished ones.
func (m *middleware) advanceTransactions() bool {
	madeProgress := false
	tmp := m.transactions[:0]

	for i := 0; i < len(m.transactions); i++ {
		trans := &m.transactions[i]

		madeProgress = m.advanceSpeculation(trans) || madeProgress
		madeProgress = m.respondEarly(trans) || madeProgress
		madeProgress = m.sendWalkCancel(trans) || madeProgress // sbin_claude_avatar v2
		madeProgress = m.relayRealRsp(trans) || madeProgress
		madeProgress = m.forwardToL2TLB(trans) || madeProgress

		if !trans.done {
			tmp = append(tmp, *trans)
		}
	}

	m.transactions = tmp

	return madeProgress
}

// Pre-edit v2 code (commented per project convention): issueValidationRead
// sent a separate 64B mem.ReadReq at the speculated address through the
// L1ToL2 network and the transaction waited for it. refs 5.3/5.6 give CAST
// no such fetch - the speculative access is the data access the requester
// was already making - so charging one made every speculation pay a second
// full trip through L2/DRAM, and it also had to be aimed at a sector the ASU
// cannot see (the translation request carries only the page ID), which put
// every one of those reads on the single L2 bank that owns 4KB-aligned
// addresses. Removed in v3. // sbin_claude_avatar

// advanceSpeculation counts down CAVA's decompress-and-compare overhead.
// When it completes, CAVA reads the authoritative metadata (refs 5.6): only
// a compressible sector whose embedded (PID, VPN) matches the request
// validates the speculation.
//
// Pre-edit v2 behavior (commented per project convention): the countdown ran
// only after the ASU's own sector fetch returned. v3 starts it when the miss
// is admitted, because the sector it rides on is the requester's demand
// access - the CU issues that access off the speculated PPN without waiting
// for anyone, so the only thing standing between the prediction and the
// early fill is decompressing the sector and comparing its embedded VPN
// (-avatar-validation-latency). // sbin_claude_avatar v3
func (m *middleware) advanceSpeculation(trans *transaction) bool {
	// if !trans.specActive {
	// 	return false
	// }
	if !trans.specActive || !trans.specCountdown {
		return false
	}

	if trans.specCycleLeft > 0 {
		trans.specCycleLeft--
		return true
	}

	trans.specActive = false
	trans.specCountdown = false

	verdict := m.registry.Validate(
		trans.specPAddr, trans.req.PID, trans.req.VAddr)
	switch verdict {
	case meta.VerdictPass:
		m.stats.CAVAPass++
		m.armEarlyResponse(trans)
	case meta.VerdictMismatch:
		// The embedded VPN exposes the mis-speculation; the speculative
		// sector is discarded and the conventional translation decides
		// (refs 5.6 Case A mismatch, 5.8). No data was made visible.
		m.stats.CAVAMismatch++
	case meta.VerdictIncompressible:
		// Case B: present but unguaranteed. Without guarantee-bit caches
		// the model conservatively waits for the real translation.
		m.stats.CAVAIncompressible++
	case meta.VerdictNoMetadata:
		m.stats.CAVANoMetadata++
	}

	return true
}

// armEarlyResponse cross-checks a validated speculation against the current
// page table before making it architecturally visible (refs 5.8: no
// instruction may consume data from an incorrect PPN). A mapping that
// changed between speculation launch and validation vetoes the early
// completion; the real translation still answers the request.
func (m *middleware) armEarlyResponse(trans *transaction) {
	page, found := m.pageTable.Find(trans.req.PID, trans.req.VAddr)
	if !found || !page.Valid || page.PAddr != trans.specPAddr {
		m.stats.PageTableVetoes++
		return
	}

	trans.earlyPending = true
	trans.earlyPage = page
}

// respondEarly answers the L1 TLB with the validated translation before the
// conventional path returns: the L1 TLB fills the entry and releases its
// MSHR, modeling Early TLB Fill (refs 5.9). The conventional walk is now
// redundant: a forward that has not left yet is suppressed outright,
// otherwise the walk-cancel leg is armed (avatar-plan.md 5.2). The
// validated page also trains the MOD, since a canceled walk produces no
// training response. // sbin_claude_avatar v2
func (m *middleware) respondEarly(trans *transaction) bool {
	if !trans.earlyPending {
		return false
	}

	if !m.topPort.CanSend() {
		return false
	}

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(trans.req.Src).
		WithRspTo(trans.req.ID).
		WithPage(trans.earlyPage).
		Build()

	if err := m.topPort.Send(rsp); err != nil {
		return false
	}

	trans.earlyPending = false
	trans.earlyDone = true
	m.stats.EarlyCompletions++
	m.trainMOD(trans, trans.earlyPage) // sbin_claude_avatar v2
	tracing.TraceReqComplete(trans.req, m.Comp)

	// sbin_claude_avatar v2: retire the conventional leg.
	if !trans.fwdSent {
		m.stats.ForwardsSuppressed++
		trans.done = true
	} else {
		trans.cancelPending = true
	}

	return true
}

// sendWalkCancel tells the L2 TLB to abandon the forwarded translation an
// EAF just made redundant. The transaction closes once the cancel is out;
// a response that still arrives (the cancel lost the race) is dropped as an
// orphan. // sbin_claude_avatar v2
func (m *middleware) sendWalkCancel(trans *transaction) bool {
	if !trans.cancelPending {
		return false
	}

	if !m.bottomPort.CanSend() {
		return false
	}

	// Pre-edit code (commented per project convention): the cancel carried
	// no translation, so the L2 TLB dropped the walk without learning the
	// mapping and every other CU that wanted the page had to walk again.
	// cancel := vm.TranslationCancelReqBuilder{}.
	// 	WithSrc(m.bottomPort.AsRemote()).
	// 	WithDst(m.l2TLBCancelPort).
	// 	WithCancelID(trans.fwdID).
	// 	WithVAddr(trans.req.VAddr).
	// 	WithPID(trans.req.PID).
	// 	Build()
	//
	// sbin_claude_avatar: refs 5.9 steps 5-7 - the EAF translation fills the
	// shared TLB and is forwarded to the CUs waiting on the same VPN.
	cancel := vm.TranslationCancelReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.l2TLBCancelPort).
		WithCancelID(trans.fwdID).
		WithVAddr(trans.req.VAddr).
		WithPID(trans.req.PID).
		WithPage(trans.earlyPage).
		Build()

	if err := m.bottomPort.Send(cancel); err != nil {
		return false
	}

	trans.cancelPending = false
	trans.done = true
	m.stats.WalkCancelsSent++

	return true
}

// relayRealRsp sends a buffered real translation response up to the L1 TLB.
func (m *middleware) relayRealRsp(trans *transaction) bool {
	if trans.realRsp == nil || trans.earlyDone {
		return false
	}

	if !m.topPort.CanSend() {
		return false
	}

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(trans.req.Src).
		WithRspTo(trans.req.ID).
		WithPage(trans.realRsp.Page).
		Build()

	if err := m.topPort.Send(rsp); err != nil {
		return false
	}

	trans.realRsp = nil
	trans.done = true
	tracing.TraceReqComplete(trans.req, m.Comp)

	return true
}

// forwardToL2TLB hands the miss to the conventional L2-TLB/page-walk path.
// The normal translation launches in parallel with the speculation
// (refs 5.3) unless the EAF already answered the request - then the forward
// is pointless and respondEarly suppresses it. // sbin_claude_avatar v2
func (m *middleware) forwardToL2TLB(trans *transaction) bool {
	if trans.fwdSent {
		return false
	}

	// sbin_claude_avatar v2: a validated speculation waiting only for the
	// top port needs no conventional walk anymore.
	if trans.earlyPending || trans.earlyDone {
		return false
	}

	if !m.bottomPort.CanSend() {
		return false
	}

	fwd := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.l2TLBPort).
		WithVAddr(trans.req.VAddr).
		WithPID(trans.req.PID).
		WithDeviceID(trans.req.DeviceID).
		WithIsWrite(trans.req.IsWrite).
		Build()

	if err := m.bottomPort.Send(fwd); err != nil {
		return false
	}

	trans.fwdID = fwd.ID
	trans.fwdSent = true
	m.stats.Forwarded++

	return true
}

// parseFromBottom drains real translation responses, up to numReqPerCycle
// per tick so the ASU does not throttle the baseline translation path.
func (m *middleware) parseFromBottom() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		if !m.parseOneFromBottom() {
			break
		}
		madeProgress = true
	}

	return madeProgress
}

// parseOneFromBottom matches one real translation response with its
// transaction, trains the MOD, and either relays the translation or
// swallows it when the request already completed early (refs 5.12: no
// duplicate completion).
func (m *middleware) parseOneFromBottom() bool {
	item := m.bottomPort.PeekIncoming()
	if item == nil {
		return false
	}

	rsp, ok := item.(*vm.TranslationRsp)
	if !ok {
		log.Panicf("ASU cannot handle bottom message of type %T", item)
	}

	for i := range m.transactions {
		trans := &m.transactions[i]
		if !trans.fwdSent || trans.fwdID != rsp.RespondTo {
			continue
		}

		m.bottomPort.RetrieveIncoming()

		m.trainMOD(trans, rsp.Page)

		if trans.earlyDone {
			// EAF already answered the L1 TLB; the late walk result only
			// contributes MOD training. The pending cancel became
			// pointless: the walk is over. // sbin_claude_avatar v2
			trans.cancelPending = false
			m.stats.SwallowedRsps++
			trans.done = true

			return true
		}

		// The real translation wins the race: cancel the speculation leg.
		// sbin_claude_avatar
		if trans.specActive || trans.earlyPending {
			trans.specActive = false
			trans.specCountdown = false // sbin_claude_avatar v3
			trans.earlyPending = false
			m.stats.RealResponseFirst++
		}

		trans.realRsp = rsp

		return true
	}

	// Pre-edit code (commented per project convention):
	// log.Panicf("ASU received a response for an unknown request %s",
	// 	rsp.RespondTo)
	//
	// sbin_claude_avatar v2: a canceled walk can still answer when the
	// cancel lost the race; its transaction already closed.
	m.bottomPort.RetrieveIncoming()
	m.stats.OrphanRsps++

	return true
}

// trainMOD feeds a completed real translation into the requester's MOD
// (refs 5.2).
func (m *middleware) trainMOD(trans *transaction, page vm.Page) {
	if !page.Valid {
		return
	}

	offset := int64(page.PAddr) - int64(page.VAddr)
	m.modOf(trans.req.Src).train(trans.req.PID, trans.req.VAddr, offset)
}

// parseFromTop admits L1 TLB misses, up to numReqPerCycle per tick.
func (m *middleware) parseFromTop() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		if !m.parseOneFromTop() {
			break
		}
		madeProgress = true
	}

	return madeProgress
}

// parseOneFromTop admits one L1 TLB miss: the request is always forwarded
// to the conventional path, and a confident MOD prediction additionally
// launches the speculative access (refs 5.3).
func (m *middleware) parseOneFromTop() bool {
	if len(m.transactions) >= m.maxReqInFlight {
		return false
	}

	item := m.topPort.PeekIncoming()
	if item == nil {
		return false
	}

	req, ok := item.(*vm.TranslationReq)
	if !ok {
		log.Panicf("ASU cannot handle request of type %s",
			reflect.TypeOf(item))
	}

	m.topPort.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.Comp)

	trans := transaction{req: req}

	// Pre-edit v1 code (commented per project convention):
	// offset, confident := m.modOf(req.Src).predict(req.PID, req.VAddr)
	// if confident {
	// 	trans.specActive = true
	// 	trans.specCycleLeft = m.validationLatency
	// 	trans.specPAddr = uint64(int64(req.VAddr) + offset)
	// 	m.stats.Speculations++
	// }
	//
	// sbin_claude_avatar v2: the speculative access is a real sector fetch;
	// a prediction outside the GPU DRAM range is dropped before it can leak
	// onto the remote-access route.
	offset, confident := m.modOf(req.Src).predict(req.PID, req.VAddr)
	if confident {
		specPAddr := uint64(int64(req.VAddr) + offset)
		if specPAddr < m.memLow || specPAddr >= m.memHigh {
			m.stats.SpecOutOfRange++
		} else {
			trans.specActive = true
			trans.specPAddr = specPAddr
			// sbin_claude_avatar v3: the requester's demand access carries
			// the sector, so only the decompress-and-compare is timed.
			trans.specCountdown = true
			trans.specCycleLeft = m.validationLatency
			m.stats.Speculations++
		}
	}

	m.transactions = append(m.transactions, trans)

	return true
}
