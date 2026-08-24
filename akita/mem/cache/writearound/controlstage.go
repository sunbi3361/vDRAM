package writearound

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/sim"
)

type controlStage struct {
	ctrlPort     sim.Port
	transactions *[]*transaction
	directory    cache.Directory
	cache        *Comp
	coalescer    *coalescer
	bankStages   []*bankStage

	currFlushReq *cache.FlushReq
	// sbin_codex: the active scoped UVM range flush (plan todo 13 of
	// mgpusim-uvm-manager). Write-through caches degenerate to drain +
	// invalidate: no dirty lines can be held.
	currRangeFlush *uvmRangeFlushState
}

type uvmRangeFlushState struct {
	req     *cache.UVMCacheRangeFlushReq
	matcher *cache.UVMRangeMatcher
	phase   cache.UVMRangeFlushPhase
}

func (s *controlStage) Tick() bool {
	madeProgress := false

	madeProgress = s.processNewRequest() || madeProgress
	madeProgress = s.processCurrentFlush() || madeProgress
	madeProgress = s.processCurrentRangeFlush() || madeProgress // sbin_codex

	return madeProgress
}

func (s *controlStage) processCurrentFlush() bool {
	if s.currFlushReq == nil {
		return false
	}

	if s.shouldWaitForInFlightTransactions() {
		return false
	}

	rsp := cache.FlushRspBuilder{}.
		WithSrc(s.ctrlPort.AsRemote()).
		WithDst(s.currFlushReq.Src).
		WithRspTo(s.currFlushReq.ID).
		Build()

	err := s.ctrlPort.Send(rsp)
	if err != nil {
		return false
	}

	s.hardResetCache()
	s.currFlushReq = nil

	return true
}

func (s *controlStage) hardResetCache() {
	s.flushPort(s.cache.topPort)
	s.flushPort(s.cache.bottomPort)
	s.flushBuffer(s.cache.dirBuf)

	for _, bankBuf := range s.cache.bankBufs {
		s.flushBuffer(bankBuf)
	}

	s.directory.Reset()
	s.cache.mshr.Reset()
	s.cache.coalesceStage.Reset()

	for _, bankStage := range s.cache.bankStages {
		bankStage.Reset()
	}

	s.cache.transactions = nil
	s.cache.postCoalesceTransactions = nil

	if s.currFlushReq.PauseAfterFlushing {
		s.cache.isPaused = true
	}
}

func (s *controlStage) flushPort(port sim.Port) {
	for port.PeekIncoming() != nil {
		port.RetrieveIncoming()
	}
}

func (s *controlStage) flushBuffer(buffer sim.Buffer) {
	for buffer.Pop() != nil {
	}
}

func (s *controlStage) processNewRequest() bool {
	req := s.ctrlPort.PeekIncoming()
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *cache.FlushReq:
		return s.startCacheFlush(req)
	case *cache.RestartReq:
		return s.doCacheRestart(req)
	case *cache.UVMCacheRangeFlushReq: // sbin_codex: scoped UVM range flush (plan todo 13).
		return s.startRangeFlush(req)
	default:
		log.Panicf("cannot handle request of type %s ",
			reflect.TypeOf(req))
	}

	panic("never")
}

func (s *controlStage) startCacheFlush(req *cache.FlushReq) bool {
	// sbin_codex: a range flush owns the control path; defer the global
	// flush until it completes (plan todo 13 of mgpusim-uvm-manager).
	if s.currRangeFlush != nil {
		return false
	}

	if s.currFlushReq != nil {
		return false
	}

	s.currFlushReq = req
	s.ctrlPort.RetrieveIncoming()

	return true
}

// startRangeFlush accepts a validated UVM range flush command; malformed
// commands are rejected with an ack and no cache mutation. // sbin_codex
func (s *controlStage) startRangeFlush(req *cache.UVMCacheRangeFlushReq) bool {
	if s.currFlushReq != nil || s.currRangeFlush != nil {
		return false
	}

	if err := cache.ValidateUVMCacheRangeFlushReq(req); err != nil {
		return s.rejectMalformedRangeFlush(req)
	}

	s.currRangeFlush = &uvmRangeFlushState{
		req:     req,
		matcher: cache.NewUVMRangeMatcher(req, s.cache.uvmRangeVirtual),
		phase:   cache.UVMRangeFlushDrain,
	}
	s.ctrlPort.RetrieveIncoming()

	return true
}

// rejectMalformedRangeFlush acks a malformed command without mutating any
// cache state. // sbin_codex
func (s *controlStage) rejectMalformedRangeFlush(
	req *cache.UVMCacheRangeFlushReq,
) bool {
	if !s.ctrlPort.CanSend() {
		return false
	}

	rsp := cache.UVMCacheRangeFlushRspBuilder{}.
		WithSrc(s.ctrlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()
	if err := s.ctrlPort.Send(rsp); err != nil {
		return false
	}

	s.ctrlPort.RetrieveIncoming()

	return true
}

// processCurrentRangeFlush drains matching in-flight transactions, then
// invalidates matching lines only for WRITEBACK_INVALIDATE, and acks. // sbin_codex
func (s *controlStage) processCurrentRangeFlush() bool {
	if s.currRangeFlush == nil {
		return false
	}

	switch s.currRangeFlush.phase {
	case cache.UVMRangeFlushDrain:
		if s.matchingInflight() {
			return false
		}
		s.currRangeFlush.phase = cache.UVMRangeFlushFinalize
		return true
	case cache.UVMRangeFlushFinalize:
		return s.finalizeRangeFlush()
	}

	return false
}

// matchingInflight reports whether any accepted matching transaction or
// pending matching refill is still in flight. // sbin_codex
func (s *controlStage) matchingInflight() bool {
	for _, t := range *s.transactions {
		if t.accessReq() != nil {
			req := t.accessReq()
			if s.currRangeFlush.matcher.MatchAccess(
				req.GetPID(), req.GetAddress(), cache.ResolveAnnotation(req)) {
				return true
			}
		}
	}

	for _, entry := range s.cache.mshr.AllEntries() {
		if s.currRangeFlush.matcher.MatchMSHR(entry) {
			return true
		}
	}

	return false
}

// finalizeRangeFlush invalidates matching lines for WRITEBACK_INVALIDATE
// (write-through caches hold no dirty data, so WRITEBACK_ONLY degenerates to
// drain only) and sends the completion ack. // sbin_codex
func (s *controlStage) finalizeRangeFlush() bool {
	if !s.ctrlPort.CanSend() {
		return false
	}

	if s.currRangeFlush.req.Operation ==
		cache.UVMCacheRangeFlushWritebackInvalidate {
		s.invalidateMatching()
	}

	rsp := cache.UVMCacheRangeFlushRspBuilder{}.
		WithSrc(s.ctrlPort.AsRemote()).
		WithDst(s.currRangeFlush.req.Src).
		WithRspTo(s.currRangeFlush.req.ID).
		Build()
	if err := s.ctrlPort.Send(rsp); err != nil {
		return false
	}

	s.currRangeFlush = nil

	return true
}

func (s *controlStage) invalidateMatching() {
	for _, set := range s.cache.directory.GetSets() {
		for _, block := range set.Blocks {
			if s.currRangeFlush.matcher.MatchBlock(block) {
				block.IsValid = false
				block.IsLocked = false
				block.ReadCount = 0
				block.IsDirty = false
				block.DirtyMask = nil
				block.Annotation = nil
			}
		}
	}
}

func (s *controlStage) doCacheRestart(req *cache.RestartReq) bool {
	s.cache.isPaused = false

	s.ctrlPort.RetrieveIncoming()

	for s.cache.topPort.PeekIncoming() != nil {
		s.cache.topPort.RetrieveIncoming()
	}

	for s.cache.bottomPort.PeekIncoming() != nil {
		s.cache.bottomPort.RetrieveIncoming()
	}

	rsp := cache.RestartRspBuilder{}.
		WithSrc(s.ctrlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	err := s.ctrlPort.Send(rsp)
	if err != nil {
		log.Panic("Unable to send restart rsp")
	}

	return true
}

func (s *controlStage) shouldWaitForInFlightTransactions() bool {
	return !s.currFlushReq.DiscardInflight && len(s.cache.transactions) != 0
}
