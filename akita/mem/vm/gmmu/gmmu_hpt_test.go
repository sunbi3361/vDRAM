// sbin_claude_hpt: FS-HPT (PACT'24) walk-mode tests. The hashed walk must
// charge one memory reference per access, never touch the page-walk cache,
// and rejoin the radix path at pageWalkComplete so UVM gating stays intact.
package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

func newHPTTestMiddleware(accessesPerWalk int) *middleware {
	comp := &Comp{
		latency:            10,
		hptEnabled:         true,
		hptAccessesPerWalk: accessesPerWalk,
	}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUHPTTest",
		sim.NewSerialEngine(),
		sim.GHz,
		nil,
	)

	return &middleware{Comp: comp}
}

func TestStartHashedWalkChargesOneMemoryAccess(t *testing.T) {
	// Given
	m := newHPTTestMiddleware(1)
	m.walkingTranslations = []transaction{{
		req:       &vm.TranslationReq{},
		level:     pageTableLevels - 1,
		fillLevel: -1,
		state:     newTransaction,
	}}

	// When
	madeProgress := m.startHashedWalk(0)

	// Then
	if !madeProgress {
		t.Fatal("starting a hashed walk did not make progress")
	}

	trans := m.walkingTranslations[0]
	if trans.cycleLeft != 10 {
		t.Fatalf("walk cycles = %d, want 10", trans.cycleLeft)
	}
	if trans.level != 0 {
		t.Fatalf("walk level = %d, want 0", trans.level)
	}
	if trans.fillLevel != -1 {
		t.Fatalf("fill level = %d, want -1", trans.fillLevel)
	}
	if trans.state != pageWalkCacheDone {
		t.Fatalf("state = %v, want the countdown state", trans.state)
	}
	if m.hptWalks != 1 || m.hptMemoryAccesses != 1 {
		t.Fatalf("stats = {walks %d, accesses %d}, want {1, 1}",
			m.hptWalks, m.hptMemoryAccesses)
	}
}

func TestStartHashedWalkScalesWithAccessesPerWalk(t *testing.T) {
	// Given a five-access collision chain. // sbin_claude: no longer the
	// length of a full radix walk, which is 4 levels under the target spec.
	m := newHPTTestMiddleware(5)
	m.walkingTranslations = []transaction{{
		req:   &vm.TranslationReq{},
		state: newTransaction,
	}}

	// When
	m.startHashedWalk(0)

	// Then
	if got := m.walkingTranslations[0].cycleLeft; got != 50 {
		t.Fatalf("walk cycles = %d, want 50", got)
	}
	if m.hptMemoryAccesses != 5 {
		t.Fatalf("memory accesses = %d, want 5", m.hptMemoryAccesses)
	}
}

// The hashed walk must reach pageWalkComplete without ever sending a
// page-walk-cache message. A nil cache port proves it: any send would panic.
func TestHashedWalkNeverTouchesThePageWalkCache(t *testing.T) {
	// Given
	m := newHPTTestMiddleware(1)
	m.walkingTranslations = []transaction{{
		req:   &vm.TranslationReq{},
		state: newTransaction,
	}}
	m.startHashedWalk(0)

	// When the countdown drains.
	for i := 0; i < 10; i++ {
		if m.walkingTranslations[0].state != pageWalkCacheDone {
			t.Fatalf("walk left the countdown after %d cycles", i)
		}
		m.advancePageWalk(0)
	}
	m.advancePageWalk(0)

	// Then
	if got := m.walkingTranslations[0].state; got != pageWalkComplete {
		t.Fatalf("state = %v, want pageWalkComplete", got)
	}
}

func TestParseFromPageWalkCacheToleratesAbsentCache(t *testing.T) {
	// Given a GMMU in HPT mode, which builds no page-walk cache.
	m := newHPTTestMiddleware(1)

	// When / Then: draining the absent port is a no-op, not a panic.
	if m.parseFromPageWalkCache() {
		t.Fatal("absent page-walk cache reported progress")
	}
}

func TestBuilderSkipsPageWalkCacheInHPTMode(t *testing.T) {
	build := func(hpt bool) *Comp {
		return MakeBuilder().
			WithEngine(sim.NewSerialEngine()).
			WithFreq(sim.GHz).
			WithPageTable(vm.NewPageTable(12)).
			WithHashedPageTable(hpt).
			Build("GMMUBuilderTest")
	}

	if c := build(true); c.pageWalkCachePort != nil {
		t.Fatal("HPT mode built a page-walk cache")
	}
	if c := build(false); c.pageWalkCachePort == nil {
		t.Fatal("radix mode did not build a page-walk cache")
	}
}

func TestBuilderRejectsZeroAccessesPerWalk(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a zero-access hashed walk was accepted")
		}
	}()

	MakeBuilder().
		WithEngine(sim.NewSerialEngine()).
		WithFreq(sim.GHz).
		WithPageTable(vm.NewPageTable(12)).
		WithHashedPageTable(true).
		WithHPTAccessesPerWalk(0).
		Build("GMMUBuilderRejectTest")
}

// Regression guard: the flag actually selects between two different paths.
// Both GMMUs are fully built, so the radix one has a working cache port and
// takes the lookup path while the hashed one goes straight to the countdown.
func TestWalkPageTableSelectsThePathByFlag(t *testing.T) {
	firstStateAfterOneTick := func(hpt bool) transactionState {
		comp := MakeBuilder().
			WithEngine(sim.NewSerialEngine()).
			WithFreq(sim.GHz).
			WithPageTable(vm.NewPageTable(12)).
			WithHashedPageTable(hpt).
			Build("GMMUPathTest")

		m := &middleware{Comp: comp}
		m.walkingTranslations = []transaction{{
			req:   &vm.TranslationReq{},
			level: pageTableLevels - 1,
			state: newTransaction,
		}}
		m.walkPageTable()

		return m.walkingTranslations[0].state
	}

	if got := firstStateAfterOneTick(false); got != sentToPageWalkCache {
		t.Fatalf("radix state = %v, want sentToPageWalkCache", got)
	}
	if got := firstStateAfterOneTick(true); got != pageWalkCacheDone {
		t.Fatalf("hashed state = %v, want the countdown state", got)
	}
}
