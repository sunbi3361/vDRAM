package writeback

// sbin_codex: scoped UVM range writeback/invalidation for the writeback cache
// (plan todo 13 of mgpusim-uvm-manager, uvm-manager.md section 19.1). This is
// the range-scoped counterpart of the global flusher: it drains matching
// in-flight cache/MSHR transactions, writes dirty matching lines to their
// stored HBM PAs through the write buffer, invalidates (or cleans) matching
// lines per the operation, and acks. Unrelated traffic stays active; the
// global flush, MSHR reset, and cache pause are never used.

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/sim"
)

type uvmRangeFlushState struct {
	req     *cache.UVMCacheRangeFlushReq
	matcher *cache.UVMRangeMatcher
	phase   cache.UVMRangeFlushPhase

	writebacks    []*transaction
	pendingBlocks []*cache.Block
}

type uvmRangeFlusher struct {
	cache  *Comp
	active *uvmRangeFlushState
}

func (f *uvmRangeFlusher) Tick() bool {
	if f.active != nil {
		switch f.active.phase {
		case cache.UVMRangeFlushDrain:
			return f.processDrain()
		case cache.UVMRangeFlushWriteback:
			return f.processWriteback()
		case cache.UVMRangeFlushWaitWriteback:
			return f.processWaitWriteback()
		case cache.UVMRangeFlushFinalize:
			return f.processFinalize()
		}
	}

	return f.extractFromPort()
}

func (f *uvmRangeFlusher) extractFromPort() bool {
	item := f.cache.controlPort.PeekIncoming()
	if item == nil {
		return false
	}

	req, ok := item.(*cache.UVMCacheRangeFlushReq)
	if !ok {
		return false // FlushReq/RestartReq belong to the flusher stage.
	}

	if f.cache.state != cacheStateRunning {
		return false // a global flush owns the cache.
	}

	if err := cache.ValidateUVMCacheRangeFlushReq(req); err != nil {
		return f.rejectMalformed(req)
	}

	f.active = &uvmRangeFlushState{
		req:     req,
		matcher: cache.NewUVMRangeMatcher(req, f.cache.uvmRangeVirtual),
		phase:   cache.UVMRangeFlushDrain,
	}
	f.cache.controlPort.RetrieveIncoming()

	return true
}

// rejectMalformed acks a malformed command without mutating any cache state.
func (f *uvmRangeFlusher) rejectMalformed(req *cache.UVMCacheRangeFlushReq) bool {
	if !f.cache.controlPort.CanSend() {
		return false
	}

	rsp := cache.UVMCacheRangeFlushRspBuilder{}.
		WithSrc(f.cache.controlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()
	f.cache.controlPort.Send(rsp)
	f.cache.controlPort.RetrieveIncoming()

	return true
}

func (f *uvmRangeFlusher) processDrain() bool {
	if f.matchingInflight() {
		return false
	}

	f.active.phase = cache.UVMRangeFlushWriteback

	return true
}

// matchingInflight reports whether any accepted matching cache/MSHR
// transaction or matching writeback is still in flight. The write buffer
// input queue window (a matching eviction between the bank stage and the
// write buffer) is at most one tick and is accepted.
func (f *uvmRangeFlusher) matchingInflight() bool {
	for _, t := range f.cache.inFlightTransactions {
		if t.accessReq() != nil {
			req := t.accessReq()
			if f.active.matcher.MatchAccess(
				req.GetPID(), req.GetAddress(), cache.ResolveAnnotation(req)) {
				return true
			}
		}
	}

	for _, entry := range f.cache.mshr.AllEntries() {
		if f.active.matcher.MatchMSHR(entry) {
			return true
		}
	}

	for tag := range f.cache.evictingList {
		if f.active.matcher.MatchEvictingTag(tag) {
			return true
		}
	}

	for _, t := range f.cache.writeBuffer.pendingEvictions {
		if f.matchingWriteback(t) {
			return true
		}
	}
	for _, t := range f.cache.writeBuffer.inflightEviction {
		if f.matchingWriteback(t) {
			return true
		}
	}

	return false
}

func (f *uvmRangeFlusher) matchingWriteback(t *transaction) bool {
	var ann *cache.VirtualAccessAnnotation
	if t.victim != nil {
		ann = t.victim.Annotation
	}
	return f.active.matcher.MatchWriteback(t.evictingPID, t.evictingAddr, ann)
}

func (f *uvmRangeFlusher) processWriteback() bool {
	if f.active.pendingBlocks == nil {
		f.active.pendingBlocks = f.collectMatchingDirtyBlocks()
	}

	pushed := false
	for len(f.active.pendingBlocks) > 0 {
		if f.cache.writeBuffer.writeBufferFull() {
			break
		}

		f.pushWriteback(f.active.pendingBlocks[0])
		f.active.pendingBlocks = f.active.pendingBlocks[1:]
		pushed = true
	}

	if len(f.active.pendingBlocks) == 0 {
		f.active.phase = cache.UVMRangeFlushWaitWriteback

		return true // the phase transition is progress
	}

	return pushed
}

func (f *uvmRangeFlusher) collectMatchingDirtyBlocks() []*cache.Block {
	var blocks []*cache.Block
	for _, set := range f.cache.directory.GetSets() {
		for _, block := range set.Blocks {
			if f.active.matcher.MatchBlock(block) && block.IsDirty {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// blockPA returns the stored HBM physical address of a line: the annotation
// HBMPA plus the in-page offset for virtual caches, the physical tag for
// baseline caches.
func (f *uvmRangeFlusher) blockPA(block *cache.Block) uint64 {
	if f.cache.uvmRangeVirtual && block.Annotation != nil {
		return block.Annotation.HBMPA + (block.Tag & (cache.UVMRangePageSize - 1))
	}
	return block.Tag
}

// pushWriteback sends one dirty line to the write buffer as a flush-style
// writeback transaction. The write buffer owns the bottom port, so the
// writeback reuses its backpressure and completion machinery.
func (f *uvmRangeFlusher) pushWriteback(block *cache.Block) {
	blockSize := 1 << f.cache.log2BlockSize
	data, err := f.cache.storage.Read(block.CacheAddress, uint64(blockSize))
	if err != nil {
		panic(err)
	}

	trans := &transaction{
		id:                sim.GetIDGenerator().Generate(),
		action:            writeBufferFlush,
		victim:            block,
		evictingPID:       block.PID,
		evictingAddr:      f.blockPA(block),
		evictingData:      data,
		evictingDirtyMask: block.DirtyMask,
	}
	f.cache.writeBuffer.pendingEvictions = append(
		f.cache.writeBuffer.pendingEvictions, trans)
	f.active.writebacks = append(f.active.writebacks, trans)
}

func (f *uvmRangeFlusher) processWaitWriteback() bool {
	for _, t := range f.active.writebacks {
		if f.writebackInFlight(t) {
			return false
		}
	}

	f.active.phase = cache.UVMRangeFlushFinalize

	return true
}

func (f *uvmRangeFlusher) writebackInFlight(t *transaction) bool {
	for _, e := range f.cache.writeBuffer.pendingEvictions {
		if e == t {
			return true
		}
	}
	for _, e := range f.cache.writeBuffer.inflightEviction {
		if e == t {
			return true
		}
	}
	return false
}

func (f *uvmRangeFlusher) processFinalize() bool {
	if !f.cache.controlPort.CanSend() {
		return false
	}

	if f.active.req.Operation == cache.UVMCacheRangeFlushWritebackInvalidate {
		f.invalidateMatching()
	} else {
		f.cleanMatching()
	}

	rsp := cache.UVMCacheRangeFlushRspBuilder{}.
		WithSrc(f.cache.controlPort.AsRemote()).
		WithDst(f.active.req.Src).
		WithRspTo(f.active.req.ID).
		Build()
	f.cache.controlPort.Send(rsp)

	f.active = nil

	return true
}

func (f *uvmRangeFlusher) invalidateMatching() {
	for _, set := range f.cache.directory.GetSets() {
		for _, block := range set.Blocks {
			if f.active.matcher.MatchBlock(block) {
				block.IsValid = false
				block.IsDirty = false
				block.DirtyMask = nil
				block.IsLocked = false
				block.ReadCount = 0
				block.Annotation = nil
			}
		}
	}
}

func (f *uvmRangeFlusher) cleanMatching() {
	for _, set := range f.cache.directory.GetSets() {
		for _, block := range set.Blocks {
			if f.active.matcher.MatchBlock(block) {
				block.IsDirty = false
				block.DirtyMask = nil
			}
		}
	}
}
