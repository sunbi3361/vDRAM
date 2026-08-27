// sbin_claude_avatar
// Package asu implements the Avatar Speculation Unit: the GPU-side engine
// for speculative address translation with rapid validation (MICRO 2024,
// refs/avatar.md). It interposes between every L1 TLB bottom port and the
// shared L2 TLB top port, so each admitted TranslationReq is literally an
// L1 TLB miss (refs 5.3).
//
// For every miss the ASU forwards the request to the conventional
// L2-TLB/page-walk path unchanged. In parallel, it probes the per-requester
// MOD table; a confident entry speculates, and CAVA rapid validation is
// checked against the authoritative avatar metadata registry (functional
// truth) after a decompress-and-compare latency.
//
// The ASU issues no memory traffic of its own. refs 5.3 and 5.6 are explicit
// that CAST's speculative access IS the requester's data access - it is
// issued to `SpeculatedPPN || PageOffset` and its return both carries the
// data and validates the speculation - so the only cost Avatar adds over the
// access the CU was going to make anyway is decompressing the returned
// sector and comparing its embedded VPN. Modeling it as an extra fetch, as
// this unit used to, charged every speculation a whole second trip through
// L2/DRAM that no Avatar GPU ever makes. A validated speculation answers the
// L1 TLB early - Early TLB Fill: the L1 TLB fills the entry and releases
// its MSHR (refs 5.9) - and the now-redundant conventional translation is
// canceled out of band: the L2 TLB releases the MSHR entry and a queued,
// not-yet-started page walk is dropped at the GMMU (avatar-plan.md 5.2). A
// cancel that loses the race simply leaves an orphan response that is
// dropped here. A failed or unavailable validation (mis-speculation, or an
// uncompressed sector that cannot embed page information, refs 5.6 Case B)
// leaves the transaction waiting for the conventional translation.
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

	// sbin_claude_avatar v2 (avatar-plan.md 5.3): walk cancellation.
	//
	// Pre-edit v2 counters (commented per project convention). They belonged
	// to the separate sector fetch v3 removed; the speculative access is the
	// demand access now, so there is nothing extra to count:
	//   ValidationReads     uint64
	//   ValidationWaitCycles uint64
	//   StaleValidationRsps uint64
	SpecOutOfRange     uint64 // predicted PAddr outside GPU DRAM, dropped
	WalkCancelsSent    uint64 // EAF cancels sent to the L2 TLB
	ForwardsSuppressed uint64 // EAF beat the forward; no walk ever existed
	OrphanRsps         uint64 // responses whose transaction already closed
}

// transaction tracks one in-flight L1 TLB miss. The forward leg and the
// speculation leg progress independently, mirroring the refs 5.12 state
// machine; either may finish first and all late events must convert into
// safe no-ops.
type transaction struct {
	req   *vm.TranslationReq
	fwdID string

	fwdSent bool

	specActive bool   // speculation leg live
	specPAddr  uint64 // speculated page-aligned physical address

	// Pre-edit v2 fields (commented per project convention): the ASU used to
	// issue its own 64B sector fetch and wait for it.
	//   specReadPending bool
	//   specReadID      string
	//   specIssueCycle  uint64
	//
	// sbin_claude_avatar v3: CAVA's decompress-and-compare is all that is
	// left to time - the sector fetch it rides on is the requester's own
	// demand access (refs 5.3, 5.6).
	specCountdown bool // decompress+compare running
	specCycleLeft int

	earlyPending bool    // validated; waiting for the top port
	earlyPage    vm.Page // page to respond early with
	earlyDone    bool    // early response sent

	// cancelPending arranges the out-of-band walk cancel after EAF
	// (avatar-plan.md 5.2). // sbin_claude_avatar
	cancelPending bool

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
	// l2TLBCancelPort is the L2 TLB's out-of-band Cancel ingress
	// (avatar-plan.md 5.2). // sbin_claude_avatar
	l2TLBCancelPort sim.RemotePort

	// memLow and memHigh bound the GPU DRAM range so a wild prediction is
	// never handed out as a translation. // sbin_claude_avatar
	memLow  uint64
	memHigh uint64

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

// SetL2TLBCancelPort sets the L2 TLB Cancel ingress that receives the EAF
// walk cancels (avatar-plan.md 5.2). // sbin_claude_avatar
func (c *Comp) SetL2TLBCancelPort(port sim.RemotePort) {
	c.l2TLBCancelPort = port
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
