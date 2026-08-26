// sbin_claude_fbt
// Package fbt implements the Forward-Backward Table (FBT) of the GPU virtual
// cache hierarchy (Yoon, Lowe-Power and Sohi, "Filtering Translation
// Bandwidth with Virtual Caching", ASPLOS'18, section 4).
//
// The FBT sits in the IOMMU, below the shared L2 TLB and above the page
// walker, in the same position the Utopia UTU occupies. An L2 TLB miss
// consults it before a page walk is allowed to start.
//
// # What the paper's structure is
//
// The FBT is fully inclusive of the GPU caches: it holds an entry for every
// physical page that currently has data cached in the private L1s or the
// shared L2. It has two halves. The backward table (BT) is tagged by physical
// page number and stores the page's leading virtual page number plus a bit
// vector of which of its lines are cached; it exists for synonym detection,
// for reverse-translating coherence probes that arrive from the CPU or
// directory with physical addresses, and as a coherence filter. The forward
// table (FT) is tagged by the leading virtual page number and stores the
// index of the matching BT entry, which lets the same structure be indexed by
// virtual address as well.
//
// That forward index is what makes the FBT usable as a large second-level
// TLB: a virtual lookup reaches a BT entry whose tag is the physical page
// number, which is exactly a translation. The paper measures 74% of shared
// TLB misses hitting in it, which is the point - those misses skip the page
// walk.
//
// # What this component models, and what it does not
//
// Modelled: the second-level TLB role. A 16K-entry, set-associative,
// virtually indexed table with a configurable lookup latency, consulted on
// every L2 TLB miss, filled from page-walk responses.
//
// Not modelled: the BT's reverse-translation and coherence-filter duties, the
// per-line bit vector, leading-virtual-address bookkeeping and synonym
// detection. Nothing in this simulator issues a physical-address probe into
// the GPU or maps one physical page at two virtual addresses, so those
// mechanisms would have no traffic to act on.
//
// One deliberate approximation to keep in mind when reading results: entries
// are replaced by LRU rather than being tied to cache residency. A page that
// left the caches entirely but was translated recently keeps its entry here,
// where the paper's inclusive table would have dropped it. Since a 16K-entry
// table already holds more pages than a 2MB L2 can (the paper counts about
// 6000 resident 4KB pages), this biases the hit rate slightly high. Making it
// exact needs fill and eviction notifications from the L1 and L2 caches.
package fbt

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// Stats counts the observable FBT behaviour. The hit count is the number of
// page walks the structure avoided.
type Stats struct {
	Hits      uint64 // L2 TLB misses resolved without a page walk
	Misses    uint64 // L2 TLB misses that still needed a page walk
	Installs  uint64 // entries filled from a walk response
	Evictions uint64 // valid entries displaced by an install
}

type transactionState int

const (
	stateLookup transactionState = iota
	stateRespond
	stateForward
	stateWaitingWalk
	stateFinished
)

type transaction struct {
	req       *vm.TranslationReq
	state     transactionState
	cycleLeft int

	page      vm.Page
	forwardID string // ID of the request handed to the page walker
}

// entry is one tracked page mapping.
type entry struct {
	valid bool
	tag   uint64
	pid   vm.PID
	page  vm.Page
}

// entrySet is one set with an LRU order; index 0 is the next victim, matching
// the replacement structure used elsewhere in the tree.
type entrySet struct {
	entries []entry
	lru     []int
}

func (s *entrySet) visit(way int) {
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

// table is the virtually indexed view of the FBT.
type table struct {
	numSets int
	sets    []entrySet
}

func newTable(numSets, numWays int) table {
	sets := make([]entrySet, numSets)
	for i := range sets {
		sets[i].entries = make([]entry, numWays)
		sets[i].lru = make([]int, numWays)

		for way := 0; way < numWays; way++ {
			sets[i].lru[way] = way
		}
	}

	return table{numSets: numSets, sets: sets}
}

// lookup returns the mapping of a virtual page and refreshes LRU on a hit.
func (t *table) lookup(pid vm.PID, vPageID uint64) (vm.Page, bool) {
	set := &t.sets[t.setIDOf(vPageID)]

	for way, e := range set.entries {
		if e.valid && e.tag == vPageID && e.pid == pid {
			set.visit(way)

			return e.page, true
		}
	}

	return vm.Page{}, false
}

// install adds a mapping, evicting the LRU way when the set is full. It
// reports whether a valid entry was displaced.
func (t *table) install(pid vm.PID, vPageID uint64, page vm.Page) bool {
	set := &t.sets[t.setIDOf(vPageID)]

	for way, e := range set.entries {
		if e.valid && e.tag == vPageID && e.pid == pid {
			set.entries[way].page = page
			set.visit(way)

			return false
		}
	}

	victim := set.lru[0]
	evicted := set.entries[victim].valid
	set.entries[victim] = entry{valid: true, tag: vPageID, pid: pid, page: page}
	set.visit(victim)

	return evicted
}

// invalidate drops the mapping of one virtual page, if present.
func (t *table) invalidate(pid vm.PID, vPageID uint64) {
	set := &t.sets[t.setIDOf(vPageID)]

	for way, e := range set.entries {
		if e.valid && e.tag == vPageID && e.pid == pid {
			set.entries[way] = entry{}

			return
		}
	}
}

// invalidateAll drops every mapping.
func (t *table) invalidateAll() {
	for i := range t.sets {
		for way := range t.sets[i].entries {
			t.sets[i].entries[way] = entry{}
		}
	}
}

func (t *table) setIDOf(vPageID uint64) uint64 {
	return vPageID % uint64(t.numSets)
}

// Comp is the Forward-Backward Table component.
type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder

	topPort    sim.Port // faces the L2 TLB bottom
	bottomPort sim.Port // faces the page walker (GMMU top)

	pageWalker sim.RemotePort

	log2PageSize   uint64
	lookupLatency  int
	maxReqInFlight int

	table table

	transactions []transaction

	stats Stats
}

// Tick runs the middleware chain.
func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// SetPageWalker sets the port that serves the page walks this table misses.
func (c *Comp) SetPageWalker(port sim.RemotePort) {
	c.pageWalker = port
}

// Stats returns a copy of the accumulated counters.
func (c *Comp) Stats() Stats {
	return c.stats
}

// InvalidatePage drops one page's mapping. A TLB shootdown reaches the FBT
// through this path in the paper; nothing drives it in this model yet.
func (c *Comp) InvalidatePage(pid vm.PID, vAddr uint64) {
	c.table.invalidate(pid, c.pageIDOf(vAddr))
}

// InvalidateAll drops every mapping, as an all-entry shootdown would.
func (c *Comp) InvalidateAll() {
	c.table.invalidateAll()
}

// pageIDOf returns the virtual page *number*, not the page-aligned address.
// The set index is this value modulo the set count, so an address would put
// every page in set 0 - page-aligned addresses are all multiples of the page
// size. This matches the shared TLB's own vAddrToSetID, which divides by the
// page size before taking the modulus.
func (c *Comp) pageIDOf(vAddr uint64) uint64 {
	return vAddr >> c.log2PageSize
}

var _ sim.Component = (*Comp)(nil)
