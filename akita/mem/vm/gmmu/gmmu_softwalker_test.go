// sbin_claude_softwalker: SoftWalker (MICRO'25) walk-mode tests. Admission
// must distribute walks round-robin over per-core SoftPWB slots, each walk
// must be priced as the radix walk plus comm+setup+per-level overheads, and
// finishing walks must free their slots.
package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

func newSWTestMiddleware(numCores, slotsPerCore int) *middleware {
	comp := &Comp{
		latency:   100,
		swEnabled: true,
		swConfig: SoftwareWalkConfig{
			NumCores:       numCores,
			SlotsPerCore:   slotsPerCore,
			CommCycles:     10,
			SetupCycles:    20,
			PerLevelCycles: 8,
		},
		swCoreInFlight: make([]int, numCores),
	}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUSWTest",
		sim.NewSerialEngine(),
		sim.GHz,
		nil,
	)

	return &middleware{Comp: comp}
}

func TestSWAssignCoreRoundRobin(t *testing.T) {
	m := newSWTestMiddleware(2, 2)

	want := []int{0, 1, 0, 1}
	for i, expected := range want {
		core, ok := m.swAssignCore()
		if !ok {
			t.Fatalf("assignment %d refused with free slots", i)
		}
		if core != expected {
			t.Fatalf("assignment %d picked core %d, want %d", i, core, expected)
		}
	}

	if m.swCoreInFlight[0] != 2 || m.swCoreInFlight[1] != 2 {
		t.Fatalf("in-flight = %v, want [2 2]", m.swCoreInFlight)
	}
	if m.swWalkCount != 4 {
		t.Fatalf("walk count = %d, want 4", m.swWalkCount)
	}
}

func TestSWAssignCoreRefusesWhenAllSlotsBusy(t *testing.T) {
	m := newSWTestMiddleware(2, 1)

	m.swAssignCore()
	m.swAssignCore()

	if _, ok := m.swAssignCore(); ok {
		t.Fatal("assignment succeeded with every slot busy")
	}
}

func TestSWAssignCoreSkipsFullCores(t *testing.T) {
	m := newSWTestMiddleware(2, 2)
	m.swCoreInFlight[0] = 2 // core 0 saturated

	for i := 0; i < 2; i++ {
		core, ok := m.swAssignCore()
		if !ok {
			t.Fatalf("assignment %d refused while core 1 had slots", i)
		}
		if core != 1 {
			t.Fatalf("assignment %d picked core %d, want 1", i, core)
		}
	}
}

func TestSWReleaseCoreFreesSlot(t *testing.T) {
	m := newSWTestMiddleware(1, 1)

	core, _ := m.swAssignCore()
	trans := transaction{swCore: core}

	if _, ok := m.swAssignCore(); ok {
		t.Fatal("assignment succeeded before the slot was released")
	}

	m.swReleaseCore(&trans)

	if trans.swCore != -1 {
		t.Fatalf("released transaction still names core %d", trans.swCore)
	}
	if _, ok := m.swAssignCore(); !ok {
		t.Fatal("assignment refused after the slot was released")
	}

	// Releasing again must be a no-op, not a double decrement.
	m.swReleaseCore(&trans)
	if m.swCoreInFlight[0] != 1 {
		t.Fatalf("in-flight = %d after double release, want 1",
			m.swCoreInFlight[0])
	}
}

func TestSWReleaseCoreIsInertWhenModeOff(t *testing.T) {
	comp := &Comp{}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUSWOffTest", sim.NewSerialEngine(), sim.GHz, nil)
	m := &middleware{Comp: comp}

	// A zero-value transaction (swCore 0) built outside startWalking must
	// not be mistaken for a slot holder.
	m.swReleaseCore(&transaction{})
}

func TestWalkCyclesChargesSoftwareOverheads(t *testing.T) {
	m := newSWTestMiddleware(1, 1)

	// sbin_claude: the modeled page table is 4-level (target spec), so a full
	// walk is 4x(100) memory + 2x10 comm + 20 setup + 4x8 instruction cycles.
	if got := m.walkCycles(pageTableLevels); got != 472 {
		t.Fatalf("full software walk = %d cycles, want 472", got)
	}
	if m.swExtraCyclesTotal != 72 {
		t.Fatalf("extra cycles = %d, want 72", m.swExtraCyclesTotal)
	}

	// A PWC hit that leaves one level still pays the fixed overheads.
	if got := m.walkCycles(1); got != 148 {
		t.Fatalf("one-level software walk = %d cycles, want 148", got)
	}
}

func TestWalkCyclesMatchesBaselineWhenModeOff(t *testing.T) {
	comp := &Comp{latency: 100}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUSWBaseTest", sim.NewSerialEngine(), sim.GHz, nil)
	m := &middleware{Comp: comp}

	// sbin_claude: 4-level page table (target spec).
	if got := m.walkCycles(pageTableLevels); got != 400 {
		t.Fatalf("baseline walk = %d cycles, want 400", got)
	}
	if m.swExtraCyclesTotal != 0 {
		t.Fatalf("baseline charged %d extra cycles", m.swExtraCyclesTotal)
	}
}

func TestStartWalkingCarriesTheAssignedCore(t *testing.T) {
	m := newSWTestMiddleware(4, 1)

	m.startWalking(&vm.TranslationReq{}, 3)

	if got := m.walkingTranslations[0].swCore; got != 3 {
		t.Fatalf("transaction carries core %d, want 3", got)
	}
}

func TestBuilderConfiguresSoftwareWalk(t *testing.T) {
	comp := MakeBuilder().
		WithEngine(sim.NewSerialEngine()).
		WithFreq(sim.GHz).
		WithPageTable(vm.NewPageTable(12)).
		WithSoftwareWalk(SoftwareWalkConfig{
			NumCores:       64,
			SlotsPerCore:   32,
			CommCycles:     10,
			SetupCycles:    20,
			PerLevelCycles: 8,
		}).
		Build("GMMUSWBuilderTest")

	if !comp.SoftwareWalkEnabled() {
		t.Fatal("software walk not enabled")
	}
	if len(comp.swCoreInFlight) != 64 {
		t.Fatalf("core counters = %d, want 64", len(comp.swCoreInFlight))
	}
	// The software walk keeps the radix path and therefore the page-walk
	// cache - unlike HPT.
	if comp.pageWalkCachePort == nil {
		t.Fatal("software-walk mode lost the page-walk cache")
	}
}

func TestBuilderRejectsSoftwareWalkWithHPT(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("software walk + HPT was accepted")
		}
	}()

	MakeBuilder().
		WithEngine(sim.NewSerialEngine()).
		WithFreq(sim.GHz).
		WithPageTable(vm.NewPageTable(12)).
		WithHashedPageTable(true).
		WithSoftwareWalk(SoftwareWalkConfig{NumCores: 1, SlotsPerCore: 1}).
		Build("GMMUSWHPTRejectTest")
}

func TestBuilderRejectsZeroCoreSoftwareWalk(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a zero-core software walk was accepted")
		}
	}()

	MakeBuilder().
		WithEngine(sim.NewSerialEngine()).
		WithFreq(sim.GHz).
		WithPageTable(vm.NewPageTable(12)).
		WithSoftwareWalk(SoftwareWalkConfig{NumCores: 0, SlotsPerCore: 32}).
		Build("GMMUSWRejectTest")
}

// The merge of worktree-latpc turned the "else if" that bypasses the
// hardware walker cap into a standalone check, which silently pinned the
// software walkers back to maxRequestsInFlight and leaked a PW-warp slot on
// every refusal. These pin the admission contract down.
// sbin_claude_softwalker
func TestAdmitWalkBypassesHardwareWalkerCap(t *testing.T) {
	m := newSWTestMiddleware(2, 2)
	m.maxRequestsInFlight = 2
	m.walkingTranslations = make([]transaction, 2) // cap already reached

	core, admitted := m.admitWalk()
	if !admitted {
		t.Fatal("software-walk admission refused at the hardware walker cap")
	}
	if core != 0 {
		t.Fatalf("admitted on core %d, want 0", core)
	}
	if m.swCoreInFlight[0] != 1 {
		t.Fatalf("core 0 in-flight = %d, want 1", m.swCoreInFlight[0])
	}
}

func TestAdmitWalkHonorsHardwareWalkerCapWhenModeOff(t *testing.T) {
	m := newSWTestMiddleware(2, 2)
	m.swEnabled = false
	m.maxRequestsInFlight = 2
	m.walkingTranslations = make([]transaction, 2)

	core, admitted := m.admitWalk()
	if admitted {
		t.Fatal("hardware-walk admission ignored maxRequestsInFlight")
	}
	if core != -1 {
		t.Fatalf("refused admission returned core %d, want -1", core)
	}

	m.walkingTranslations = make([]transaction, 1)

	if core, admitted = m.admitWalk(); !admitted || core != -1 {
		t.Fatalf("below the cap: core %d admitted %v, want -1 true",
			core, admitted)
	}
}

func TestAdmitWalkLeaksNoSlotWhenRefused(t *testing.T) {
	m := newSWTestMiddleware(2, 1)
	m.maxRequestsInFlight = 1
	m.swCoreInFlight[0] = 1 // every SoftPWB slot busy
	m.swCoreInFlight[1] = 1

	for i := 0; i < 4; i++ {
		if core, admitted := m.admitWalk(); admitted {
			t.Fatalf("admission %d granted core %d with no free slot",
				i, core)
		}
	}

	if m.swCoreInFlight[0] != 1 || m.swCoreInFlight[1] != 1 {
		t.Fatalf("in-flight = %v, want [1 1]", m.swCoreInFlight)
	}
	if m.swAdmissionBlockedTicks != 4 {
		t.Fatalf("blocked ticks = %d, want 4", m.swAdmissionBlockedTicks)
	}
}
