package tlb

import (
	"log" // sbin_claude_vc

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
	// topChannels are the independent top-side request classes. Channel 0 is
	// always present and its port is topPort. // sbin_claude_vc
	topChannels []*topChannel
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

	// bottomCommitsThisCycle counts the MSHR fills committed in the current
	// cycle. The multi-channel response path has no responding-entry
	// register to serialise on, so the fill rate is capped explicitly to
	// stay identical to the single-channel path. // sbin_claude_vc
	bottomCommitsThisCycle int

	inflightFlushReq *FlushReq

	// pendingCancels remembers canceled request IDs whose request has not
	// reached the MSHR yet; the request is dropped when it emerges from the
	// lookup pipeline. // sbin_claude_avatar
	pendingCancels map[string]struct{}
	// pendingBottomCancels are cancels bound for the downstream walker,
	// waiting for the bottom port. // sbin_claude_avatar
	pendingBottomCancels []*vm.TranslationCancelReq
}

// topChannel is one independent top-side request class. Each channel owns its
// port, its lookup pipeline and its pending-response queue, so a channel whose
// requester is back-pressured cannot stall the other channels.
//
// This exists because a TLB whose top port is shared by two client classes
// with different drain paths can deadlock: the shared, in-order port queue
// lets a blocked client of one class hold up every answer of the other class
// (head-of-line blocking in directconnection). Giving each class its own
// channel removes that coupling while keeping one TLB, one set array and one
// MSHR. // sbin_claude_vc
type topChannel struct {
	port     sim.Port
	pipeline pipelining.Pipeline
	buffer   sim.Buffer

	// pending holds the answers waiting for this channel's port. Only the
	// multi-channel response path uses it; with a single channel the
	// pre-existing respondingMSHREntry path is kept untouched so that
	// single-channel timing does not change. // sbin_claude_vc
	pending []pendingTopRsp
}

// pendingTopRsp is one page-walk answer waiting for its channel's top port.
// sbin_claude_vc
type pendingTopRsp struct {
	req  *vm.TranslationReq
	page vm.Page
}

// isMultiChannel reports whether the top side is split into more than one
// request class. // sbin_claude_vc
func (c *Comp) isMultiChannel() bool {
	return len(c.topChannels) > 1
}

// channelOf returns the channel a request arrived on. A request always names
// the top port it was addressed to, so the reply can be routed back out of
// the same port - and therefore over the same connection - as the request
// came in. // sbin_claude_vc
func (c *Comp) channelOf(req *vm.TranslationReq) *topChannel {
	dst := req.Meta().Dst

	for _, channel := range c.topChannels {
		if channel.port.AsRemote() == dst {
			return channel
		}
	}

	log.Panicf("%s received a request addressed to %s, which is not one of its top ports",
		c.Name(), dst)

	return nil
}

// hasPendingTopRsp reports whether any channel still owes an answer to its
// requesters. // sbin_claude_vc
func (c *Comp) hasPendingTopRsp() bool {
	for _, channel := range c.topChannels {
		if len(channel.pending) > 0 {
			return true
		}
	}

	return false
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
