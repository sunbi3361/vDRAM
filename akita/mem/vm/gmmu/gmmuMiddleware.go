package gmmu

import (
	"log"
	"reflect"
	"strconv"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/pagewalkcache"
	"github.com/sarchlab/akita/v4/sim"
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

	// sbin_codex: UVM replay commands, replay re-injection, and pending block
	// acknowledgements (todo 8 of mgpusim-uvm-manager).
	madeProgress = m.handleReplayRange() || madeProgress
	madeProgress = m.handleReplayTick() || madeProgress
	madeProgress = m.trySendBlockAcks() || madeProgress

	madeProgress = m.walkPageTable() || madeProgress
	madeProgress = m.parseFromPageWalkCache() || madeProgress
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

	// sbin_codex: a managed invalid translation is a typed fault owned by the
	// GMMU (todo 8 of mgpusim-uvm-manager). It replaces the downstream panic
	// on an unusable managed mapping.
	if !page.Valid &&
		(page.Managed || page.Location != vm.MemoryLocationUNMANAGED) {
		return m.doManagedFault(walkingIndex)
	}

	return m.doPageWalkHit(walkingIndex)
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
		WithLocation(walking.page.Location). // sbin_codex
		Build()

	if err := m.topPort.Send(rsp); err != nil {
		return false
	}
	m.walkingTranslations[walkingIndex].state = transactionFinished // sbin_codex

	// sbin_codex: UVM disposition and replay retirement (todo 8 of
	// mgpusim-uvm-manager). An old remote read committed to the CPU endpoint
	// is a distinct disposition; every other completed translation is
	// downstream-visible. A walk linked to a fault record retires it.
	trans := &m.walkingTranslations[walkingIndex]
	if trans.req.AccessKind == vm.AccessKindRead &&
		trans.page.Location == vm.MemoryLocationCPU_REMOTE {
		m.markDisposed(trans, dispositionRemoteCommitted)
	} else {
		m.markDisposed(trans, dispositionDownstreamVisible)
	}
	if trans.faultRecord != nil {
		m.retireFaultRecord(trans.faultRecord, trans.replay)
	}

	tracing.TraceReqComplete(walking.req, m.Comp)

	return true
}

// sbin_codex: doManagedFault converts a managed invalid translation into a
// typed fault (todo 8 of mgpusim-uvm-manager). The GMMU assigns a
// fault-pending token, owns a replay record, returns a fault-pending response
// so the leaf MSHRs retain the originals, and notifies the CP once per 64 KB
// region.
func (m *middleware) doManagedFault(
	walkingIndex int,
) bool {
	trans := &m.walkingTranslations[walkingIndex]
	req := trans.req

	rec := m.findRecordByReqID(req.ID)
	if rec == nil && req.FaultPendingToken != 0 {
		rec = m.findRecordByToken(req.FaultPendingToken)
	}
	if rec == nil {
		rec = m.newFaultRecord(req)
	}

	if !rec.notify || !rec.notified {
		if !rec.notify {
			// sbin_codex: coalesced onto an already-notified 64 KB region.
			rec.notified = true
		} else if !m.commandProcessor.CanSend() {
			return false
		} else {
			notif := m.buildFaultNotification(rec)
			if err := m.commandProcessor.Send(notif); err != nil {
				return false
			}
			rec.notified = true
		}
	}

	if !m.topPort.CanSend() {
		return false
	}
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(trans.page).
		WithLocation(vm.MemoryLocationINVALID).
		WithFaultPendingToken(rec.token).
		WithWaiterDelta(req.WaiterDelta).
		Build()
	if err := m.topPort.Send(rsp); err != nil {
		return false
	}

	trans.state = transactionFinished
	m.markDisposed(trans, dispositionRetained)
	tracing.TraceReqComplete(req, m.Comp)

	return true
}

// sbin_codex: newFaultRecord creates the GMMU-owned replay record for a
// stalled managed request. The first fault of a 64 KB region receives the
// region replay token; later faults in the same region coalesce onto the same
// service transaction without a second notification.
func (m *middleware) newFaultRecord(req *vm.TranslationReq) *faultRecord {
	regionBase := alignDown(req.VAddr, faultRegionSize)
	rec := &faultRecord{
		token:       m.nextFaultToken + 1,
		pid:         req.PID,
		vAddr:       req.VAddr,
		deviceID:    req.DeviceID,
		accessKind:  req.AccessKind,
		waiterDelta: req.WaiterDelta,
		regionBase:  regionBase,
		sequence:    m.gate.lastAssignedSequence,
		req:         req,
	}
	m.nextFaultToken++

	if !m.pendingRegions[regionBase] {
		m.pendingRegions[regionBase] = true
		rec.replayToken = m.nextReplayToken + 1
		m.nextReplayToken++
		m.regionReplayTokens[regionBase] = rec.replayToken
		rec.notify = true
	} else {
		rec.replayToken = m.regionReplayTokens[regionBase]
	}

	m.faultRecords = append(m.faultRecords, rec)
	return rec
}

func (m *middleware) findRecordByToken(
	token vm.FaultPendingToken,
) *faultRecord {
	for _, rec := range m.faultRecords {
		if rec.token == token {
			return rec
		}
	}
	return nil
}

func (m *middleware) findRecordByReqID(reqID string) *faultRecord {
	for _, rec := range m.faultRecords {
		if rec.req != nil && rec.req.ID == reqID {
			return rec
		}
	}
	return nil
}

func (m *middleware) buildFaultNotification(
	rec *faultRecord,
) *vm.FaultNotification {
	notif := &vm.FaultNotification{
		PID:               rec.pid,
		GPU:               m.deviceID,
		VAddr:             rec.vAddr,
		AccessKind:        rec.accessKind,
		FaultPendingToken: rec.token,
		ReplayToken:       rec.replayToken,
		WaiterDelta:       rec.waiterDelta,
	}
	notif.ID = sim.GetIDGenerator().Generate()
	notif.Src = m.commandProcessor.AsRemote()
	notif.Dst = rec.req.Src

	return notif
}

// sbin_codex: retireFaultRecord removes a replayed record from the GMMU-owned
// replay queue and clears the region once its last record retires. A walk
// linked to a replay command decrements the command's in-flight count.
func (m *middleware) retireFaultRecord(
	rec *faultRecord,
	replay *replayCommand,
) {
	for i, r := range m.faultRecords {
		if r == rec {
			m.faultRecords = append(m.faultRecords[:i], m.faultRecords[i+1:]...)
			break
		}
	}

	remaining := false
	for _, r := range m.faultRecords {
		if r.regionBase == rec.regionBase {
			remaining = true
			break
		}
	}
	if !remaining {
		delete(m.pendingRegions, rec.regionBase)
		delete(m.regionReplayTokens, rec.regionBase)
	}

	if replay != nil {
		replay.inFlight--
		m.maybeCompleteReplay(replay)
	}
}

// sbin_codex: handleReplayRange consumes a ReplayRange from the command
// processor. The replay token must match the serviced region; replays are
// serialized FIFO (one active at a time, uvm-manager.md §8.4).
func (m *middleware) handleReplayRange() bool {
	item := m.commandProcessor.PeekIncoming()
	if item == nil {
		return false
	}

	req, ok := item.(*vm.ReplayRange)
	if !ok {
		log.Panicf("GMMU cannot handle command-processor message of type %T", item)
	}

	regionBase := alignDown(req.StartVA, faultRegionSize)
	if req.GPU != m.deviceID ||
		(m.regionReplayTokens[regionBase] != 0 &&
			m.regionReplayTokens[regionBase] != req.ReplayToken) {
		// sbin_codex: unknown GPU or inconsistent replay token: reject.
		m.commandProcessor.RetrieveIncoming()
		return true
	}

	if m.activeReplay != nil {
		m.pendingReplays = append(m.pendingReplays, req)
		m.commandProcessor.RetrieveIncoming()
		return true
	}

	cmd := &replayCommand{req: req}
	m.activeReplay = cmd
	m.matchReplayRecords(cmd)
	m.commandProcessor.RetrieveIncoming()
	m.handleReplayTick()

	return true
}

// sbin_codex: matchReplayRecords collects the GMMU-owned records whose
// request falls inside the serviced range, in FIFO order.
func (m *middleware) matchReplayRecords(cmd *replayCommand) {
	for _, rec := range m.faultRecords {
		if rec.pid == cmd.req.PID &&
			rec.vAddr >= cmd.req.StartVA &&
			rec.vAddr < cmd.req.StartVA+cmd.req.Size {
			cmd.pending = append(cmd.pending, rec)
		}
	}
}

// sbin_codex: handleReplayTick re-injects the replayed walks under
// backpressure (bounded by maxRequestsInFlight) and completes the replay
// command once every matched record has been re-walked and retired.
func (m *middleware) handleReplayTick() bool {
	madeProgress := false

	if m.activeReplay != nil {
		cmd := m.activeReplay
		for len(cmd.pending) > 0 &&
			len(m.walkingTranslations) < m.maxRequestsInFlight {
			rec := cmd.pending[0]
			cmd.pending = cmd.pending[1:]
			if m.findRecordByToken(rec.token) != rec {
				continue // sbin_codex: already retired by a re-fetch walk.
			}
			m.appendWalking(rec.req, rec.sequence, rec, cmd)
			cmd.inFlight++
			madeProgress = true
		}
		m.maybeCompleteReplay(cmd)
	}

	if m.activeReplay == nil && len(m.pendingReplays) > 0 {
		req := m.pendingReplays[0]
		m.pendingReplays = m.pendingReplays[1:]
		cmd := &replayCommand{req: req}
		m.activeReplay = cmd
		m.matchReplayRecords(cmd)
		madeProgress = true
	}

	return madeProgress
}

// sbin_codex: maybeCompleteReplay acknowledges the replay command once every
// matched record has completed.
func (m *middleware) maybeCompleteReplay(cmd *replayCommand) bool {
	if len(cmd.pending) > 0 || cmd.inFlight > 0 {
		return false
	}
	if !m.commandProcessor.CanSend() {
		return false
	}

	ack := &vm.ReplayAck{
		RspTo:       cmd.req.ID,
		ReplayToken: cmd.req.ReplayToken,
	}
	ack.ID = sim.GetIDGenerator().Generate()
	ack.Src = m.commandProcessor.AsRemote()
	ack.Dst = cmd.req.Src
	if err := m.commandProcessor.Send(ack); err != nil {
		return false
	}

	m.activeReplay = nil
	return true
}

// sbin_codex: markDisposed records how an admitted request was resolved and
// decrements every active block that was waiting on it. A block whose
// pending-disposal count reaches zero becomes ackable.
func (c *Comp) markDisposed(trans *transaction, d disposition) {
	trans.disposition = d
	for _, block := range c.activeBlocks {
		if block.acked {
			continue
		}
		if trans.sequence <= block.watermark && block.matches(trans.req) {
			block.pendingDisposals--
		}
	}
	c.trySendBlockAcks()
}

// sbin_codex: trySendBlockAcks sends the exactly-one BlockAck per block once
// every matching request with sequence<=watermark is disposed.
func (c *Comp) trySendBlockAcks() bool {
	madeProgress := false
	for _, block := range c.activeBlocks {
		if block.acked || block.pendingDisposals > 0 {
			continue
		}
		if !c.controlPort.CanSend() {
			continue
		}
		ack := &vm.BlockAck{
			CommandID: block.commandID,
			GateID:    c.gate.gateID,
			Watermark: block.watermark,
		}
		ack.ID = sim.GetIDGenerator().Generate()
		ack.Src = c.controlPort.AsRemote()
		ack.Dst = block.src
		if err := c.controlPort.Send(ack); err != nil {
			continue
		}
		block.acked = true
		madeProgress = true
	}
	return madeProgress
}

// sbin_codex: matchingClosedBlock returns the first active block whose range
// matches the request. A request admitted while such a block is closed parks
// instead of walking.
func (c *Comp) matchingClosedBlock(req *vm.TranslationReq) *blockCommand {
	for _, block := range c.activeBlocks {
		if block.matches(req) {
			return block
		}
	}
	return nil
}

// sbin_codex: appendWalking starts a translation walk with the given ingress
// sequence and replay linkage.
func (c *Comp) appendWalking(
	req *vm.TranslationReq,
	sequence uint64,
	rec *faultRecord,
	replay *replayCommand,
) {
	trans := transaction{
		req:         req,
		level:       pageTableLevels - 1,
		fillLevel:   -1,
		msgID:       "invalid",
		state:       newTransaction,
		sequence:    sequence,
		faultRecord: rec,
		replay:      replay,
	}

	c.walkingTranslations = append(c.walkingTranslations, trans)
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
	// sbin_codex: the gate assigns a monotonic ingress sequence to every
	// admitted request. A request admitted while a matching block is closed
	// gets a sequence above the watermark and parks (todo 8 of
	// mgpusim-uvm-manager).
	m.gate.lastAssignedSequence++
	sequence := m.gate.lastAssignedSequence
	if block := m.matchingClosedBlock(req); block != nil {
		block.parked = append(block.parked, &parkedRequest{
			sequence: sequence,
			req:      req,
		})
		return
	}

	m.appendWalking(req, sequence, nil, nil)
}

// sbin_codex: matches reports whether the block range covers the request.
func (b *blockCommand) matches(req *vm.TranslationReq) bool {
	return req.PID == b.pid &&
		req.VAddr >= b.startVA &&
		req.VAddr < b.startVA+b.size
}

// sbin_codex: alignDown aligns an address down to the given power-of-two
// boundary.
func alignDown(addr, boundary uint64) uint64 {
	return addr &^ (boundary - 1)
}
