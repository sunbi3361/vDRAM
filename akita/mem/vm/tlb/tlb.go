package tlb

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm" // sbin_codex
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

const (
	tlbStateEnable = 0
	tlbStatePause  = 1
	tlbStateDrain  = 2
	tlbStateFlush  = 3
)

// Comp is a Translation Lookaside Buffer (TLB) that stores part of the page
// table.
type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	topPort     sim.Port
	bottomPort  sim.Port
	controlPort sim.Port
	// cancelPort receives out-of-band TranslationCancelReqs (Avatar EAF,
	// refs/avatar.md 5.9). Out of band, because an in-band cancel could
	// never overtake the queued request it names. Unconnected and inert on
	// non-Avatar platforms. // sbin_claude_avatar
	cancelPort sim.Port

	addressMapper mem.AddressToPortMapper
	// walkCancelDst is the downstream Cancel port (the GMMU's) that receives
	// a cancel when a released MSHR entry leaves its page walk orphaned.
	// Empty when no downstream cancellation is wired. // sbin_claude_avatar
	walkCancelDst sim.RemotePort

	numSets                int
	numWays                int
	pageSize               uint64
	numReqPerCycle         int
	state                  int
	pageAdmissionPredicate func(vm.Page) bool // sbin_codex

	sets []internal.Set

	mshr                mshr
	respondingMSHREntry *mshrEntry
	responsePipeline    pipelining.Pipeline
	responseBuffer      sim.Buffer

	inflightFlushReq *FlushReq

	// pendingCancels remembers canceled request IDs whose request has not
	// reached the MSHR yet; the request is dropped when it emerges from the
	// lookup pipeline. // sbin_claude_avatar
	pendingCancels map[string]struct{}
	// pendingBottomCancels are cancels bound for the downstream walker,
	// waiting for the bottom port. // sbin_claude_avatar
	pendingBottomCancels []*vm.TranslationCancelReq
}

// SetWalkCancelProvider sets the downstream Cancel port that is told to
// abandon a page walk once its MSHR entry is released (Avatar EAF).
// sbin_claude_avatar
func (c *Comp) SetWalkCancelProvider(dst sim.RemotePort) {
	c.walkCancelDst = dst
}

// reset sets all the entries in the TLB to be invalid
func (c *Comp) reset() {
	c.sets = make([]internal.Set, c.numSets)
	for i := 0; i < c.numSets; i++ {
		set := internal.NewSet(c.numWays)
		c.sets[i] = set
	}
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}
