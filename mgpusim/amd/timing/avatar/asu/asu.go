// sbin_claude_avatar
// Package asu implements the Avatar Speculation Unit: the GPU-side engine
// for speculative address translation with rapid validation (MICRO 2024,
// refs/avatar.md). It interposes between every L1 TLB bottom port and the
// shared L2 TLB top port, so each admitted TranslationReq is literally an
// L1 TLB miss (refs 5.3).
//
// For every miss the ASU forwards the request to the conventional
// L2-TLB/page-walk path unchanged. In parallel, it probes the per-requester
// MOD table; a confident entry launches a speculative access whose CAVA
// rapid validation is modeled as a configurable latency followed by a check
// against the authoritative avatar metadata registry (the same
// latency-model/functional-truth split the GMMU and the Utopia UTU use). A
// validated speculation answers the L1 TLB early - Early TLB Fill: the L1
// TLB fills the entry and releases its MSHR (refs 5.9) - and the late real
// response is swallowed so no request completes twice (refs 5.12). A failed
// or unavailable validation (mis-speculation, or an uncompressed sector
// that cannot embed page information, refs 5.6 Case B) leaves the
// transaction waiting for the conventional translation.
//
// Modeled simplifications (avatar-plan.md 1.6): speculative fetches issue
// no real data requests (no cache pollution or guarantee bits), and EAF
// does not cancel the in-flight page walk.
package asu

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// Stats counts the observable speculation behavior.
type Stats struct {
	Forwarded          uint64 // L1 misses admitted and forwarded down
	Speculations       uint64 // MOD confident, speculative access launched
	CAVAPass           uint64 // rapid validation succeeded
	CAVAMismatch       uint64 // embedded metadata exposed a mis-speculation
	CAVAIncompressible uint64 // uncompressed sector, validation impossible
	CAVANoMetadata     uint64 // frame never held an installed page
	EarlyCompletions   uint64 // translations answered before the real path
	RealResponseFirst  uint64 // real translation beat the speculation
	SwallowedRsps      uint64 // late real responses dropped after EAF
	PageTableVetoes    uint64 // CAVA pass rejected by the page-table check
}

// transaction tracks one in-flight L1 TLB miss. The forward leg and the
// speculation leg progress independently, mirroring the refs 5.12 state
// machine; either may finish first and all late events must convert into
// safe no-ops.
type transaction struct {
	req   *vm.TranslationReq
	fwdID string

	fwdSent bool

	specActive    bool // validation countdown running
	specCycleLeft int
	specPAddr     uint64 // speculated page-aligned physical address

	earlyPending bool    // validated; waiting for the top port
	earlyPage    vm.Page // page to respond early with
	earlyDone    bool    // early response sent; swallow the real response

	realRsp *vm.TranslationRsp // real response waiting for the top port

	done bool
}

// Comp is the Avatar Speculation Unit component.
type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder

	topPort    sim.Port // faces the L1 TLB bottom ports
	bottomPort sim.Port // faces the shared L2 TLB top port

	l2TLBPort sim.RemotePort // destination of forwarded translations

	registry     *meta.Registry
	pageTable    vm.PageTable
	deviceID     uint64
	log2PageSize uint64

	validationLatency   int
	modNumEntries       int
	confidenceThreshold int
	maxReqInFlight      int
	numReqPerCycle      int

	// mods holds one MOD table per requesting L1 TLB (per-CU, refs 5.2),
	// keyed by the source port name.
	mods map[sim.RemotePort]*modTable

	transactions []transaction

	stats Stats
}

// Tick runs the middleware chain.
func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// SetL2TLBPort sets the L2 TLB top port that serves forwarded translations.
func (c *Comp) SetL2TLBPort(port sim.RemotePort) {
	c.l2TLBPort = port
}

// TopPort returns the port facing the L1 TLB bottoms.
func (c *Comp) TopPort() sim.Port {
	return c.topPort
}

// BottomPort returns the port facing the shared L2 TLB.
func (c *Comp) BottomPort() sim.Port {
	return c.bottomPort
}

// Stats returns a copy of the accumulated counters.
func (c *Comp) Stats() Stats {
	return c.stats
}

func (c *Comp) modOf(src sim.RemotePort) *modTable {
	mod, found := c.mods[src]
	if !found {
		mod = newModTable(c.modNumEntries, c.confidenceThreshold)
		c.mods[src] = mod
	}

	return mod
}

var _ sim.Component = (*Comp)(nil)
