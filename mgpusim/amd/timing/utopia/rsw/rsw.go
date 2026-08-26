// sbin_claude_utopia
// Package rsw implements the Utopia Translation Unit (UTU): the GPU-side
// RestSeg walker. It sits between the shared L2 TLB and the GMMU. An L2 TLB
// miss first performs a RestSeg Walk (SF lookup, then TAR tag match,
// utopia.md 4.5); only when every RestSeg reports NotInRestSeg is the request
// forwarded to the GMMU for the conventional FlexSeg walk (utopia.md 4.7).
//
// Timing model (v1): the SF and TAR caches are finite, set-associative,
// LRU-managed metadata caches with configurable hit latency; a miss charges a
// configurable memory-fetch latency, matching the modeling level of the GMMU
// page walk (latency model, no real DRAM contention). The functional truth of
// the TAR/SF contents lives in the driver-owned restseg.Registry, exactly
// like the GMMU consults the functional page table after its modeled walk.
package rsw

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
)

// Stats counts the observable RestSeg-walk behavior (utopia.md 4.14).
type Stats struct {
	RSWHits        uint64 // translations resolved by the RestSeg walk
	RSWMisses      uint64 // NotInRestSeg after a TAR tag match attempt
	SFFiltered     uint64 // TAR access skipped because SF[set] == 0
	SFCacheHits    uint64
	SFCacheMisses  uint64
	TARCacheHits   uint64
	TARCacheMisses uint64
	FlexSegWalks   uint64 // requests forwarded to the GMMU
	Passthrough    uint64 // forwarded untouched (no RestSeg configured)
}

type transactionState int

const (
	stateSFAccess transactionState = iota
	stateTARAccess
	stateRespond
	stateForward
	stateWaitingFSW
	stateFinished
)

type transaction struct {
	req       *vm.TranslationReq
	state     transactionState
	cycleLeft int

	page vm.Page // resolved RestSeg translation (stateRespond)

	sfWasHit  bool
	sfLatency int
	forwardID string // ID of the request forwarded to the GMMU
}

// metaLine is one cached line of TAR or SF metadata.
type metaLine struct {
	valid bool
	tag   uint64
}

// metaSet holds the lines of one cache set with an LRU order (index 0 is the
// next victim), following the pagewalkcache replacement structure.
type metaSet struct {
	lines []metaLine
	lru   []int
}

// metaCache is a presence-only cache over TAR or SF metadata lines. One line
// covers entriesPerLine consecutive RestSeg sets (multiple small TAR/SF
// entries share one memory line). It answers "would this metadata access hit
// in the small GMMU-side cache" (utopia.md 4.6); the metadata value itself is
// read from the authoritative registry.
type metaCache struct {
	numSets        int
	entriesPerLine int
	sets           []metaSet
}

func newMetaCache(capacityBytes uint64, lineBytes, assoc, entriesPerLine int) metaCache {
	numLines := int(capacityBytes) / lineBytes
	if numLines < assoc {
		numLines = assoc
	}
	numSets := numLines / assoc
	if numSets == 0 {
		numSets = 1
	}

	sets := make([]metaSet, numSets)
	for i := range sets {
		sets[i].lines = make([]metaLine, assoc)
		sets[i].lru = make([]int, assoc)
		for way := 0; way < assoc; way++ {
			sets[i].lru[way] = way
		}
	}

	return metaCache{
		numSets:        numSets,
		entriesPerLine: entriesPerLine,
		sets:           sets,
	}
}

// lineIDOf maps a RestSeg set index to the metadata line holding its entry.
func (c *metaCache) lineIDOf(restSegSet int) uint64 {
	return uint64(restSegSet / c.entriesPerLine)
}

// lookup reports whether the metadata line is cached and refreshes LRU on a
// hit.
func (c *metaCache) lookup(restSegSet int) bool {
	lineID := c.lineIDOf(restSegSet)
	set := &c.sets[lineID%uint64(c.numSets)]
	tag := lineID / uint64(c.numSets)

	for way, line := range set.lines {
		if line.valid && line.tag == tag {
			set.visit(way)
			return true
		}
	}

	return false
}

// install fills the metadata line, evicting the LRU way when needed.
func (c *metaCache) install(restSegSet int) {
	lineID := c.lineIDOf(restSegSet)
	set := &c.sets[lineID%uint64(c.numSets)]
	tag := lineID / uint64(c.numSets)

	for way, line := range set.lines {
		if line.valid && line.tag == tag {
			set.visit(way)
			return
		}
	}

	victim := set.lru[0]
	set.lines[victim] = metaLine{valid: true, tag: tag}
	set.visit(victim)
}

// invalidateAll drops every cached metadata line (shootdown support).
func (c *metaCache) invalidateAll() {
	for i := range c.sets {
		for way := range c.sets[i].lines {
			c.sets[i].lines[way] = metaLine{}
		}
	}
}

func (s *metaSet) visit(way int) {
	for i, candidate := range s.lru {
		if candidate != way {
			continue
		}
		copy(s.lru[i:], s.lru[i+1:])
		s.lru[len(s.lru)-1] = way
		return
	}
	s.lru = append(s.lru, way)
}

// Comp is the Utopia Translation Unit component.
type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder

	topPort    sim.Port // faces the L2 TLB bottom
	bottomPort sim.Port // faces the GMMU top (FlexSeg walker)

	flexSegWalker sim.RemotePort

	deviceID uint64
	registry *restseg.Registry
	configs  []restseg.Config // lazily fetched from the registry
	fetched  bool

	sfCache  metaCache
	tarCache metaCache

	sfHitLatency  int
	tarHitLatency int
	missLatency   int

	maxReqInFlight int

	transactions []transaction

	stats Stats
}

// Tick runs the middleware chain.
func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// SetFlexSegWalker sets the GMMU port that serves FlexSeg page walks.
func (c *Comp) SetFlexSegWalker(port sim.RemotePort) {
	c.flexSegWalker = port
}

// Stats returns a copy of the accumulated counters.
func (c *Comp) Stats() Stats {
	return c.stats
}

// InvalidateMetadataCaches drops every cached TAR/SF line. The driver-side
// shootdown path uses it when RestSeg mappings change (utopia.md 4.12).
func (c *Comp) InvalidateMetadataCaches() {
	c.sfCache.invalidateAll()
	c.tarCache.invalidateAll()
}

// segmentConfigs lazily loads the RestSeg layouts. The driver registers them
// during platform construction (RegisterGPU), which runs after the GPU
// builder but before the first simulation tick.
func (c *Comp) segmentConfigs() []restseg.Config {
	if !c.fetched {
		c.configs = c.registry.SegmentConfigs(int(c.deviceID))
		c.fetched = true

		// sbin_claude_utopia: derive the TAR line packing from the segment
		// associativity now that it is known. One set's TAR entries occupy
		// assoc*tarEntryBytes bytes, so one 64B line covers
		// lineBytes/(assoc*tarEntryBytes) sets (floored at one set per line
		// for very wide sets). startWalk consults this method before the
		// first TAR cache probe, so the packing is set before any lookup.
		if len(c.configs) > 0 {
			setsPerLine := metaLineBytes /
				(c.configs[0].Associativity * tarEntryBytes)
			if setsPerLine < 1 {
				setsPerLine = 1
			}
			c.tarCache.entriesPerLine = setsPerLine
		}
	}

	return c.configs
}

var _ sim.Component = (*Comp)(nil)
