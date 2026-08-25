package writearound

// sbin_codex: region-scoped cache maintenance for the UVM control path. The
// cache is never paused and unrelated lines are never touched; only requests
// that target the victim region are drained before the matching lines are
// invalidated.

import "github.com/sarchlab/akita/v4/mem/cache"

func (s *controlStage) startRangeFlush(req *cache.RangeFlushReq) bool {
	if s.currRangeFlushReq != nil {
		return false
	}

	s.currRangeFlushReq = req
	s.ctrlPort.RetrieveIncoming()

	return true
}

func (s *controlStage) processCurrentRangeFlush() bool {
	req := s.currRangeFlushReq
	if req == nil {
		return false
	}

	if s.hasInFlightTransactionInRange(req) {
		return false
	}

	// A write-around cache never holds dirty data, so Writeback degenerates to
	// drain + invalidate.
	if req.Invalidate {
		s.invalidateRangeBlocks(req)
	}

	// A request translated just before the region's translations were
	// invalidated can still arrive during the pass, so the pass repeats until
	// the region is quiet. It terminates because no new translation for the
	// region can be obtained once the driver has invalidated the range.
	// sbin_codex
	if s.hasInFlightTransactionInRange(req) {
		return true
	}

	rsp := cache.RangeFlushRspBuilder{}.
		WithSrc(s.ctrlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()

	if err := s.ctrlPort.Send(rsp); err != nil {
		return false
	}

	s.currRangeFlushReq = nil

	return true
}

func (s *controlStage) hasInFlightTransactionInRange(
	req *cache.RangeFlushReq,
) bool {
	for _, trans := range s.cache.transactions {
		if req.MatchesAddress(trans.PID(), trans.Address()) {
			return true
		}
	}

	return false
}

func (s *controlStage) invalidateRangeBlocks(req *cache.RangeFlushReq) {
	for _, set := range s.directory.GetSets() {
		for _, block := range set.Blocks {
			if !block.IsValid {
				continue
			}

			if !req.MatchesBlock(block.PID, block.Tag) {
				continue
			}

			block.IsValid = false
			block.IsDirty = false
		}
	}
}
