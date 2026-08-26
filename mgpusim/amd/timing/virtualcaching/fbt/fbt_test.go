// sbin_claude_fbt
package fbt

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// agent stands in for the L2 TLB above the FBT and the page walker below it.
type agent struct {
	*sim.TickingComponent

	port     sim.Port
	received []sim.Msg
}

func newAgent(engine sim.Engine, name string) *agent {
	a := &agent{}
	a.TickingComponent = sim.NewTickingComponent(name, engine, 1*sim.GHz, a)
	a.port = sim.NewPort(a, 16, 16, name+".Port")
	a.AddPort("Port", a.port)

	return a
}

func (a *agent) Tick() bool {
	progress := false

	for {
		msg := a.port.RetrieveIncoming()
		if msg == nil {
			break
		}

		a.received = append(a.received, msg)
		progress = true
	}

	return progress
}

// harness wires requester -> FBT -> walker with zero-latency connections and
// drives every component from one clock.
type harness struct {
	t         *testing.T
	comp      *Comp
	requester *agent
	walker    *agent
	conns     []*directconnection.Comp
}

func newHarness(t *testing.T, build func(Builder) Builder) *harness {
	t.Helper()

	engine := sim.NewSerialEngine()
	requester := newAgent(engine, "Requester")
	walker := newAgent(engine, "Walker")

	builder := MakeBuilder().WithEngine(engine)
	if build != nil {
		builder = build(builder)
	}

	comp := builder.Build("FBT")
	comp.SetPageWalker(walker.port.AsRemote())

	h := &harness{t: t, comp: comp, requester: requester, walker: walker}
	h.connect("TopConn", comp.GetPortByName("Top"), requester.port, engine)
	h.connect("BottomConn", comp.GetPortByName("Bottom"), walker.port, engine)

	return h
}

func (h *harness) connect(name string, a, b sim.Port, engine sim.Engine) {
	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build(name)
	conn.PlugIn(a)
	conn.PlugIn(b)
	h.conns = append(h.conns, conn)
}

// tick advances every component once, in the order a cycle would.
func (h *harness) tick() {
	h.comp.Tick()

	for _, conn := range h.conns {
		conn.Tick()
	}

	h.requester.Tick()
	h.walker.Tick()
}

func (h *harness) run(cycles int) {
	for range cycles {
		h.tick()
	}
}

func (h *harness) request(vAddr uint64) *vm.TranslationReq {
	h.t.Helper()

	req := vm.TranslationReqBuilder{}.
		WithSrc(h.requester.port.AsRemote()).
		WithDst(h.comp.GetPortByName("Top").AsRemote()).
		WithPID(1).
		WithVAddr(vAddr).
		WithDeviceID(1).
		Build()

	if err := h.comp.GetPortByName("Top").Deliver(req); err != nil {
		h.t.Fatalf("delivering request: %v", err)
	}

	return req
}

// answerWalk replies to the single outstanding walk with the given mapping.
func (h *harness) answerWalk(pAddr uint64) {
	h.t.Helper()

	if len(h.walker.received) != 1 {
		h.t.Fatalf("expected exactly 1 walk to answer, got %d",
			len(h.walker.received))
	}

	fwd := h.walker.received[0].(*vm.TranslationReq)
	h.walker.received = nil

	page := vm.Page{
		PID:      fwd.PID,
		VAddr:    fwd.VAddr &^ 0xfff,
		PAddr:    pAddr &^ 0xfff,
		PageSize: 4096,
		Valid:    true,
	}

	rsp := fwd.GenerateRsp(page).(*vm.TranslationRsp)
	if err := h.comp.GetPortByName("Bottom").Deliver(rsp); err != nil {
		h.t.Fatalf("delivering walk response: %v", err)
	}
}

// lastTranslation returns the physical page of the most recent answer and
// clears the requester's inbox.
func (h *harness) lastTranslation() uint64 {
	h.t.Helper()

	if len(h.requester.received) != 1 {
		h.t.Fatalf("expected exactly 1 translation, got %d",
			len(h.requester.received))
	}

	rsp := h.requester.received[0].(*vm.TranslationRsp)
	h.requester.received = nil

	return rsp.Page.PAddr
}

// TestMissWalksAndInstalls checks the miss path: the request reaches the page
// walker, and the answer is both relayed upward and recorded.
func TestMissWalksAndInstalls(t *testing.T) {
	h := newHarness(t, nil)

	h.request(0x1000)
	h.run(20)

	if len(h.walker.received) != 1 {
		t.Fatalf("expected the miss to reach the walker, got %d requests",
			len(h.walker.received))
	}

	h.answerWalk(0x8000)
	h.run(20)

	if got := h.lastTranslation(); got != 0x8000 {
		t.Fatalf("translation = %#x, want 0x8000", got)
	}

	stats := h.comp.Stats()
	if stats.Misses != 1 || stats.Hits != 0 || stats.Installs != 1 {
		t.Fatalf("stats = %+v, want 1 miss, 0 hits, 1 install", stats)
	}
}

// TestHitAvoidsWalk is the property the FBT exists for as a second-level TLB:
// a page it already tracks is answered without letting a page walk start.
func TestHitAvoidsWalk(t *testing.T) {
	h := newHarness(t, nil)

	h.request(0x1000)
	h.run(20)
	h.answerWalk(0x8000)
	h.run(20)
	h.lastTranslation()

	// A second access to another line of the same page.
	h.request(0x1abc)
	h.run(20)

	if len(h.walker.received) != 0 {
		t.Fatalf("a tracked page must not reach the walker, got %d requests",
			len(h.walker.received))
	}

	if got := h.lastTranslation(); got != 0x8000 {
		t.Fatalf("translation = %#x, want 0x8000", got)
	}

	stats := h.comp.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("stats = %+v, want 1 hit and 1 miss", stats)
	}
}

// TestLookupLatencyIsCharged checks that an answer is not produced before the
// configured table access has elapsed.
func TestLookupLatencyIsCharged(t *testing.T) {
	h := newHarness(t, func(b Builder) Builder {
		return b.WithLookupLatency(7)
	})

	h.request(0x1000)
	h.run(3)

	if len(h.walker.received) != 0 {
		t.Fatal("the lookup must not resolve before its latency elapses")
	}

	h.run(20)

	if len(h.walker.received) != 1 {
		t.Fatalf("expected the miss to reach the walker, got %d requests",
			len(h.walker.received))
	}
}

// TestEvictionDropsTheOldestPage pins the LRU replacement: with a
// single-entry set, a third page displaces the least recently used one, which
// then has to walk again.
func TestEvictionDropsTheOldestPage(t *testing.T) {
	h := newHarness(t, func(b Builder) Builder {
		return b.WithNumEntries(2).WithNumWays(2)
	})

	// Three pages that all map to the single set.
	for i, vAddr := range []uint64{0x1000, 0x2000, 0x3000} {
		h.request(vAddr)
		h.run(20)
		h.answerWalk(uint64(0x8000 + i*0x1000))
		h.run(20)
		h.lastTranslation()
	}

	if got := h.comp.Stats().Evictions; got != 1 {
		t.Fatalf("evictions = %d, want 1", got)
	}

	// The first page was evicted, so it must walk again.
	h.request(0x1000)
	h.run(20)

	if len(h.walker.received) != 1 {
		t.Fatalf("an evicted page must walk again, got %d requests",
			len(h.walker.received))
	}
}

// TestConsecutivePagesUseDifferentSets guards the set index. Indexing by the
// page-aligned address instead of the page number sends every page to set 0,
// which silently shrinks the table to one set's worth of ways.
func TestConsecutivePagesUseDifferentSets(t *testing.T) {
	// Two sets, one way each: the two pages can only both stay resident if
	// they index different sets.
	h := newHarness(t, func(b Builder) Builder {
		return b.WithNumEntries(2).WithNumWays(1)
	})

	for i, vAddr := range []uint64{0x1000, 0x2000} {
		h.request(vAddr)
		h.run(20)
		h.answerWalk(uint64(0x8000 + i*0x1000))
		h.run(20)
		h.lastTranslation()
	}

	if got := h.comp.Stats().Evictions; got != 0 {
		t.Fatalf("evictions = %d, want 0: the pages share a set", got)
	}

	h.request(0x1000)
	h.run(20)

	if len(h.walker.received) != 0 {
		t.Fatal("both pages should still be resident, so neither may walk")
	}
}

// TestInvalidatePageForcesAWalk covers the shootdown hook.
func TestInvalidatePageForcesAWalk(t *testing.T) {
	h := newHarness(t, nil)

	h.request(0x1000)
	h.run(20)
	h.answerWalk(0x8000)
	h.run(20)
	h.lastTranslation()

	h.comp.InvalidatePage(1, 0x1000)

	h.request(0x1000)
	h.run(20)

	if len(h.walker.received) != 1 {
		t.Fatalf("an invalidated page must walk again, got %d requests",
			len(h.walker.received))
	}
}
