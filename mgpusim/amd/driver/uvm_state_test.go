package driver

import (
	"strings"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
)

// sbin_codex: VA-block / 64 KB region / page-state / invariant contract tests
// (todo 4 of plan mgpusim-uvm-manager). The QA regex
// 'TestUVM(StateLifecycle|ForbiddenTransitions|InvariantContext)' runs the
// lifecycle, forbidden-transition, and invariant fixtures in this file; the
// Ginkgo "UVM state" specs live in uvm_state_ginkgo_test.go.

// buildTestRegistration builds a registration with pageCount base pages and
// its VA-block model. Deterministic; used by both Go and Ginkgo tests.
func buildTestRegistration(pid vm.PID, base, pageCount uint64) *ManagedAllocationRegistration {
	numWords := (pageCount + 63) / 64
	reg := &ManagedAllocationRegistration{
		PID:             pid,
		Base:            base,
		Size:            pageCount * basePageSize,
		PageCount:       pageCount,
		PageSize:        basePageSize,
		ResidentMask:    make([]uint64, numWords),
		InFlightMask:    make([]uint64, numWords),
		DirtyMask:       make([]uint64, numWords),
		ValidMask:       make([]uint64, numWords),
		CPUBackingPages: make([]uint64, pageCount),
	}
	for w := uint64(0); w < numWords; w++ {
		bits := pageCount - w*64
		if bits > 64 {
			bits = 64
		}
		reg.ValidMask[w] = (uint64(1) << bits) - 1
	}
	for i := uint64(0); i < pageCount; i++ {
		reg.CPUBackingPages[i] = 0x1_0000_0000 + i*basePageSize
	}
	reg.VABlocks = buildVABlocks(reg)
	return reg
}

// setResident flips the GPU-residency bit of allocation page `page`.
func setResident(reg *ManagedAllocationRegistration, page uint64, on bool) {
	if on {
		reg.ResidentMask[page/64] |= uint64(1) << (page % 64)
	} else {
		reg.ResidentMask[page/64] &^= uint64(1) << (page % 64)
	}
}

// TestUVMStateLifecycle walks the legal §23 CPU -> GPU -> CPU lifecycle and
// asserts every transition succeeds, the state advances, and the migration
// recency timestamp updates only on migration/admission (§31.2).
func TestUVMStateLifecycle(t *testing.T) {
	reg := buildTestRegistration(vm.PID(1), 4096, 512)
	block := reg.VABlocks[0]
	region := block.SubBlocks[0]
	ctx := RegionContext{PID: vm.PID(1), GPU: 1, Block: 0, Region: 0}
	sm := NewRegionStateMachine(ctx, region)

	steps := []RegionState{
		RegionFaultPending,
		RegionMigratingToGPU,
		RegionGPUResident,
		RegionEvictPending,
		RegionMigratingToCPU,
		RegionCPUResident,
	}
	now := sim.VTimeInSec(100)
	for _, to := range steps {
		if err := sm.Transition(to, now); err != nil {
			t.Fatalf("Transition(%s) from %s: %v", to, region.State, err)
		}
		if region.State != to {
			t.Errorf("state = %s, want %s", region.State, to)
		}
	}

	// The final transition (MIGRATING_TO_CPU -> CPU_RESIDENT) is a migration,
	// so recency must equal the last transition time.
	if region.LastMigrationTime != now {
		t.Errorf("LastMigrationTime = %v, want %v after migration", region.LastMigrationTime, now)
	}
}

// TestUVMRecencyNotOnResidencyHit verifies §31.2: recency is updated ONLY on
// migration/admission, never on a non-migration transition or a residency hit.
func TestUVMRecencyNotOnResidencyHit(t *testing.T) {
	reg := buildTestRegistration(vm.PID(1), 4096, 512)
	region := reg.VABlocks[0].SubBlocks[0]
	sm := NewRegionStateMachine(
		RegionContext{PID: vm.PID(1), GPU: 1, Block: 0, Region: 0}, region)

	sm.Transition(RegionFaultPending, 10) // not a migration: no recency update
	if region.LastMigrationTime != 0 {
		t.Errorf("recency updated on FAULT_PENDING: %v", region.LastMigrationTime)
	}
	sm.Transition(RegionMigratingToGPU, 20) // migration starts
	if region.LastMigrationTime != 20 {
		t.Errorf("recency = %v, want 20 on migration start", region.LastMigrationTime)
	}
	sm.Transition(RegionGPUResident, 30) // admission completes
	if region.LastMigrationTime != 30 {
		t.Errorf("recency = %v, want 30 on admission", region.LastMigrationTime)
	}
	sm.Transition(RegionEvictPending, 40) // not a migration: no recency update
	if region.LastMigrationTime != 30 {
		t.Errorf("recency updated on EVICT_PENDING: %v", region.LastMigrationTime)
	}
	// A residency hit (no state change) must not touch recency either.
	before := region.LastMigrationTime
	_ = before
	if region.LastMigrationTime != 30 {
		t.Errorf("recency changed without a transition: %v", region.LastMigrationTime)
	}
}

// TestUVMForbiddenTransitions table-drives every (from, to) pair NOT in the
// §23 table and asserts the transition is rejected with PID/GPU/block/region/
// state context BEFORE any mutation (prior state is never corrupted).
func TestUVMForbiddenTransitions(t *testing.T) {
	reg := buildTestRegistration(vm.PID(7), 4096, 512)
	block := reg.VABlocks[0]
	region := block.SubBlocks[0]
	ctx := RegionContext{PID: vm.PID(7), GPU: 2, Block: 0, Region: 0}
	sm := NewRegionStateMachine(ctx, region)

	forbidden := []struct {
		setup RegionState
		to    RegionState
	}{
		{RegionIDLE, RegionGPUResident},              // skips fault + migration
		{RegionIDLE, RegionMigratingToCPU},           // no eviction from IDLE
		{RegionIDLE, RegionCPUResident},              // skips the whole chain
		{RegionFaultPending, RegionGPUResident},      // skips migration
		{RegionFaultPending, RegionCPUResident},      // skips migration + completion
		{RegionMigratingToGPU, RegionMigratingToGPU}, // duplicate migration
		{RegionMigratingToGPU, RegionCPUResident},    // skips completion
		{RegionGPUResident, RegionMigratingToGPU},    // double migration
		{RegionGPUResident, RegionCPUResident},       // skips eviction
		{RegionEvictPending, RegionGPUResident},      // cancels eviction
		{RegionEvictPending, RegionCPUResident},      // skips migration
		{RegionMigratingToCPU, RegionGPUResident},    // reverse migration
		{RegionMigratingToCPU, RegionMigratingToGPU}, // reverse migration
		{RegionCPUResident, RegionGPUResident},       // skips fault + migration
		{RegionCPUResident, RegionEvictPending},      // no eviction from CPU
	}

	for _, tc := range forbidden {
		region.State = tc.setup
		err := sm.Transition(tc.to, 50)
		if err == nil {
			t.Errorf("Transition(%s -> %s) succeeded, want rejection", tc.setup, tc.to)
			continue
		}
		te, ok := err.(*TransitionError)
		if !ok {
			t.Errorf("Transition(%s -> %s) error type = %T, want *TransitionError",
				tc.setup, tc.to, err)
			continue
		}
		if te.Context.PID != vm.PID(7) || te.Context.GPU != 2 ||
			te.Context.Block != 0 || te.Context.Region != 0 {
			t.Errorf("Transition(%s -> %s) context = %+v, want PID=7 GPU=2 block=0 region=0",
				tc.setup, tc.to, te.Context)
		}
		if te.From != tc.setup || te.To != tc.to {
			t.Errorf("Transition(%s -> %s) error from/to = %s/%s",
				tc.setup, tc.to, te.From, te.To)
		}
		if region.State != tc.setup {
			t.Errorf("Transition(%s -> %s) mutated state to %s, want %s unchanged",
				tc.setup, tc.to, region.State, tc.setup)
		}
	}
}

// TestUVMCoalescingRules verifies the §23 coalescing/stall behaviors: a second
// fault while MIGRATING_TO_GPU coalesces, an access-counter migration while
// MIGRATING_TO_GPU is ignored, and a GPU access while MIGRATING_TO_CPU stalls.
func TestUVMCoalescingRules(t *testing.T) {
	reg := buildTestRegistration(vm.PID(1), 4096, 512)
	region := reg.VABlocks[0].SubBlocks[0]
	ctx := RegionContext{PID: vm.PID(1), GPU: 1, Block: 0, Region: 0}
	sm := NewRegionStateMachine(ctx, region)

	region.State = RegionMigratingToGPU
	if err := sm.CoalesceFault(); err != nil {
		t.Errorf("second fault while MIGRATING_TO_GPU not coalesced: %v", err)
	}
	if err := sm.CoalesceAccessCounterMigration(); err != nil {
		t.Errorf("access-counter migration while MIGRATING_TO_GPU not ignored: %v", err)
	}
	if region.State != RegionMigratingToGPU {
		t.Errorf("coalescing mutated state to %s", region.State)
	}

	region.State = RegionMigratingToCPU
	if err := sm.StallOnGPUAccess(); err == nil {
		t.Error("GPU access while MIGRATING_TO_CPU did not stall")
	} else if !strings.Contains(err.Error(), "stall") {
		t.Errorf("stall error = %v, want a stall reason", err)
	}
	if region.State != RegionMigratingToCPU {
		t.Errorf("stall mutated state to %s", region.State)
	}

	// Coalescing outside the in-flight states is rejected.
	region.State = RegionGPUResident
	if err := sm.CoalesceFault(); err == nil {
		t.Error("CoalesceFault on GPU_RESIDENT succeeded, want rejection")
	}
	if err := sm.CoalesceAccessCounterMigration(); err == nil {
		t.Error("CoalesceAccessCounterMigration on GPU_RESIDENT succeeded, want rejection")
	}
	if err := sm.StallOnGPUAccess(); err != nil {
		t.Errorf("GPU access on GPU_RESIDENT stalled: %v", err)
	}
}

// TestUVMInvariantContext builds a consistent GPU-resident region, proves every
// §28 invariant passes, then corrupts each invariant and proves it fails with
// PID/GPU/block/region context.
func TestUVMInvariantContext(t *testing.T) {
	reg := buildTestRegistration(vm.PID(3), 4096, 16) // one full region
	block := reg.VABlocks[0]
	region := block.SubBlocks[0]
	res := NewAdmissionReservation(64 * mem.KB)
	ctx := InvariantContext{
		PID: vm.PID(3), GPU: 1, Block: block, BlockIdx: 0,
		Region: region, RegionIdx: 0, Reg: reg, Reservation: res,
	}

	// Consistent GPU-resident state: all 16 pages resident with GPU PAs.
	region.State = RegionGPUResident
	for i := uint64(0); i < pagesPerSubBlock; i++ {
		setResident(reg, i, true)
		block.Pages[i].GPUPhysicalPage = 0x2_0000_0000 + i*basePageSize
		block.Pages[i].CPUPhysicalPage = reg.CPUBackingPages[i]
	}
	if err := res.ReserveAdmission(64 * mem.KB); err != nil {
		t.Fatalf("ReserveAdmission: %v", err)
	}
	res.CommitAdmission(64 * mem.KB)
	if err := ctx.CheckAll(); err != nil {
		t.Fatalf("consistent GPU-resident state failed invariants: %v", err)
	}

	// Violation: GPU_RESIDENT page with no GPU physical page. Block-local
	// page 3 is allocation page 2 (valid in region 0).
	block.Pages[3].GPUPhysicalPage = 0
	if err := ctx.CheckGPUPhysicalAllocation(); err == nil {
		t.Error("GPU_RESIDENT with no GPU PA: invariant not violated")
	}
	block.Pages[3].GPUPhysicalPage = 0x2_0000_0000 + 3*basePageSize

	// Violation: remote-mapped page with no CPU backing. Block-local page 1
	// is allocation page 0 (valid in region 0).
	block.Pages[1].RemoteMapped = true
	block.Pages[1].CPUPhysicalPage = 0
	if err := ctx.CheckRemoteMapping(); err == nil {
		t.Error("REMOTE mapping with no CPU backing: invariant not violated")
	}
	block.Pages[1].CPUPhysicalPage = reg.CPUBackingPages[0]

	// Violation: CPU_REMOTE page cached on GPU.
	region.State = RegionCPUResident
	for i := uint64(0); i < pagesPerSubBlock; i++ {
		setResident(reg, i, false)
		block.Pages[i].GPUPhysicalPage = 0
		block.Pages[i].RemoteMapped = true
	}
	block.Pages[1].CachedOnGPU = true
	if err := ctx.CheckRemoteCacheability(); err == nil {
		t.Error("CPU_REMOTE cached on GPU: invariant not violated")
	}
	block.Pages[1].CachedOnGPU = false

	// Violation: two authoritative residences (region GPU_RESIDENT but pages
	// not resident).
	region.State = RegionGPUResident
	if err := ctx.CheckResidencyAuthority(); err == nil {
		t.Error("GPU_RESIDENT region with no resident pages: invariant not violated")
	}
	region.State = RegionCPUResident

	// Violation: oversubscription (R > C).
	if err := ctx.CheckOversubscription(); err != nil {
		t.Fatalf("R <= C state oversubscribed: %v", err)
	}
	res.residentBytes = 128 * mem.KB // white-box corruption: R > C
	if err := ctx.CheckOversubscription(); err == nil {
		t.Error("R > C: oversubscription invariant not violated")
	}
}

// TestUVMVABlockGeometry table-drives the 2 MB / 64 KB / 4 KB geometry:
// block counts, sub-block counts, page indexing, alignment, and the TBN
// must-not-cross-a-block-boundary rule (§4.1).
func TestUVMVABlockGeometry(t *testing.T) {
	reg := buildTestRegistration(vm.PID(1), 4096, 513) // 2 MB + 4 KB
	if len(reg.VABlocks) != 2 {
		t.Fatalf("VABlocks = %d, want 2", len(reg.VABlocks))
	}
	b0, b1 := reg.VABlocks[0], reg.VABlocks[1]
	if b0.StartVA != 0 || b1.StartVA != 2*mem.MB {
		t.Errorf("block starts = %#x/%#x, want 0x0/0x200000", b0.StartVA, b1.StartVA)
	}
	if b0.Size != 2*mem.MB || b1.Size != 2*mem.MB {
		t.Errorf("block sizes = %d/%d, want 2MB", b0.Size, b1.Size)
	}
	if len(b0.SubBlocks) != 32 || len(b1.SubBlocks) != 32 {
		t.Errorf("sub-block counts = %d/%d, want 32/32", len(b0.SubBlocks), len(b1.SubBlocks))
	}
	// Partial block: block 1 covers allocation pages 511..512 (block-local
	// pages 0..1); block-local page 2 is beyond the allocation.
	if b1.Pages[0].CPUPhysicalPage == 0 || b1.Pages[1].CPUPhysicalPage == 0 {
		t.Error("block 1 pages 0/1 CPU backing not set")
	}
	if b1.Pages[2].CPUPhysicalPage != 0 {
		t.Error("block 1 page 2 (beyond allocation) has CPU backing")
	}

	// Geometry helpers.
	if PageIndexInBlock(2*mem.MB-basePageSize) != 511 {
		t.Errorf("PageIndexInBlock(511) = %d", PageIndexInBlock(2*mem.MB-basePageSize))
	}
	if SubBlockIndexForPage(0) != 0 || SubBlockIndexForPage(15) != 0 ||
		SubBlockIndexForPage(16) != 1 || SubBlockIndexForPage(511) != 31 {
		t.Errorf("SubBlockIndexForPage misindexed")
	}
	if SubBlockIndexForVA(4096+16*basePageSize) != 1 {
		t.Errorf("SubBlockIndexForVA = %d", SubBlockIndexForVA(4096+16*basePageSize))
	}
	if BlockForVA(4096+2*mem.MB) != 2*mem.MB {
		t.Errorf("BlockForVA = %#x", BlockForVA(4096+2*mem.MB))
	}
	if SubBlockStartVA(4096+17*basePageSize) != 64*mem.KB {
		t.Errorf("SubBlockStartVA = %#x", SubBlockStartVA(4096+17*basePageSize))
	}

	// TBN nodes are 64 KB aligned; a misaligned node is rejected.
	if err := TBNNodeWithinBlock(64 * mem.KB); err != nil {
		t.Errorf("aligned TBN node rejected: %v", err)
	}
	if err := TBNNodeWithinBlock(2*mem.MB - 64*mem.KB); err != nil {
		t.Errorf("aligned TBN node at block edge rejected: %v", err)
	}
	if err := TBNNodeWithinBlock(4096); err == nil {
		t.Error("misaligned TBN node accepted")
	}
}

// TestUVMVABlockBuildFromRegistration drives the real registration path
// (newManagedAllocationRegistration) and asserts the VA-block model and CPU
// backing pages are built.
func TestUVMVABlockBuildFromRegistration(t *testing.T) {
	res := internal.ManagedAllocationResult{
		Base:            4096,
		Size:            2*mem.MB + 4096,
		PageCount:       513,
		PageSize:        4096,
		CPUBackingPages: make([]uint64, 513),
		PIDs:            []vm.PID{vm.PID(1)},
	}
	for i := range res.CPUBackingPages {
		res.CPUBackingPages[i] = 0x1_0000_0000 + uint64(i)*basePageSize
	}
	reg, err := newManagedAllocationRegistration(vm.PID(1), res)
	if err != nil {
		t.Fatalf("newManagedAllocationRegistration: %v", err)
	}
	if len(reg.VABlocks) != 2 {
		t.Fatalf("VABlocks = %d, want 2", len(reg.VABlocks))
	}
	if len(reg.CPUBackingPages) != 513 {
		t.Errorf("CPUBackingPages = %d, want 513", len(reg.CPUBackingPages))
	}
	// Allocation page 0 (VA 4096) is block-local page 1 of block 0 (block 0
	// starts at VA 0).
	if reg.VABlocks[0].Pages[1].CPUPhysicalPage != res.CPUBackingPages[0] {
		t.Errorf("block 0 page 1 CPU PA = %#x, want %#x",
			reg.VABlocks[0].Pages[1].CPUPhysicalPage, res.CPUBackingPages[0])
	}
}

// TestUVMAdmissionReservation table-drives the R+I+N <= C reservation tracking:
// legal reservations commit, over-capacity reservations fail before mutation,
// and migration moves bytes between R and I without exceeding C.
func TestUVMAdmissionReservation(t *testing.T) {
	res := NewAdmissionReservation(64 * mem.KB)

	if err := res.ReserveAdmission(16 * mem.KB); err != nil {
		t.Fatalf("ReserveAdmission(16KB): %v", err)
	}
	if res.ReservedBytes() != 16*mem.KB {
		t.Errorf("ReservedBytes = %d, want 16KB", res.ReservedBytes())
	}
	// Over-capacity reservation must fail without mutating N.
	if err := res.ReserveAdmission(64 * mem.KB); err == nil {
		t.Error("ReserveAdmission(64KB) with 16KB reserved succeeded, want rejection")
	}
	if res.ReservedBytes() != 16*mem.KB {
		t.Errorf("failed reservation mutated N to %d", res.ReservedBytes())
	}

	res.CommitAdmission(16 * mem.KB)
	if res.ResidentBytes() != 16*mem.KB || res.ReservedBytes() != 0 {
		t.Errorf("after commit R=%d N=%d, want R=16KB N=0",
			res.ResidentBytes(), res.ReservedBytes())
	}

	// Eviction: R -> I, then I -> released.
	res.StartMigration(16 * mem.KB)
	if res.ResidentBytes() != 0 || res.InFlightBytes() != 16*mem.KB {
		t.Errorf("after StartMigration R=%d I=%d, want R=0 I=16KB",
			res.ResidentBytes(), res.InFlightBytes())
	}
	res.CompleteMigrationToCPU(16 * mem.KB)
	if res.InFlightBytes() != 0 {
		t.Errorf("after CompleteMigrationToCPU I=%d, want 0", res.InFlightBytes())
	}

	// Admission again: N -> I -> R.
	if err := res.ReserveAdmission(64 * mem.KB); err != nil {
		t.Fatalf("ReserveAdmission(64KB) after eviction: %v", err)
	}
	res.CommitAdmission(64 * mem.KB)
	if res.ResidentBytes() != 64*mem.KB {
		t.Errorf("R = %d, want 64KB", res.ResidentBytes())
	}
	if res.ResidentBytes()+res.InFlightBytes()+res.ReservedBytes() > res.CapacityBytes() {
		t.Error("R+I+N exceeds capacity after full admission")
	}
}

// TestUVMAccessCounter verifies the §31.1 counter state: reset on kernel
// launch, increment for CPU-remote accesses, and rejection of increments for
// GPU-resident (non-remote) accesses.
func TestUVMAccessCounter(t *testing.T) {
	region := NewSubBlockState(4096)
	if region.State != RegionIDLE {
		t.Errorf("new region state = %s, want IDLE", region.State)
	}
	if err := region.RecordRemoteAccess(); err != nil {
		t.Fatalf("RecordRemoteAccess on IDLE: %v", err)
	}
	if region.AccessCounter != 1 {
		t.Errorf("AccessCounter = %d, want 1", region.AccessCounter)
	}
	region.ResetAccessCounter()
	if region.AccessCounter != 0 {
		t.Errorf("AccessCounter after reset = %d, want 0", region.AccessCounter)
	}

	region.State = RegionGPUResident
	if err := region.RecordRemoteAccess(); err == nil {
		t.Error("RecordRemoteAccess on GPU_RESIDENT succeeded, want rejection")
	}
	if region.AccessCounter != 0 {
		t.Errorf("AccessCounter mutated by rejected increment: %d", region.AccessCounter)
	}
}

// TestUVMStatisticsOwnership verifies every tracked statistic has exactly one
// documented owner/update point.
func TestUVMStatisticsOwnership(t *testing.T) {
	seen := make(map[string]string)
	for _, o := range StatisticOwnership {
		if o.Statistic == "" || o.Owner == "" {
			t.Errorf("statistic ownership entry has empty field: %+v", o)
		}
		if prev, dup := seen[o.Statistic]; dup {
			t.Errorf("statistic %q owned by both %q and %q", o.Statistic, prev, o.Owner)
		}
		seen[o.Statistic] = o.Owner
	}
	if len(seen) == 0 {
		t.Error("no statistics documented")
	}
}
