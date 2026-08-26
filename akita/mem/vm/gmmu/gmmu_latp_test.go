// sbin_claude_latpc: LATP batched-walk tests (MICRO'25 §5.4,
// refs/latpc-plan.md 3). A same-group request must join an in-flight walk
// without a walker slot, the lead must hold its slot while the batch drains,
// and each member must cost one L4 row-buffer hit.
package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// fakeLATPPort satisfies sim.Port for the few methods the LATP paths call.
// The embedded nil interface panics on anything un-overridden, which is the
// guard this test wants.
type fakeLATPPort struct {
	sim.Port
	incoming []sim.Msg
	sent     []sim.Msg
}

func (p *fakeLATPPort) CanSend() bool { return true }

func (p *fakeLATPPort) Send(msg sim.Msg) *sim.SendError {
	p.sent = append(p.sent, msg)
	return nil
}

func (p *fakeLATPPort) AsRemote() sim.RemotePort { return "FakeLATPTop" }

func (p *fakeLATPPort) Name() string { return "FakeLATPTop" }

func (p *fakeLATPPort) PeekIncoming() sim.Msg {
	if len(p.incoming) == 0 {
		return nil
	}
	return p.incoming[0]
}

func (p *fakeLATPPort) RetrieveIncoming() sim.Msg {
	if len(p.incoming) == 0 {
		return nil
	}
	msg := p.incoming[0]
	p.incoming = p.incoming[1:]
	return msg
}

func newLATPTestMiddleware() (*middleware, *fakeLATPPort) {
	comp := &Comp{
		latency:             10,
		maxRequestsInFlight: 2,
		latpEnabled:         true,
		latpL4RowHitLatency: 2,
		pageTable:           vm.NewPageTable(12),
	}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMULATPTest",
		sim.NewSerialEngine(),
		sim.GHz,
		nil,
	)

	port := &fakeLATPPort{}
	comp.topPort = port

	return &middleware{Comp: comp}, port
}

func groupReq(vAddr uint64, group string, stride int64, index int,
) *vm.TranslationReq {
	return vm.TranslationReqBuilder{}.
		WithPID(1).
		WithVAddr(vAddr).
		WithGroup(group, stride, index).
		Build()
}

func TestTryJoinBatchAttachesSameGroupMember(t *testing.T) {
	m, _ := newLATPTestMiddleware()
	m.walkingTranslations = []transaction{{
		req:   groupReq(0x8000, "g", 0, 0),
		state: pageWalkCacheDone,
	}}

	if !m.tryJoinBatch(groupReq(0xa000, "g", 2, 1)) {
		t.Fatal("a same-group prefetch did not join the in-flight walk")
	}
	if got := len(m.walkingTranslations[0].members); got != 1 {
		t.Fatalf("member count = %d, want 1", got)
	}
	if m.latpBatches != 1 {
		t.Fatalf("batch count = %d, want 1", m.latpBatches)
	}

	// A second member joins the same batch without recounting it.
	m.tryJoinBatch(groupReq(0xc000, "g", 2, 2))
	if m.latpBatches != 1 || m.latpBatchedMembers != 0 {
		t.Fatalf("stats = {batches %d, members %d}, want {1, 0} before drain",
			m.latpBatches, m.latpBatchedMembers)
	}
}

func TestTryJoinBatchRejectsDemandsAndForeignGroups(t *testing.T) {
	m, _ := newLATPTestMiddleware()
	m.walkingTranslations = []transaction{{
		req:   groupReq(0x8000, "g", 0, 0),
		state: pageWalkCacheDone,
	}}

	if m.tryJoinBatch(groupReq(0xa000, "g", 0, 0)) {
		t.Fatal("a demand request joined a batch; it must start its own walk")
	}
	if m.tryJoinBatch(groupReq(0xa000, "h", 2, 1)) {
		t.Fatal("a foreign-group request joined the batch")
	}
	if m.tryJoinBatch(groupReq(0xa000, "", 2, 1)) {
		t.Fatal("a request without a group joined the batch")
	}
}

func TestDoPageWalkHitHoldsSlotWhileMembersRemain(t *testing.T) {
	m, port := newLATPTestMiddleware()
	page := vm.Page{PID: 1, VAddr: 0x8000, PAddr: 0x100000, Valid: true}
	m.walkingTranslations = []transaction{{
		req:     groupReq(0x8000, "g", 0, 0),
		page:    page,
		state:   pageWalkComplete,
		members: []*vm.TranslationReq{groupReq(0xa000, "g", 2, 1)},
	}}

	if !m.doPageWalkHit(0) {
		t.Fatal("the lead's response did not make progress")
	}
	if got := m.walkingTranslations[0].state; got != batchDraining {
		t.Fatalf("state = %v, want batchDraining", got)
	}
	if got := m.walkingTranslations[0].drainCycleLeft; got != 2 {
		t.Fatalf("drain countdown = %d, want the row-hit latency 2", got)
	}
	if len(port.sent) != 1 {
		t.Fatalf("sent %d responses, want only the lead's", len(port.sent))
	}

	// Without members the lead retires immediately, as before LATP.
	m.walkingTranslations = []transaction{{
		req:   groupReq(0x8000, "g", 0, 0),
		page:  page,
		state: pageWalkComplete,
	}}
	m.doPageWalkHit(0)
	if got := m.walkingTranslations[0].state; got != transactionFinished {
		t.Fatalf("memberless state = %v, want transactionFinished", got)
	}
}

func TestDrainBatchMemberChargesOneRowHitPerMember(t *testing.T) {
	m, port := newLATPTestMiddleware()
	member1 := groupReq(0xa000, "g", 2, 1)
	member2 := groupReq(0xc000, "g", 2, 2)
	m.pageTable.Insert(vm.Page{
		PID: 1, VAddr: 0xa000, PAddr: 0x200000, Valid: true})
	m.pageTable.Insert(vm.Page{
		PID: 1, VAddr: 0xc000, PAddr: 0x300000, Valid: true})

	m.walkingTranslations = []transaction{{
		req:            groupReq(0x8000, "g", 0, 0),
		state:          batchDraining,
		drainCycleLeft: 2,
		members:        []*vm.TranslationReq{member1, member2},
	}}

	// Two countdown cycles, then the first member's response.
	for i := 0; i < 2; i++ {
		if !m.drainBatchMember(0) {
			t.Fatalf("countdown cycle %d did not make progress", i)
		}
		if len(port.sent) != 0 {
			t.Fatalf("a response left during the countdown (cycle %d)", i)
		}
	}
	if !m.drainBatchMember(0) {
		t.Fatal("the first member's response did not make progress")
	}

	if len(port.sent) != 1 {
		t.Fatalf("sent %d responses, want 1", len(port.sent))
	}
	rsp := port.sent[0].(*vm.TranslationRsp)
	if rsp.RespondTo != member1.ID {
		t.Fatal("the response does not answer the first member")
	}
	if rsp.Page.PAddr != 0x200000 {
		t.Fatalf("member page PAddr = %#x, want 0x200000", rsp.Page.PAddr)
	}
	if got := m.walkingTranslations[0].drainCycleLeft; got != 2 {
		t.Fatalf("countdown after first member = %d, want re-armed 2", got)
	}
	if got := m.walkingTranslations[0].state; got != batchDraining {
		t.Fatalf("state = %v, want batchDraining until the last member", got)
	}

	// Drain the second member.
	m.drainBatchMember(0)
	m.drainBatchMember(0)
	m.drainBatchMember(0)

	if len(port.sent) != 2 {
		t.Fatalf("sent %d responses, want 2", len(port.sent))
	}
	if got := m.walkingTranslations[0].state; got != transactionFinished {
		t.Fatalf("state = %v, want transactionFinished", got)
	}
	if m.latpBatchedMembers != 2 {
		t.Fatalf("batched members = %d, want 2", m.latpBatchedMembers)
	}
}

func TestDrainBatchMemberRefusesManagedPages(t *testing.T) {
	m, _ := newLATPTestMiddleware()
	member := groupReq(0xa000, "g", 2, 1)
	m.pageTable.Insert(vm.Page{
		PID: 1, VAddr: 0xa000, PAddr: 0x200000, Valid: true, Managed: true})
	m.walkingTranslations = []transaction{{
		req:     groupReq(0x8000, "g", 0, 0),
		state:   batchDraining,
		members: []*vm.TranslationReq{member},
	}}

	defer func() {
		if recover() == nil {
			t.Fatal("a managed page was drained; LATP is non-UVM only")
		}
	}()
	m.drainBatchMember(0)
}

func TestParseFromTopJoinsEvenWhenWalkersAreFull(t *testing.T) {
	m, port := newLATPTestMiddleware()
	// Both walker slots are taken; the lead of group "g" is one of them.
	m.walkingTranslations = []transaction{
		{req: groupReq(0x8000, "g", 0, 0), state: pageWalkCacheDone},
		{req: groupReq(0x40000, "h", 0, 0), state: pageWalkCacheDone},
	}

	member := groupReq(0xa000, "g", 2, 1)
	port.incoming = []sim.Msg{member}

	if !m.parseFromTop() {
		t.Fatal("a joinable member was blocked by the walker-slot cap")
	}
	if got := len(m.walkingTranslations); got != 2 {
		t.Fatalf("walker slots = %d, want the member to take none", got)
	}
	if got := len(m.walkingTranslations[0].members); got != 1 {
		t.Fatalf("member count = %d, want 1", got)
	}

	// A demand of a new group stays blocked at the cap, as before.
	port.incoming = []sim.Msg{groupReq(0x80000, "k", 0, 0)}
	if m.parseFromTop() {
		t.Fatal("a new-group demand was admitted past the walker-slot cap")
	}
}
