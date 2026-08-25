package writeback

// sbin_codex: region-scoped writeback+invalidate for the UVM control path.
//
// Unlike the global flusher this never puts the cache into a flushing state:
// only the lines belonging to the victim region are drained, written back, and
// invalidated, while every unrelated line and request keeps flowing. The
// sequence is
//
//	wait until the region has no in-flight access and no locked line
//	 -> invalidate matching lines, lock and queue the dirty ones
//	 -> issue one bankEvict per dirty line through the normal write buffer
//	 -> acknowledge only after every write-back reached the low module

import (
	"github.com/sarchlab/akita/v4/mem/cache"
)

type rangeFlushStage int

const (
	rangeFlushIdle rangeFlushStage = iota
	rangeFlushWaiting
	rangeFlushEvicting
	rangeFlushDraining
)

func (f *flusher) startProcessingRangeFlush(req *cache.RangeFlushReq) bool {
	if f.processingRangeFlush != nil {
		return false
	}

	f.processingRangeFlush = req
	f.rangeStage = rangeFlushWaiting
	f.cache.controlPort.RetrieveIncoming()

	return true
}

func (f *flusher) processRangeFlush() bool {
	if f.processingRangeFlush == nil {
		return false
	}

	switch f.rangeStage {
	case rangeFlushWaiting:
		return f.prepareRangeFlush()
	case rangeFlushEvicting:
		return f.issueRangeEvictions()
	case rangeFlushDraining:
		return f.finalizeRangeFlush()
	default:
		return false
	}
}

// prepareRangeFlush waits until nothing is actively touching the region, then
// invalidates the matching lines and reserves the dirty ones for write-back.
func (f *flusher) prepareRangeFlush() bool {
	req := f.processingRangeFlush

	if f.regionHasInFlightAccess(req) {
		return false
	}

	for _, set := range f.cache.directory.GetSets() {
		for _, block := range set.Blocks {
			if !block.IsValid || !req.MatchesBlock(block.PID, block.Tag) {
				continue
			}

			if !f.reserveVictim(block, req) {
				return false
			}
		}
	}

	f.rangeStage = rangeFlushEvicting

	return true
}

// reserveVictim takes one matching line out of service. It returns false when
// the line is still busy, so the caller retries on a later cycle.
func (f *flusher) reserveVictim(
	block *cache.Block,
	req *cache.RangeFlushReq,
) bool {
	if block.IsLocked || block.ReadCount > 0 {
		return false
	}

	if _, evicting := f.cache.evictingList[block.Tag]; evicting {
		return false
	}

	block.IsValid = false

	if block.IsDirty && req.Writeback {
		block.IsLocked = true
		f.cache.evictingList[block.Tag] = true
		f.rangeBlockToEvict = append(f.rangeBlockToEvict, block)
		f.rangeLockedBlocks = append(f.rangeLockedBlocks, block)

		return true
	}

	block.IsDirty = false

	return true
}

// regionHasInFlightAccess reports whether an accepted read/write still targets
// the region. Requests to unrelated addresses are ignored.
func (f *flusher) regionHasInFlightAccess(req *cache.RangeFlushReq) bool {
	for _, trans := range f.cache.inFlightTransactions {
		access := trans.accessReq()
		if access == nil {
			continue
		}

		if req.MatchesAddress(access.GetPID(), access.GetAddress()) {
			return true
		}
	}

	return false
}

// issueRangeEvictions pushes the reserved dirty lines into the bank stages,
// reusing the ordinary eviction path all the way to the low module.
func (f *flusher) issueRangeEvictions() bool {
	if len(f.rangeBlockToEvict) == 0 {
		f.rangeStage = rangeFlushDraining
		return true
	}

	block := f.rangeBlockToEvict[0]
	bankNum := bankID(
		block,
		f.cache.directory.WayAssociativity(),
		len(f.cache.dirToBankBuffers))
	bankBuf := f.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		return false
	}

	bankBuf.Push(&transaction{
		action:            bankEvict,
		rangeFlush:        f.processingRangeFlush,
		victim:            block,
		evictingPID:       block.PID,
		evictingAddr:      block.Tag,
		evictingDirtyMask: block.DirtyMask,
		rangeFlushID:      f.processingRangeFlush.ID,
	})

	f.rangeBlockToEvict = f.rangeBlockToEvict[1:]
	f.rangeEvictionsPending++

	return true
}

func (f *flusher) finalizeRangeFlush() bool {
	if f.rangeEvictionsPending > 0 {
		return false
	}

	// A store that was translated just before the region's translations were
	// invalidated can still arrive while the first pass is writing back. The
	// pass therefore repeats until the region is both clean and quiet; it
	// terminates because no new translation for the region can be obtained
	// once the driver has invalidated the range. // sbin_codex
	if f.rangeNeedsAnotherPass() {
		f.unlockRangeVictims()

		f.rangeStage = rangeFlushWaiting

		return true
	}

	if !f.cache.controlPort.CanSend() {
		return false
	}

	req := f.processingRangeFlush
	rsp := cache.RangeFlushRspBuilder{}.
		WithSrc(f.cache.controlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()

	if err := f.cache.controlPort.Send(rsp); err != nil {
		return false
	}

	f.unlockRangeVictims()

	f.processingRangeFlush = nil
	f.rangeStage = rangeFlushIdle

	return true
}

// rangeNeedsAnotherPass reports whether the region acquired new lines or new
// accesses while the current pass was running.
func (f *flusher) rangeNeedsAnotherPass() bool {
	req := f.processingRangeFlush

	if f.regionHasInFlightAccess(req) {
		return true
	}

	for _, set := range f.cache.directory.GetSets() {
		for _, block := range set.Blocks {
			if !block.IsValid || block.IsLocked {
				continue
			}

			if req.MatchesBlock(block.PID, block.Tag) {
				return true
			}
		}
	}

	return false
}

// unlockRangeVictims releases the lines this pass took out of service.
func (f *flusher) unlockRangeVictims() {
	for _, block := range f.rangeLockedBlocks {
		block.IsLocked = false
		block.IsDirty = false
	}

	f.rangeLockedBlocks = nil
}

// rangeEvictionCompleted is called by the write buffer when one region
// write-back has been acknowledged by the low module.
func (f *flusher) rangeEvictionCompleted() {
	if f.rangeEvictionsPending > 0 {
		f.rangeEvictionsPending--
	}
}
