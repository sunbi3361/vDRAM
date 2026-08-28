// sbin_claude_latpc: tests for the page walk queue (MICRO'25 Table 2 and
// Figure 10) - baseline GMMU hardware that LATP's PW Buffer tag check
// searches - and for the paper's PW Buffer address tag (Figure 15), which is
// the default tag. The narrower Regularity-Detector-group-ID tag is kept as
// an ablation and is pinned here too.
package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// newLATPKnobMiddleware builds a GMMU whose walkers, page walk queue depth
// and tag mode the caller picks. log2PageSize is real (4 KB) because the
// address tag computes base = VAddr - Stride*Index pages.
func newLATPKnobMiddleware(
	walkers, pwQueue int,
	addrTag bool,
) (*middleware, *fakeLATPPort) {
	comp := &Comp{
		latency:             10,
		log2PageSize:        12,
		maxRequestsInFlight: walkers,
		latpEnabled:         true,
		latpL4RowHitLatency: 2,
		pwQueueSize:         pwQueue,
		latpAddrTag:         addrTag,
		pageTable:           vm.NewPageTable(12),
	}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMULATPKnobTest", sim.NewSerialEngine(), sim.GHz, nil)

	port := &fakeLATPPort{}
	comp.topPort = port

	return &middleware{Comp: comp}, port
}

// busyLead is an in-flight walk leading group g, occupying one walker.
func busyLead(vAddr uint64, group string) transaction {
	return transaction{
		req:        groupReq(vAddr, group, 0, 0),
		state:      pageWalkCacheDone,
		groupBase:  vAddr,
		memberMask: 1,
	}
}

// TestPWQueueJoinsFromBehindTheHead is the whole point of the queue: with
// every walker busy, a member that sits behind a request waiting for a
// walker still reaches the PW Buffer tag check.
func TestPWQueueJoinsFromBehindTheHead(t *testing.T) {
	m, port := newLATPKnobMiddleware(1, 8, false)
	m.walkingTranslations = []transaction{busyLead(0x8000, "g")}

	blocker := groupReq(0x20000, "h", 0, 0) // a demand: cannot coalesce
	member := groupReq(0xa000, "g", 2, 1)
	port.incoming = []sim.Msg{blocker, member}

	if !m.parseFromTop() {
		t.Fatal("the queue made no progress")
	}
	if got := len(m.walkingTranslations[0].members); got != 1 {
		t.Fatalf("member count = %d, want 1", got)
	}
	if m.latpLookaheadJoins != 1 {
		t.Fatalf("lookahead joins = %d, want 1", m.latpLookaheadJoins)
	}
	if len(m.pwQueue) != 1 || m.pwQueue[0] != blocker {
		t.Fatal("the blocking demand should still be queued, in order")
	}
}

// TestOneEntryQueueLosesTheSameJoin pins what queue depth buys: with room
// for only the blocked demand, the member behind it never reaches the PW
// Buffer tag check and the cycle is charged as a head block. This is how the
// GMMU behaved before it had a queue at all.
func TestOneEntryQueueLosesTheSameJoin(t *testing.T) {
	m, port := newLATPKnobMiddleware(1, 1, false)
	m.walkingTranslations = []transaction{busyLead(0x8000, "g")}
	port.incoming = []sim.Msg{
		groupReq(0x20000, "h", 0, 0),
		groupReq(0xa000, "g", 2, 1),
	}

	m.parseFromTop()
	m.parseFromTop()

	if got := len(m.walkingTranslations[0].members); got != 0 {
		t.Fatalf("member count = %d, want 0 behind a one-entry queue", got)
	}
	if m.pwQueueHeadBlockTicks == 0 {
		t.Fatal("the blocked demand was not counted as a head block")
	}
}

// TestPWQueueAdmitsInOrder: only coalescing looks ahead. A new walk still
// starts from the head, so admission order is unchanged.
func TestPWQueueAdmitsInOrder(t *testing.T) {
	m, port := newLATPKnobMiddleware(2, 8, false)
	first := groupReq(0x20000, "h", 0, 0)
	port.incoming = []sim.Msg{first, groupReq(0x40000, "i", 0, 0)}

	if !m.parseFromTop() {
		t.Fatal("the queue made no progress")
	}
	if len(m.walkingTranslations) != 1 {
		t.Fatalf("started %d walks, want 1", len(m.walkingTranslations))
	}
	if m.walkingTranslations[0].req != first {
		t.Fatal("admission did not take the head of the queue")
	}
	if len(m.pwQueue) != 1 {
		t.Fatalf("queue holds %d requests, want 1", len(m.pwQueue))
	}
}

// TestPWQueueStopsRefillingAtCapacity keeps the modeled queue finite.
func TestPWQueueStopsRefillingAtCapacity(t *testing.T) {
	m, port := newLATPKnobMiddleware(0, 2, false)
	for i := 0; i < 5; i++ {
		port.incoming = append(port.incoming,
			groupReq(uint64(0x20000+i*0x1000), "h", 0, 0))
	}

	m.parseFromTop()

	if len(m.pwQueue) != 2 {
		t.Fatalf("queue depth = %d, want the 2-entry capacity",
			len(m.pwQueue))
	}
	if len(port.incoming) != 3 {
		t.Fatalf("%d requests left on the port, want 3", len(port.incoming))
	}
}

// TestPWQueueDropsCanceledRequests: a cancel that arrives after the request
// was pulled into the queue must still drop it, never start a walk.
func TestPWQueueDropsCanceledRequests(t *testing.T) {
	m, port := newLATPKnobMiddleware(2, 8, false)
	m.canceledReqs = map[string]struct{}{}

	canceled := groupReq(0x20000, "h", 0, 0)
	port.incoming = []sim.Msg{canceled}
	m.refillPWQueue()

	m.canceledReqs[canceled.ID] = struct{}{}

	if !m.parseFromTop() {
		t.Fatal("dropping a canceled request made no progress")
	}
	if len(m.walkingTranslations) != 0 {
		t.Fatal("a canceled request started a walk")
	}
	if len(m.pwQueue) != 0 {
		t.Fatal("a canceled request stayed in the queue")
	}
}

// TestAddressTagJoinsAcrossWarpInstructions is the tag deviation itself:
// the paper's entry holds only <Base Address, Stride, Valid Mask>, so a
// request from another warp instruction whose group arithmetic resolves to
// the same base shares the entry. The GroupID tag cannot express this.
func TestAddressTagJoinsAcrossWarpInstructions(t *testing.T) {
	m, _ := newLATPKnobMiddleware(2, 0, true)
	m.walkingTranslations = []transaction{busyLead(0x8000, "g")}

	// Base 0x8000, stride 2 pages, index 1 -> 0x8000 + 2*0x1000 = 0xa000.
	foreign := groupReq(0xa000, "OTHER-INSTRUCTION", 2, 1)

	if !m.tryJoinBatch(foreign) {
		t.Fatal("the address tag did not merge a foreign-group request")
	}
	if m.latpCrossGroupJoins != 1 {
		t.Fatalf("cross-group joins = %d, want 1", m.latpCrossGroupJoins)
	}

	// The same request is refused by the default GroupID tag.
	m2, _ := newLATPKnobMiddleware(2, 0, false)
	m2.walkingTranslations = []transaction{busyLead(0x8000, "g")}
	if m2.tryJoinBatch(groupReq(0xa000, "OTHER-INSTRUCTION", 2, 1)) {
		t.Fatal("the GroupID tag merged a foreign-group request")
	}
}

// TestAddressTagHonoursTheValidMask: one entry tracks each index once, and
// never more than the mask width.
func TestAddressTagHonoursTheValidMask(t *testing.T) {
	m, _ := newLATPKnobMiddleware(2, 0, true)
	m.walkingTranslations = []transaction{busyLead(0x8000, "g")}

	if !m.tryJoinBatch(groupReq(0xa000, "g", 2, 1)) {
		t.Fatal("the first member did not join")
	}
	if m.tryJoinBatch(groupReq(0xa000, "g", 2, 1)) {
		t.Fatal("the same index joined twice; the valid mask was ignored")
	}
	if got := len(m.walkingTranslations[0].members); got != 1 {
		t.Fatalf("member count = %d, want 1", got)
	}
}

// TestAddressTagAdmitsADemandOntoItsBase: Fig. 15 matches a demand with
// Index 0 against Base Address, which is how a group started by a lone
// prefetch picks up its own demand. Index 0 is taken here, so the demand of
// a different base must still be refused.
func TestAddressTagAdmitsADemandOntoItsBase(t *testing.T) {
	m, _ := newLATPKnobMiddleware(2, 0, true)
	// A lone prefetch leads: VAddr 0xa000, stride 2, index 1 -> base 0x8000,
	// mask bit 1 set, so the base VPN slot is still free.
	m.walkingTranslations = []transaction{{
		req:        groupReq(0xa000, "g", 2, 1),
		state:      pageWalkCacheDone,
		groupBase:  0x8000,
		memberMask: 1 << 1,
	}}

	if !m.tryJoinBatch(groupReq(0x8000, "g", 0, 0)) {
		t.Fatal("the group's own demand did not join its base slot")
	}
	if m.tryJoinBatch(groupReq(0x40000, "g", 0, 0)) {
		t.Fatal("a demand outside the group joined the entry")
	}
}

// TestStartWalkingSeedsTheEntryTag: the base and mask an entry is tagged
// with come from the request that opened it.
func TestStartWalkingSeedsTheEntryTag(t *testing.T) {
	m, _ := newLATPKnobMiddleware(2, 0, true)

	m.startWalking(groupReq(0xc000, "g", 2, 2), -1)

	trans := m.walkingTranslations[0]
	if trans.groupBase != 0x8000 {
		t.Fatalf("group base = %#x, want 0x8000", trans.groupBase)
	}
	if trans.memberMask != 1<<2 {
		t.Fatalf("valid mask = %#b, want bit 2", trans.memberMask)
	}
	if m.latpLonePrefetchWalks != 1 {
		t.Fatalf("lone prefetch walks = %d, want 1", m.latpLonePrefetchWalks)
	}
}
