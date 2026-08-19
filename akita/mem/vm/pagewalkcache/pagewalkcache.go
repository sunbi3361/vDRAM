package pagewalkcache

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// Comp is a non-blocking page-walk cache accessed by a GMMU.
type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder

	topPort sim.Port

	numBlocks           int
	numLevels           int
	numReqPerCycle      int
	maxRequestsInFlight int
	pageSize            uint64
	log2PageSize        uint64
	bitsPerLevel        uint64
	latency             int

	sets        []setState
	reads       []readTransaction
	removeReads []int
}

type readTransaction struct {
	req       *LookupReq
	cycleLeft int
}

type blockState struct {
	PID     vm.PID
	Segment uint64
	WayID   int
	Valid   bool
}

type setState struct {
	Blocks []blockState
	LRU    []int
}

const lowestCacheLevel = 1 // sbin_codex: leaf level zero is never cached.

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

func initSets(numLevels, numBlocks int) []setState {
	sets := make([]setState, numLevels)
	for level := lowestCacheLevel; level < len(sets); level++ {
		sets[level].Blocks = make([]blockState, numBlocks)
		sets[level].LRU = make([]int, numBlocks)
		for way := 0; way < numBlocks; way++ {
			sets[level].Blocks[way].WayID = way
			sets[level].LRU[way] = way
		}
	}
	return sets
}

func setLookup(s *setState, pid vm.PID, segment uint64) (int, bool) {
	for _, block := range s.Blocks {
		if block.Valid && block.PID == pid && block.Segment == segment {
			return block.WayID, true
		}
	}
	return 0, false
}

func setUpdate(s *setState, wayID int, pid vm.PID, segment uint64) {
	block := &s.Blocks[wayID]
	block.PID = pid
	block.Segment = segment
	block.Valid = true
	setVisit(s, wayID)
}

func setEvict(s *setState) (int, bool) {
	if len(s.LRU) == 0 {
		return 0, false
	}
	return s.LRU[0], true
}

func setVisit(s *setState, wayID int) {
	for i, candidate := range s.LRU {
		if candidate != wayID {
			continue
		}
		copy(s.LRU[i:], s.LRU[i+1:])
		s.LRU[len(s.LRU)-1] = wayID
		return
	}
	s.LRU = append(s.LRU, wayID)
}

func pageTableSegment(
	vAddr, log2PageSize, bitsPerLevel uint64,
	numLevels, level int,
) uint64 {
	vpn := vAddr >> log2PageSize
	shift := uint64(level) * bitsPerLevel
	prefixBits := uint64(numLevels-level) * bitsPerLevel
	return (vpn >> shift) & lowBitsMask(prefixBits)
}

func lowBitsMask(bits uint64) uint64 {
	if bits >= 64 {
		return ^uint64(0)
	}
	return (uint64(1) << bits) - 1
}

func (c *Comp) segment(vAddr uint64, level int) (uint64, bool) {
	if level < lowestCacheLevel || level >= c.numLevels {
		return 0, false
	}
	return pageTableSegment(
		vAddr,
		c.log2PageSize,
		c.bitsPerLevel,
		c.numLevels,
		level,
	), true
}

func (c *Comp) fill(req *FillReq) {
	segment, validLevel := c.segment(req.VAddr, req.Level)
	if !validLevel {
		return
	}
	set := &c.sets[req.Level]
	if _, found := setLookup(set, req.PID, segment); found {
		return
	}
	wayID, ok := setEvict(set)
	if ok {
		setUpdate(set, wayID, req.PID, segment)
	}
}

// lookup checks every level in one modeled access. // sbin_codex: each level
// is an independent fully-associative bank, so the lowest numerical hit is
// the deepest page-walk level available to GMMU.
func (c *Comp) lookup(req *LookupReq) (int, bool) {
	deepestHitLevel := -1
	for level := c.numLevels - 1; level >= lowestCacheLevel; level-- {
		segment, validLevel := c.segment(req.VAddr, level)
		if !validLevel {
			continue
		}
		set := &c.sets[level]
		wayID, found := setLookup(set, req.PID, segment)
		if !found {
			continue
		}
		setVisit(set, wayID)
		deepestHitLevel = level
	}
	return deepestHitLevel, deepestHitLevel >= lowestCacheLevel
}

var _ sim.Component = (*Comp)(nil)
