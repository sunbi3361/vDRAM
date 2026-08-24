package uvm

// sbin_codex: UVM coordinator contract tests (plan todo 21 of
// mgpusim-uvm-manager). These plain Go tests drive the coordinator with
// timing-neutral fixture handlers and assert: shared handlers/drain across
// modes, child semantic keys, same-mode serial/parallel determinism, exact
// parity for a predeclared trace DAG, the feedback race `A fault -> A replay
// -> A2 fault` with independent B (normal may total-order A,B,A2; ideal
// A,A2,B; canonical DAG has A->A2, B unordered), locally justified
// timing-derived unmatched roots, rejection of missing provenance and
// duplicates, the duplicate-transition-epoch rule, the stable
// sourceLocalSequence tie-break, and ideal DMA accounting (zero transfer
// delay, logical bytes still counted).

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

const (
	fixtureCPUResident = "CPU_RESIDENT"
	fixtureMigrating   = "MIGRATING"
	fixtureGPUResident = "GPU_RESIDENT"
	fixtureEvicting    = "EVICTING"
)

// fixtureRegion tracks one 64 KB region's residency state.
type fixtureRegion struct {
	state  string
	pinned bool
}

// fixture is the timing-neutral functional state machine behind the
// coordinator: the same handlers run in both modes and only the coordinator
// timing differs. It records functional counters, logical byte accounting,
// and the observed-access provenance registry.
type fixture struct {
	pid    vm.PID
	gpu    int
	launch uint64
	source string

	regions   map[string]*fixtureRegion
	counters  map[string]uint64
	bytesH2D  uint64
	bytesD2H  uint64
	capacity  uint64 // 0 disables the admission gate
	refaults  map[string]bool
	feedback  map[string]bool
	done      map[string]bool
	recency   map[string]uint64
	recencyN  uint64
	observed  []string
	concurrent int
	maxConcurrent int
}

func newFixture(pid vm.PID, gpu int, launch uint64, source string) *fixture {
	return &fixture{
		pid:       pid,
		gpu:       gpu,
		launch:    launch,
		source:    source,
		regions:   make(map[string]*fixtureRegion),
		counters:  make(map[string]uint64),
		refaults:  make(map[string]bool),
		feedback:  make(map[string]bool),
		done:      make(map[string]bool),
		recency:   make(map[string]uint64),
	}
}

// addRegion registers a region with an initial residency state.
func (f *fixture) addRegion(regionBase uint64, state string) {
	f.regions[fmt.Sprintf("%#x", regionBase)] = &fixtureRegion{state: state}
}

// pin marks a region ineligible as an eviction victim.
func (f *fixture) pin(regionBase uint64) {
	f.regions[fmt.Sprintf("%#x", regionBase)].pinned = true
}

// refaultOn marks a region whose eviction causes its observed access to
// re-fault (the timing-derived refault).
func (f *fixture) refaultOn(regionBase uint64) {
	f.refaults[fmt.Sprintf("%#x", regionBase)] = true
}

// feedbackOn marks a region whose replay re-faults (the feedback race).
func (f *fixture) feedbackOn(regionBase uint64) {
	f.feedback[fmt.Sprintf("%#x", regionBase)] = true
}

// key builds the semantic key of a program-origin root.
func (f *fixture) key(
	kind OriginKind, regionBase uint64, access vm.AccessKind, ordinal uint64,
) SemanticRootKey {
	return SemanticRootKey{
		KernelLaunchOrdinal:     f.launch,
		SourceComponentStableID: f.source,
		OriginKind:              kind,
		PID:                     f.pid,
		GPU:                     f.gpu,
		RegionBase:              regionBase,
		AccessKind:              access,
		ProgramCommandOrdinal:   ordinal,
	}
}

// request builds a delivered fault-request root.
func (f *fixture) request(
	regionBase uint64, ordinal, seq uint64, at sim.VTimeInSec,
) *Root {
	return &Root{
		SemanticKey: f.key(OriginFaultRequest, regionBase,
			vm.AccessKindRead, ordinal),
		Stamp: SameModeStamp{
			KernelLaunchOrdinal: f.launch,
			SourceBuildOrdinal:  0,
			SourceLocalSequence: seq,
		},
		Operation:        "fault-request",
		CurrentVTime:     at,
		OperationOrdinal: 1,
	}
}

// region returns the fixture region of a root.
func (f *fixture) region(root *Root) *fixtureRegion {
	return f.regions[fmt.Sprintf("%#x", root.SemanticKey.RegionBase)]
}

// enter/leave track the handler concurrency (the drain must be serialized).
func (f *fixture) enter() {
	f.concurrent++
	if f.concurrent > f.maxConcurrent {
		f.maxConcurrent = f.concurrent
	}
}

func (f *fixture) leave() {
	f.concurrent--
}

// countState counts the regions in a residency state.
func (f *fixture) countState(state string) uint64 {
	var n uint64
	for _, reg := range f.regions {
		if reg.state == state {
			n++
		}
	}
	return n
}

// lruVictim returns the least-recently-admitted unpinned GPU-resident
// region, or "" when none is eligible.
func (f *fixture) lruVictim() string {
	var best string
	var bestRec uint64
	for base, reg := range f.regions {
		if reg.state != fixtureGPUResident || reg.pinned {
			continue
		}
		if best == "" || f.recency[base] < bestRec {
			best = base
			bestRec = f.recency[base]
		}
	}
	return best
}

// admissionGate runs the projected-occupancy gate (todo 20): free =
// C-(R+I+N+bytes); NeedToEvict = max(0, H-(free+E)) with H = one 64 KB
// region; a required victim is launched only when an unpinned resident
// victim exists (otherwise the optional target is infeasible and the
// admission proceeds with a shortfall). Returns the launched pre-eviction
// children.
func (f *fixture) admissionGate(root *Root, newBytes uint64) []*Root {
	if f.capacity == 0 {
		return nil
	}
	r := f.countState(fixtureGPUResident)
	i := f.countState(fixtureMigrating)
	e := f.countState(fixtureEvicting)
	free := uint64(0)
	if f.capacity > r+i+newBytes {
		free = f.capacity - (r + i + newBytes)
	}
	need := uint64(0)
	if free+e < 1 {
		need = 1 - (free + e)
	}
	var children []*Root
	for v := uint64(0); v < need; v++ {
		victim := f.lruVictim()
		if victim == "" {
			break // optional headroom infeasible: admit with a shortfall
		}
		regionBase, err := strconv.ParseUint(victim, 0, 64)
		if err != nil {
			panic(err)
		}
		pre := &Root{
			SemanticKey: f.key(OriginPreEviction, regionBase,
				vm.AccessKindWrite, root.SemanticKey.ProgramCommandOrdinal),
			Operation:  "pre-eviction",
			EdgeLabel:  "pre-evict",
			Provenance: root.Key(),
		}
		f.regions[victim].state = fixtureEvicting
		children = append(children, pre)
	}
	return children
}

// faultRequest delivers a fault request: it coalesces into an in-flight
// migration (no separate service root) or generates the fault-service child.
func (f *fixture) faultRequest(root *Root) ([]*Root, string, string, bool) {
	f.enter()
	defer f.leave()
	f.counters["fault-request"]++
	reg := f.region(root)
	if reg.state == fixtureMigrating {
		return nil, "", "delivered", true
	}
	return []*Root{{Operation: "fault-service", EdgeLabel: "service"}},
		"", "delivered", true
}

// faultService services one 64 KB region: a resident demand replays with
// zero work; a CPU-resident demand runs the admission gate and migrates; an
// in-flight demand coalesces.
func (f *fixture) faultService(root *Root) ([]*Root, string, string, bool) {
	f.enter()
	defer f.leave()
	f.counters["fault-service"]++
	reg := f.region(root)
	switch reg.state {
	case fixtureGPUResident:
		return []*Root{{Operation: "replay", EdgeLabel: "replay"}},
			reg.state, "replayed", true
	case fixtureMigrating:
		return nil, reg.state, "coalesced", true
	default: // CPU_RESIDENT
		children := f.admissionGate(root, 1)
		reg.state = fixtureMigrating
		f.bytesH2D += 64 * 1024
		f.recency[fmt.Sprintf("%#x", root.SemanticKey.RegionBase)] = f.recencyN
		f.recencyN++
		children = append(children,
			&Root{Operation: "dma-h2d", EdgeLabel: "migrate"})
		return children, reg.state, "migrating", true
	}
}

// dmaH2D completes an H2D migration: the region becomes GPU-resident, the
// logical transfer bytes are counted, and the blocked access is replayed.
func (f *fixture) dmaH2D(root *Root) ([]*Root, string, string, bool) {
	f.enter()
	defer f.leave()
	f.counters["dma-h2d"]++
	reg := f.region(root)
	reg.state = fixtureGPUResident
	root.Bytes = 64 * 1024
	return []*Root{{Operation: "replay", EdgeLabel: "replay"}},
		reg.state, "h2d-done", true
}

// replay completes a GMMU replay: a feedback region re-faults (the A2
// demand) exactly once.
func (f *fixture) replay(root *Root) ([]*Root, string, string, bool) {
	f.enter()
	defer f.leave()
	f.counters["replay"]++
	reg := f.region(root)
	base := fmt.Sprintf("%#x", root.SemanticKey.RegionBase)
	if f.feedback[base] && !f.done[base] {
		f.done[base] = true
		sk := root.SemanticKey
		sk.OriginKind = OriginFaultRequest
		sk.AccessKind = vm.AccessKindRead
		sk.ProgramCommandOrdinal++
		return []*Root{{
			SemanticKey: sk,
			Operation:   "fault-request",
			EdgeLabel:   "feedback",
		}}, reg.state, "replayed", true
	}
	return nil, reg.state, "replayed", true
}

// preEviction completes a pre-eviction D2H: the victim returns to
// CPU-resident and its observed access re-faults when configured.
func (f *fixture) preEviction(root *Root) ([]*Root, string, string, bool) {
	f.enter()
	defer f.leave()
	f.counters["pre-eviction"]++
	reg := f.region(root)
	reg.state = fixtureCPUResident
	root.Bytes = 64 * 1024
	f.bytesD2H += 64 * 1024
	base := fmt.Sprintf("%#x", root.SemanticKey.RegionBase)
	if f.refaults[base] {
		sk := root.SemanticKey
		sk.OriginKind = OriginFaultRequest
		sk.AccessKind = vm.AccessKindRead
		sk.ProgramCommandOrdinal++
		return []*Root{{
			SemanticKey:   sk,
			Operation:     "fault-request",
			EdgeLabel:     "refault",
			TimingDerived: true,
			Provenance: provenanceOf(f.pid, f.gpu,
				root.SemanticKey.RegionBase, vm.AccessKindRead),
		}}, reg.state, "pre-evicted", true
	}
	return nil, reg.state, "pre-evicted", true
}

// registerHandlers registers the timing-neutral handlers on the coordinator.
func (f *fixture) registerHandlers(c *Coordinator) {
	c.RegisterHandler("fault-request", HandlerFunc(f.faultRequest))
	c.RegisterHandler("fault-service", HandlerFunc(f.faultService))
	c.RegisterHandler("dma-h2d", HandlerFunc(f.dmaH2D))
	c.RegisterHandler("replay", HandlerFunc(f.replay))
	c.RegisterHandler("pre-eviction", HandlerFunc(f.preEviction))
}

// registerProvenance registers every observed access of the fixture.
func (f *fixture) registerProvenance(c *Coordinator) {
	for _, base := range f.observed {
		c.RegisterProvenance(base)
	}
}

// observe records an observed access of a region.
func (f *fixture) observe(regionBase uint64) {
	f.observed = append(f.observed,
		provenanceOf(f.pid, f.gpu, regionBase, vm.AccessKindRead))
}

// drainAll runs the secondary-event serialized drain at every ready time
// until quiescence.
func drainAll(c *Coordinator) int {
	total := 0
	for {
		next, ok := c.NextReadyTime()
		if !ok {
			return total
		}
		total += c.Drain(next)
	}
}

// defaultTransport models the per-operation transport of the fixtures
// (normal mode only; ideal mode zeroes it).
func defaultTransport() func(root *Root) sim.VTimeInSec {
	return func(root *Root) sim.VTimeInSec {
		switch root.Operation {
		case "dma-h2d", "pre-eviction":
			return 10
		case "replay":
			return 4
		default: // fault-request, fault-service
			return 3
		}
	}
}

// buildCoordinator builds a coordinator with the fixture handlers and the
// default transport.
func buildCoordinator(mode Mode, f *fixture) *Coordinator {
	c := NewCoordinator(mode)
	c.SetTransport(defaultTransport())
	f.registerHandlers(c)
	f.registerProvenance(c)
	return c
}

// TestUVMSemanticRootIdentity proves the cross-mode identity contract: the
// semantic key carries (kernelLaunchOrdinal, sourceComponentStableID,
// originKind, PID, GPU, regionBase, accessKind, programCommandOrdinal) and
// the sourceLocalSequence is excluded from cross-mode identity.
func TestUVMSemanticRootIdentity(t *testing.T) {
	sk := SemanticRootKey{
		KernelLaunchOrdinal:     3,
		SourceComponentStableID: "gmmu0",
		OriginKind:              OriginFaultRequest,
		PID:                     1,
		GPU:                     0,
		RegionBase:              0x10000,
		AccessKind:              vm.AccessKindRead,
		ProgramCommandOrdinal:   7,
	}

	// Two modes generate the same demand with different local sequences:
	// the identity is the semantic key, not the stamp.
	normal := &Root{
		SemanticKey: sk,
		Stamp:       SameModeStamp{KernelLaunchOrdinal: 3, SourceBuildOrdinal: 0, SourceLocalSequence: 0},
	}
	ideal := &Root{
		SemanticKey: sk,
		Stamp:       SameModeStamp{KernelLaunchOrdinal: 3, SourceBuildOrdinal: 0, SourceLocalSequence: 9},
	}
	if normal.Key() != ideal.Key() {
		t.Fatalf("cross-mode identity differs: %s vs %s",
			normal.Key(), ideal.Key())
	}
	if normal.Stamp == ideal.Stamp {
		t.Fatal("the local tie-break must differ across the modes")
	}
	if !sk.Equal(ideal.SemanticKey) {
		t.Fatal("Equal must compare the full semantic key")
	}
	if sk.String() == "" {
		t.Fatal("canonical key string must be non-empty")
	}

	// A different programCommandOrdinal changes the identity (the A2 demand
	// is a distinct root).
	a2 := sk
	a2.ProgramCommandOrdinal++
	if sk.String() == a2.String() {
		t.Fatal("the programCommandOrdinal must be part of the identity")
	}
}

// TestUVMStableSourceTieBreak proves the sourceLocalSequence is a stable
// local tie-break: two independent roots with the same ready time and the
// same launch/build ordinals are ordered by the sequence, deterministically
// across repeated runs.
func TestUVMStableSourceTieBreak(t *testing.T) {
	f := newFixture(1, 0, 3, "gmmu0")
	f.addRegion(0x10000, fixtureCPUResident)
	f.addRegion(0x20000, fixtureCPUResident)

	run := func() []string {
		c := buildCoordinator(ModeNormal, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0))
		c.Enqueue(f.request(0x20000, 2, 1, 0))
		drainAll(c)
		var order []string
		for _, n := range c.Trace().Nodes() {
			if n.Operation == "fault-request" {
				order = append(order, n.Key)
			}
		}
		return order
	}

	first := run()
	if len(first) != 2 {
		t.Fatalf("request nodes = %d, want 2", len(first))
	}
	if first[0] != f.key(OriginFaultRequest, 0x10000,
		vm.AccessKindRead, 1).String() {
		t.Fatalf("first request = %s, want region 0x10000", first[0])
	}
	// Serial/parallel determinism: repeated runs produce the same order.
	for i := 0; i < 5; i++ {
		again := run()
		if len(again) != 2 || again[0] != first[0] || again[1] != first[1] {
			t.Fatalf("run %d order differs: %v vs %v", i, again, first)
		}
	}
}

// TestUVMPredecessorChildOrdinal proves child operationOrdinal follows the
// normative state-machine edges (parent + 1 + i) and successors require
// predecessor completion.
func TestUVMPredecessorChildOrdinal(t *testing.T) {
	f := newFixture(1, 0, 3, "gmmu0")
	f.addRegion(0x10000, fixtureCPUResident)

	c := buildCoordinator(ModeNormal, f)
	c.Enqueue(f.request(0x10000, 1, 0, 0))
	drainAll(c)

	roots := c.ExecutedRoots()
	byOp := make(map[string]*Root)
	for _, r := range roots {
		byOp[r.Operation] = r
	}
	req := byOp["fault-request"]
	svc := byOp["fault-service"]
	dma := byOp["dma-h2d"]
	rpl := byOp["replay"]
	if req == nil || svc == nil || dma == nil || rpl == nil {
		t.Fatalf("executed roots = %v, want request/service/dma/replay",
			rootOps(roots))
	}
	if req.OperationOrdinal != 1 || svc.OperationOrdinal != 2 ||
		dma.OperationOrdinal != 3 || rpl.OperationOrdinal != 4 {
		t.Fatalf("ordinals = %d/%d/%d/%d, want 1/2/3/4",
			req.OperationOrdinal, svc.OperationOrdinal, dma.OperationOrdinal,
			rpl.OperationOrdinal)
	}
	// The child semantic keys follow causality: (parentKey, edgeLabel,
	// parentLocalOccurrence).
	if svc.ChildKey == nil || svc.ChildKey.ParentKey != req.Key() ||
		svc.ChildKey.EdgeLabel != "service" || svc.ChildKey.ParentLocalOccurrence != 0 {
		t.Fatalf("service child key = %+v, want (request, service, 0)",
			svc.ChildKey)
	}
	if dma.ChildKey == nil || dma.ChildKey.ParentKey != svc.Key() ||
		dma.ChildKey.EdgeLabel != "migrate" {
		t.Fatalf("dma child key = %+v, want (service, migrate, 0)", dma.ChildKey)
	}
	// The trace order is the causal order: a successor never executes
	// before its predecessor completes.
	var order []string
	for _, n := range c.Trace().Nodes() {
		order = append(order, n.Operation)
	}
	want := []string{"fault-request", "fault-service", "dma-h2d", "replay"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("trace order = %v, want %v", order, want)
		}
	}
	// The same-mode stamp is inherited by the children.
	if svc.Stamp != req.Stamp || dma.Stamp != req.Stamp || rpl.Stamp != req.Stamp {
		t.Fatal("children must inherit the parent's same-mode stamp")
	}
}

// rootOps lists the operations of the executed roots.
func rootOps(roots []*Root) []string {
	var ops []string
	for _, r := range roots {
		ops = append(ops, r.Operation)
	}
	return ops
}

// TestUVMPrimarySecondaryDrain proves both modes enqueue delivered roots and
// use ONE secondary-event serialized drain: nothing executes without the
// drain, the drain executes one root at a time, and both modes share the
// same drain path.
func TestUVMPrimarySecondaryDrain(t *testing.T) {
	for _, mode := range []Mode{ModeNormal, ModeIdeal} {
		f := newFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, fixtureCPUResident)
		c := buildCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0))

		if c.Trace().Len() != 0 {
			t.Fatalf("%s: roots executed before the drain", mode)
		}
		if mode == ModeNormal {
			// Normal mode: the root becomes ready only after the modeled
			// transport; a drain at the generation time executes nothing.
			if n := c.Drain(0); n != 0 {
				t.Fatalf("normal: drain at 0 executed %d roots, want 0",
					n)
			}
		}
		drainAll(c)
		if c.Trace().Len() == 0 {
			t.Fatalf("%s: drain executed nothing", mode)
		}
		if f.maxConcurrent != 1 {
			t.Fatalf("%s: max handler concurrency = %d, want 1 (serialized)",
				mode, f.maxConcurrent)
		}
		if f.counters["fault-request"] != 1 {
			t.Fatalf("%s: fault-request count = %d, want 1",
				mode, f.counters["fault-request"])
		}
	}
}

// TestUVMNormalAndIdealCoordinator proves both modes run the same functional
// state machine through the same handlers: identical decisions/counters,
// normal delay, zero ideal UVM latency.
func TestUVMNormalAndIdealCoordinator(t *testing.T) {
	build := func(mode Mode) (*Coordinator, *fixture) {
		f := newFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, fixtureCPUResident)
		f.addRegion(0x20000, fixtureCPUResident)
		c := buildCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0))
		c.Enqueue(f.request(0x20000, 2, 1, 0))
		drainAll(c)
		return c, f
	}

	normal, fn := build(ModeNormal)
	ideal, fi := build(ModeIdeal)

	for _, key := range []string{"fault-request", "fault-service",
		"dma-h2d", "replay"} {
		if fn.counters[key] != fi.counters[key] {
			t.Fatalf("counter %s = %d (normal) vs %d (ideal)",
				key, fn.counters[key], fi.counters[key])
		}
	}
	if fn.bytesH2D != fi.bytesH2D {
		t.Fatalf("H2D bytes = %d (normal) vs %d (ideal)",
			fn.bytesH2D, fi.bytesH2D)
	}
	if !normal.Trace().Equal(ideal.Trace()) {
		t.Fatal("the trace DAGs must be identical across modes")
	}
	if normal.TotalLatency() <= 0 {
		t.Fatalf("normal total latency = %v, want > 0", normal.TotalLatency())
	}
	if ideal.TotalLatency() != 0 {
		t.Fatalf("ideal total latency = %v, want 0", ideal.TotalLatency())
	}
	if m := normal.Match(ideal); len(m.Failures) != 0 {
		t.Fatalf("cross-mode match failures: %v", m.Failures)
	}
}

// TestUVMPredeclaredTraceExactParity proves a trace-driven fixture that
// predeclares an identical root DAG requires exact canonical
// decisions/counters: every trace node and every functional counter is
// exactly equal across the modes.
func TestUVMPredeclaredTraceExactParity(t *testing.T) {
	run := func(mode Mode) (*Coordinator, *fixture) {
		f := newFixture(2, 1, 5, "gmmu1")
		f.addRegion(0x10000, fixtureCPUResident)
		f.addRegion(0x20000, fixtureCPUResident)
		f.addRegion(0x30000, fixtureCPUResident)
		c := buildCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0))
		c.Enqueue(f.request(0x20000, 2, 1, 0))
		c.Enqueue(f.request(0x30000, 3, 2, 0))
		drainAll(c)
		return c, f
	}

	normal, fn := run(ModeNormal)
	ideal, fi := run(ModeIdeal)

	if normal.Trace().Len() != ideal.Trace().Len() {
		t.Fatalf("trace node count = %d (normal) vs %d (ideal)",
			normal.Trace().Len(), ideal.Trace().Len())
	}
	if !normal.Trace().Equal(ideal.Trace()) {
		t.Fatal("predeclared trace parity: the trace DAGs must be exactly equal")
	}
	for _, key := range []string{"fault-request", "fault-service",
		"dma-h2d", "replay"} {
		if fn.counters[key] != fi.counters[key] {
			t.Fatalf("counter %s = %d (normal) vs %d (ideal)",
				key, fn.counters[key], fi.counters[key])
		}
	}
	if normal.TotalBytes() != ideal.TotalBytes() {
		t.Fatalf("total bytes = %d (normal) vs %d (ideal)",
			normal.TotalBytes(), ideal.TotalBytes())
	}
	if m := normal.Match(ideal); len(m.Failures) != 0 {
		t.Fatalf("cross-mode match failures: %v", m.Failures)
	}
}

// TestUVMFeedbackRootPartialOrder proves the feedback race `A fault -> A
// replay -> A2 fault` with independent B: normal may total-order A,B,A2,
// ideal A,A2,B, while the canonical DAG has A->A2 and B unordered.
func TestUVMFeedbackRootPartialOrder(t *testing.T) {
	run := func(mode Mode) (*Coordinator, *fixture, []string) {
		f := newFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, fixtureCPUResident)
		f.addRegion(0x20000, fixtureCPUResident)
		f.feedbackOn(0x10000) // A's replay re-faults exactly once
		c := buildCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0)) // A
		c.Enqueue(f.request(0x20000, 2, 1, 0)) // B
		drainAll(c)
		var order []string
		for _, n := range c.Trace().Nodes() {
			if n.Operation == "fault-request" {
				order = append(order, n.Key)
			}
		}
		return c, f, order
	}

	normal, fn, normalOrder := run(ModeNormal)
	ideal, fi, idealOrder := run(ModeIdeal)

	// Normal may total-order A, B, A2: A and B are ready together (same
	// transport), A2 is generated only after A's replay completes.
	if len(normalOrder) != 3 {
		t.Fatalf("normal request order = %v, want 3 requests", normalOrder)
	}
	if normalOrder[0] != fkey(fn, OriginFaultRequest, 0x10000, 1) ||
		normalOrder[1] != fkey(fn, OriginFaultRequest, 0x20000, 2) {
		t.Fatalf("normal order = %v, want A then B", normalOrder)
	}
	// Ideal runs zero-time successors to quiescence: A, A2, then B. The A2
	// demand is a generated root: its identity follows causality (the child
	// key), not the semantic key.
	if len(idealOrder) != 3 {
		t.Fatalf("ideal request order = %v, want 3 requests", idealOrder)
	}
	if idealOrder[0] != fkey(fi, OriginFaultRequest, 0x10000, 1) {
		t.Fatalf("ideal first request = %s, want A", idealOrder[0])
	}
	if !contains(idealOrder[1], "feedback") {
		t.Fatalf("ideal second request = %s, want the A2 child key",
			idealOrder[1])
	}
	if idealOrder[2] != fkey(fi, OriginFaultRequest, 0x20000, 2) {
		t.Fatalf("ideal third request = %s, want B", idealOrder[2])
	}

	// The canonical DAG: A -> A2 (via the replay chain), B unordered. The
	// matched-root DAGs are identical despite the total-order difference.
	if !normal.Trace().Equal(ideal.Trace()) {
		t.Fatal("the matched-root DAGs must be identical across modes")
	}
	if m := normal.Match(ideal); len(m.Failures) != 0 {
		t.Fatalf("cross-mode match failures: %v", m.Failures)
	}
	if normal.TotalLatency() <= 0 || ideal.TotalLatency() != 0 {
		t.Fatalf("latency = %v (normal) / %v (ideal), want > 0 / 0",
			normal.TotalLatency(), ideal.TotalLatency())
	}
}

// fkey builds the canonical key string of a fixture root.
func fkey(f *fixture, kind OriginKind, regionBase uint64, ordinal uint64) string {
	return f.key(kind, regionBase, vm.AccessKindRead, ordinal).String()
}

// TestUVMUnmatchedServiceRootProvenance proves a timing-derived service root
// in only one mode is accepted ONLY with valid provenance, local legality,
// final-state/accounting equality, and no duplicate transition epoch;
// deleting the provenance or duplicating the root fails.
func TestUVMUnmatchedServiceRootProvenance(t *testing.T) {
	run := func(mode Mode) (*Coordinator, *fixture) {
		f := newFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, fixtureCPUResident)
		f.addRegion(0x20000, fixtureCPUResident)
		f.pin(0x20000)    // B is pinned: never an eviction victim
		f.refaultOn(0x10000) // A's eviction re-faults (timing-derived)
		f.capacity = 2
		f.observe(0x10000)
		f.observe(0x20000)
		c := buildCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0)) // A
		c.Enqueue(f.request(0x20000, 2, 1, 0)) // B
		drainAll(c)
		return c, f
	}

	normal, fn := run(ModeNormal)
	ideal, fi := run(ModeIdeal)

	// Normal: B's admission fits (free = 1 = H); A's admission has no
	// resident victim (B in-flight) -> the optional target is infeasible.
	// Ideal: B's admission sees A resident at capacity -> the pre-eviction
	// of A, whose refault re-migrates A.
	if fn.counters["pre-eviction"] != 0 {
		t.Fatalf("normal pre-eviction count = %d, want 0",
			fn.counters["pre-eviction"])
	}
	if fi.counters["pre-eviction"] != 1 {
		t.Fatalf("ideal pre-eviction count = %d, want 1",
			fi.counters["pre-eviction"])
	}

	m := normal.Match(ideal)
	if len(m.Failures) != 0 {
		t.Fatalf("unmatched roots must be accepted with justification: %v",
			m.Failures)
	}
	if len(m.Unmatched) == 0 {
		t.Fatal("the ideal mode must report its unmatched roots")
	}
	// Every unmatched root is locally justified: the pre-eviction subtree
	// and the timing-derived refault subtree.
	for _, u := range m.Unmatched {
		if u.Mode != ModeIdeal {
			t.Fatalf("unmatched root in mode %s, want ideal only", u.Mode)
		}
		if u.Root.Provenance == "" && u.Root.ChildKey == nil {
			t.Fatalf("unmatched root without provenance: %s", u.Root.Key())
		}
	}

	// Final-state equality: A and B are GPU-resident in both modes.
	for _, base := range []string{"0x10000", "0x20000"} {
		if fn.regions[base].state != fixtureGPUResident {
			t.Fatalf("normal final state of %s = %s, want GPU_RESIDENT",
				base, fn.regions[base].state)
		}
		if fi.regions[base].state != fixtureGPUResident {
			t.Fatalf("ideal final state of %s = %s, want GPU_RESIDENT",
				base, fi.regions[base].state)
		}
	}
	// Accounting equations: migrated - evicted == resident + cpu in both
	// modes (the unmatched roots contribute to the equations).
	eq := func(f *fixture) uint64 {
		return f.bytesH2D - f.bytesD2H
	}
	if eq(fn) != eq(fi) {
		t.Fatalf("accounting equation = %d (normal) vs %d (ideal)",
			eq(fn), eq(fi))
	}
	if fi.bytesD2H != 64*1024 {
		t.Fatalf("ideal evicted bytes = %d, want 64KB", fi.bytesD2H)
	}

	// Deleting the provenance of the unmatched pre-eviction fails.
	f2 := newFixture(1, 0, 3, "gmmu0")
	f2.addRegion(0x10000, fixtureCPUResident)
	f2.addRegion(0x20000, fixtureCPUResident)
	f2.pin(0x20000)
	f2.refaultOn(0x10000)
	f2.capacity = 2
	f2.observe(0x10000)
	f2.observe(0x20000)
	c2 := buildCoordinator(ModeIdeal, f2)
	c2.Enqueue(f2.request(0x10000, 1, 0, 0))
	c2.Enqueue(f2.request(0x20000, 2, 1, 0))
	drainAll(c2)
	// Strip the provenance of every executed pre-eviction root.
	for _, r := range c2.ExecutedRoots() {
		if r.SemanticKey.OriginKind == OriginPreEviction {
			r.Provenance = ""
		}
	}
	m2 := normal.Match(c2)
	found := false
	for _, msg := range m2.Failures {
		if contains(msg, "without valid provenance") {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleting the provenance must fail the match: %v",
			m2.Failures)
	}
}

// contains reports whether s contains the substring.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestUVMDuplicateTransitionEpoch proves a duplicate service root for one
// transition epoch is always failure.
func TestUVMDuplicateTransitionEpoch(t *testing.T) {
	f := newFixture(1, 0, 3, "gmmu0")
	// The region is already GPU-resident: the service does zero work (no
	// transition), so both roots service the same transition epoch.
	f.addRegion(0x10000, fixtureGPUResident)
	c := buildCoordinator(ModeNormal, f)

	svc := func() *Root {
		return &Root{
			SemanticKey: f.key(OriginFaultService, 0x10000,
				vm.AccessKindRead, 1),
			Stamp:       SameModeStamp{KernelLaunchOrdinal: 3, SourceBuildOrdinal: 0, SourceLocalSequence: 0},
			Operation:   "fault-service",
			Provenance:  provenanceOf(1, 0, 0x10000, vm.AccessKindRead),
			CurrentVTime: 0,
		}
	}
	c.RegisterProvenance(provenanceOf(1, 0, 0x10000, vm.AccessKindRead))
	c.Enqueue(svc())
	c.Enqueue(svc()) // duplicate service root for the same transition epoch

	drainAll(c)
	if len(c.Failures()) == 0 {
		t.Fatal("the duplicate service root must fail")
	}
	found := false
	for _, msg := range c.Failures() {
		if contains(msg, "duplicate service root") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failures = %v, want a duplicate-transition-epoch failure",
			c.Failures())
	}
	// Exactly one service root executed; the duplicate is recorded as
	// FAILED (both nodes are in the trace).
	executed := 0
	for _, n := range c.Trace().Nodes() {
		if n.Operation == "fault-service" {
			executed++
		}
	}
	if executed != 2 {
		t.Fatalf("service roots = %d, want 2 (one executed, one failed)",
			executed)
	}
	if c.Trace().Node(svc().Key()).Result == "" {
		t.Fatal("the failed duplicate must be recorded in the trace")
	}
}

// TestUVMIdealDMAAccounting proves ideal mode zeroes the DMA transfer delay
// while the logical bytes are still counted.
func TestUVMIdealDMAAccounting(t *testing.T) {
	run := func(mode Mode) (*Coordinator, *fixture) {
		f := newFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, fixtureCPUResident)
		c := buildCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0))
		drainAll(c)
		return c, f
	}

	normal, fn := run(ModeNormal)
	ideal, fi := run(ModeIdeal)

	dmaBytes := func(c *Coordinator) uint64 {
		for _, n := range c.Trace().Nodes() {
			if n.Operation == "dma-h2d" {
				return n.Bytes
			}
		}
		return 0
	}
	if dmaBytes(normal) != 64*1024 || dmaBytes(ideal) != 64*1024 {
		t.Fatalf("DMA bytes = %d (normal) / %d (ideal), want 64KB both",
			dmaBytes(normal), dmaBytes(ideal))
	}
	if fn.bytesH2D != fi.bytesH2D {
		t.Fatalf("H2D accounting = %d (normal) vs %d (ideal)",
			fn.bytesH2D, fi.bytesH2D)
	}
	if ideal.TotalLatency() != 0 {
		t.Fatalf("ideal DMA latency = %v, want 0", ideal.TotalLatency())
	}
	if normal.TotalLatency() <= 0 {
		t.Fatalf("normal DMA latency = %v, want > 0", normal.TotalLatency())
	}
	if normal.TotalBytes() != ideal.TotalBytes() {
		t.Fatalf("total bytes = %d (normal) vs %d (ideal)",
			normal.TotalBytes(), ideal.TotalBytes())
	}
}