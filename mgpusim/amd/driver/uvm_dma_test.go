package driver

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: maximal-run migration through the existing CP DMA Engine with
// capacity reservations (todo 16 of plan mgpusim-uvm-manager, uvm-manager.md
// §23.1.2). The QA regex
// 'TestUVM(DMARunCoalescing|FragmentedRuns|SuperiorProcessingLimit|
// Subordinate64BCount|ReservationAccounting|SecondRunRollback)' runs the
// fixtures in this file (the SuperiorProcessingLimit and Subordinate64BCount
// fixtures live in ./amd/timing/cp): maximal runs map one-to-one to superior
// MemCopy*Req requests, destination PFNs are allocated before H2D, resident
// and in-flight pages are removed from the migration mask, reservations
// account exactly (R/I/N), commit publishes residency/PTEs only after ALL
// runs succeed, and an injected failure rolls back unpublished state exactly
// once. D2H always copies every valid page of a logical 64 KB eviction.

// buildFaultDriverWithDRAM is buildFaultDriver with a configurable GPU DRAM
// size (used to prove migration frames are released back to the device).
func buildFaultDriverWithDRAM(t *testing.T, dramSize uint64) (
	*Driver, *faultServiceMiddleware, []vm.PageTable,
) {
	t.Helper()

	cfg := DefaultUVMConfig()
	cfg.Enabled = true

	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)
	gpuTables := []vm.PageTable{vm.NewPageTable(12), vm.NewPageTable(12)}

	d := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGPUPageTables(gpuTables).
		WithGlobalStorage(mem.NewStorage(8 * mem.GB)).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	gpuPort := sim.NewPort(d, 1, 1, "TestGPU")
	d.RegisterGPU(gpuPort, DeviceProperties{CUCount: 4, DRAMSize: dramSize})

	return d, d.uvmFault, gpuTables
}

// pagesRange returns the allocation page indices [lo, hi] inclusive.
func pagesRange(lo, hi uint64) []uint64 {
	pages := make([]uint64, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		pages = append(pages, p)
	}
	return pages
}

// TestUVMDMARunCoalescing proves the one-to-one mapping between maximal
// contiguous PFN runs and superior DMA requests: 15 missing pages with
// contiguous CPU backing PAs and contiguous allocated destination frames emit
// exactly one MemCopyH2DReq, and a logical 64 KB eviction with contiguous GPU
// frames emits exactly one MemCopyD2HReq covering all 16 valid pages.
func TestUVMDMARunCoalescing(t *testing.T) {
	d, mw, gpuTables := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	// Distinct CPU backing bytes so the coalesced DMA payload is observable.
	// sbin_codex (todo 17): the TBN selection (15/16 valid pages occupied)
	// expands to the 2 MB block, so the migration also prefetches page 15:
	// 16 pages in one maximal run.
	cpuData := make([]byte, 16*basePageSize)
	for i := range cpuData {
		cpuData[i] = byte(i * 7)
	}
	for i := 0; i < 16; i++ {
		d.globalStorage.Write(reg.CPUBackingPages[i],
			cpuData[i*basePageSize:(i+1)*basePageSize])
	}

	// A fault on region 0: 15 demand pages, no pre-assigned destination
	// frames, so the migration allocates 16 contiguous GPU frames (15 demand
	// + 1 TBN prefetch).
	intakeFault(t, d, pid, 1, uint64(ptr))
	tx := mw.queue[0]
	mw.Tick()
	mw.Handle(tx.latencyEvent)
	mw.Tick()

	reqs := drainRequests(d)
	// Maximal runs map one-to-one to superior requests: the 16 pages have
	// contiguous CPU backing PAs AND contiguous allocated destination frames
	// -> exactly one MemCopyH2DReq covering the whole run.
	if len(reqs) != 1 {
		t.Fatalf("DMA reqs = %d, want 1 (one maximal run)", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("req = %T, want MemCopyH2DReq", reqs[0])
	}
	if len(h2d.SrcBuffer) != 16*basePageSize {
		t.Errorf("run bytes = %d, want %d", len(h2d.SrcBuffer), 16*basePageSize)
	}
	// The destination PFNs were allocated from the GPU device (contiguous,
	// starting at the GPU device's first frame).
	wantDst := uint64(4096) + 4*mem.GB
	if h2d.DstAddress != wantDst {
		t.Errorf("DstAddress = %#x, want %#x (first allocated GPU frame)",
			h2d.DstAddress, wantDst)
	}
	if !bytes.Equal(h2d.SrcBuffer, cpuData) {
		t.Error("coalesced payload mismatch")
	}
	// Reservation accounting: N = 16 pages reserved before the DMA.
	if got := d.uvm.Reservation().ReservedBytes(); got != 16*basePageSize {
		t.Errorf("reserved N = %d, want %d", got, 16*basePageSize)
	}
	for page := uint64(0); page < 16; page++ {
		if !maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d not marked in flight", page)
		}
	}
	if tx.phase != faultPhaseMigrating {
		t.Errorf("phase = %v, want migrating", tx.phase)
	}

	// One completion for the single run: residency + PTE publication + one
	// 64 KB TLB invalidation.
	deliverGeneralRsp(t, d, h2d)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbReq, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	if tlbReq.StartVA != 0 || tlbReq.Size != 64*mem.KB {
		t.Errorf("TLB invalidate = %#x+%d, want 0+64KB", tlbReq.StartVA, tlbReq.Size)
	}
	for page := uint64(0); page < 16; page++ {
		if !maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d not resident after migration", page)
		}
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d still in flight", page)
		}
		pte, found := gpuTables[0].Find(pid, reg.Base+page*basePageSize)
		if !found || pte.Location != vm.MemoryLocationGPU_LOCAL {
			t.Errorf("page %d PTE = %+v/%v, want GPU_LOCAL", page, pte, found)
		}
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 16*basePageSize {
		t.Errorf("resident R = %d, want %d", got, 16*basePageSize)
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d, want 0 after commit", got)
	}

	// D2H: a logical 64 KB eviction copies every valid page of the region as
	// one maximal run (uvm-manager.md §18.3).
	d2, _ := buildCopyDriver(t, false)
	ctx2 := d2.Init()
	d2.AllocateManagedMemory(ctx2, 128*mem.KB)
	reg2 := d2.uvm.registrations[0]
	makeRegionGPUResident(t, d2, reg2, 0, 1)

	transfer, err := d2.startEvictionD2H(reg2, 1, 64*mem.KB)
	if err != nil {
		t.Fatalf("startEvictionD2H: %v", err)
	}
	reqs = drainRequests(d2)
	if len(reqs) != 1 {
		t.Fatalf("D2H reqs = %d, want 1 (one maximal run of 16 pages)", len(reqs))
	}
	d2h, ok := reqs[0].(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("D2H req = %T, want MemCopyD2HReq", reqs[0])
	}
	wantSrc := uint64(0x2_0000_0000) + 15*basePageSize
	if d2h.SrcAddress != wantSrc {
		t.Errorf("D2H SrcAddress = %#x, want %#x", d2h.SrcAddress, wantSrc)
	}
	if len(d2h.DstBuffer) != 16*basePageSize {
		t.Errorf("D2H bytes = %d, want %d (all 16 valid pages)",
			len(d2h.DstBuffer), 16*basePageSize)
	}

	// Completion writes the run's buffer to the CPU backing frames.
	evictData := make([]byte, 16*basePageSize)
	for i := range evictData {
		evictData[i] = byte(0xA0 + i%16)
	}
	copy(d2h.DstBuffer, evictData)
	rsp := sim.GeneralRspBuilder{}.
		WithSrc(d2.gpuPort.AsRemote()).
		WithDst(d2.gpuPort.AsRemote()).
		WithOriginalReq(d2h).
		Build()
	if !transfer.processRsp(rsp) {
		t.Fatal("D2H completion not accepted")
	}
	for i := 0; i < 16; i++ {
		got, _ := d2.globalStorage.Read(reg2.CPUBackingPages[15+i], basePageSize)
		if !bytes.Equal(got, evictData[i*basePageSize:(i+1)*basePageSize]) {
			t.Errorf("CPU backing page %d mismatch after D2H", 15+i)
		}
	}
}

// TestUVMFragmentedRuns proves that holes and non-contiguous PFN groups split
// the migration into multiple maximal runs, each emitting exactly one
// superior request, and that resident pages are removed from the migration
// mask (never re-copied, never in flight).
func TestUVMFragmentedRuns(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	// Fixture A: two non-contiguous destination groups. Pages 0..4 have
	// pre-assigned frames at 0x2_0000_0000+; pages 5..14 get freshly
	// allocated frames (contiguous, starting at the GPU device's first
	// frame). The two groups are non-contiguous -> two maximal runs.
	// sbin_codex (todo 17): the TBN selection expands to the 2 MB block, so
	// page 15 joins the fresh-frame group: run 1 covers pages 5..15.
	block := reg.VABlocks[0]
	for page := uint64(0); page <= 4; page++ {
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		block.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}

	intakeFault(t, d, pid, 1, uint64(ptr))
	tx := mw.queue[0]
	mw.Tick()
	mw.Handle(tx.latencyEvent)
	mw.Tick()

	reqs := drainRequests(d)
	if len(reqs) != 2 {
		t.Fatalf("DMA reqs = %d, want 2 (two maximal runs)", len(reqs))
	}
	run0, ok0 := reqs[0].(*protocol.MemCopyH2DReq)
	run1, ok1 := reqs[1].(*protocol.MemCopyH2DReq)
	if !ok0 || !ok1 {
		t.Fatalf("reqs = %T, %T; want MemCopyH2DReq", reqs[0], reqs[1])
	}
	if run0.DstAddress != 0x2_0000_0000 || len(run0.SrcBuffer) != 5*basePageSize {
		t.Errorf("run 0 = %#x+%d, want 0x2_0000_0000+%d",
			run0.DstAddress, len(run0.SrcBuffer), 5*basePageSize)
	}
	if run1.DstAddress != uint64(4096)+4*mem.GB || len(run1.SrcBuffer) != 11*basePageSize {
		t.Errorf("run 1 = %#x+%d, want first GPU frame+%d",
			run1.DstAddress, len(run1.SrcBuffer), 11*basePageSize)
	}
	// The two runs' payloads are the CPU backing bytes of their pages.
	want0 := make([]byte, 0, 5*basePageSize)
	for page := uint64(0); page <= 4; page++ {
		data, _ := d.globalStorage.Read(reg.CPUBackingPages[page], basePageSize)
		want0 = append(want0, data...)
	}
	if !bytes.Equal(run0.SrcBuffer, want0) {
		t.Error("run 0 payload mismatch")
	}

	// Complete both runs -> residency for all 15 pages.
	deliverGeneralRsp(t, d, run0)
	deliverGeneralRsp(t, d, run1)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	if _, ok := reqs[0].(*protocol.UVMTLBInvalidateReq); !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	for page := uint64(0); page < 16; page++ {
		if !maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d not resident", page)
		}
	}

	// Fixture B: resident holes. Pages 3 and 7 are already resident; the
	// missing pages form runs [0..2], [4..6], [8..14]. sbin_codex (todo 17):
	// the TBN selection expands to the 2 MB block and prefetches page 15, so
	// the last run covers [8..15].
	d2, mw2, _ := buildFaultDriver(t, false)
	ctx2 := d2.Init()
	pid2 := ctx2.pid
	ptr2 := d2.AllocateManagedMemory(ctx2, 64*mem.KB)
	reg2 := d2.uvm.registrations[0]
	block2 := reg2.VABlocks[0]
	for _, page := range []uint64{3, 7} {
		setResident(reg2, page, true)
		blockLocal := (reg2.Base + page*basePageSize - block2.StartVA) / basePageSize
		block2.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}
	if err := d2.uvm.Reservation().ReserveAdmission(2 * basePageSize); err != nil {
		t.Fatalf("ReserveAdmission: %v", err)
	}
	d2.uvm.Reservation().CommitAdmission(2 * basePageSize)

	intakeFault(t, d2, pid2, 1, uint64(ptr2))
	tx2 := mw2.queue[0]
	mw2.Tick()
	mw2.Handle(tx2.latencyEvent)
	mw2.Tick()

	reqs = drainRequests(d2)
	if len(reqs) != 3 {
		t.Fatalf("DMA reqs = %d, want 3 (runs [0..2], [4..6], [8..15])", len(reqs))
	}
	for i, wantLen := range []int{3, 3, 8} {
		h2d, ok := reqs[i].(*protocol.MemCopyH2DReq)
		if !ok {
			t.Fatalf("run %d = %T, want MemCopyH2DReq", i, reqs[i])
		}
		if len(h2d.SrcBuffer) != wantLen*basePageSize {
			t.Errorf("run %d bytes = %d, want %d pages", i, len(h2d.SrcBuffer), wantLen)
		}
	}
	// The resident pages are not in flight and not re-copied.
	for _, page := range []uint64{3, 7} {
		if maskBit(reg2.InFlightMask, page) {
			t.Errorf("resident page %d marked in flight", page)
		}
	}
}

// TestUVMReservationAccounting proves the exact R/I/N reservation accounting
// of a migration: prepare reserves N before the DMA, commit moves N to R,
// rollback releases N exactly once and returns the allocated frames, and a
// rejected reservation mutates nothing.
func TestUVMReservationAccounting(t *testing.T) {
	d, _, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	tx := &faultTransaction{
		PID:        pid,
		GPU:        1,
		RegionBase: 0,
		Key:        copyRegionKey{PID: pid, GPU: 1, RegionBase: 0},
		reg:        reg,
	}
	missing := pagesRange(0, 4)

	// Prepare: the reservation happens BEFORE the destination frame
	// allocation; N = 5 pages, in-flight set, one maximal run.
	plan, err := d.uvm.prepareFaultMigration(tx, missing)
	if err != nil {
		t.Fatalf("prepareFaultMigration: %v", err)
	}
	tx.plan = plan
	if got := d.uvm.Reservation().ReservedBytes(); got != 5*basePageSize {
		t.Errorf("reserved N = %d, want %d", got, 5*basePageSize)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 0 {
		t.Errorf("resident R = %d, want 0", got)
	}
	if got := d.uvm.Reservation().InFlightBytes(); got != 0 {
		t.Errorf("in-flight I = %d, want 0 (H2D reservations are N)", got)
	}
	for _, page := range missing {
		if !maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d not in flight", page)
		}
	}
	if plan.PageCount != 5 || plan.TotalBytes != 5*basePageSize ||
		len(plan.Runs) != 1 {
		t.Errorf("plan = %d pages/%d bytes/%d runs, want 5/%d/1",
			plan.PageCount, plan.TotalBytes, len(plan.Runs), 5*basePageSize)
	}

	// Commit: publish residency (commitFaultMigration), then commit the
	// admission (completeFaultMigration): N -> R.
	pages, err := d.uvm.commitFaultMigration(tx)
	if err != nil {
		t.Fatalf("commitFaultMigration: %v", err)
	}
	if len(pages) != 5 {
		t.Fatalf("committed pages = %d, want 5", len(pages))
	}
	// The admission is still reserved until completeFaultMigration commits it.
	if got := d.uvm.Reservation().ReservedBytes(); got != 5*basePageSize {
		t.Errorf("reserved N = %d before admission commit, want %d",
			got, 5*basePageSize)
	}
	// Walk the region to MIGRATING_TO_GPU so completeFaultMigration can commit.
	sm := NewRegionStateMachine(
		RegionContext{PID: pid, GPU: 1, Block: 0, Region: 0},
		reg.VABlocks[0].SubBlocks[0])
	for _, to := range []RegionState{RegionFaultPending, RegionMigratingToGPU} {
		if err := sm.Transition(to, sim.VTimeInSec(1)); err != nil {
			t.Fatalf("Transition(%s): %v", to, err)
		}
	}
	if err := d.uvm.completeFaultMigration(
		tx, 5*basePageSize, sim.VTimeInSec(1)); err != nil {
		t.Fatalf("completeFaultMigration: %v", err)
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d, want 0 after commit", got)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 5*basePageSize {
		t.Errorf("resident R = %d, want %d", got, 5*basePageSize)
	}
	for _, page := range missing {
		if !maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d not resident", page)
		}
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d still in flight", page)
		}
	}

	// Rollback: a second migration prepares, then rolls back exactly once:
	// N -> 0, in-flight cleared, allocated frames freed and reset.
	tx2 := &faultTransaction{
		PID:        pid,
		GPU:        1,
		RegionBase: 0,
		Key:        copyRegionKey{PID: pid, GPU: 1, RegionBase: 0},
		reg:        reg,
	}
	missing2 := pagesRange(5, 9)
	plan2, err := d.uvm.prepareFaultMigration(tx2, missing2)
	if err != nil {
		t.Fatalf("prepareFaultMigration #2: %v", err)
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 5*basePageSize {
		t.Errorf("reserved N = %d, want %d", got, 5*basePageSize)
	}
	if len(plan2.frames) != 5 {
		t.Fatalf("allocated frames = %d, want 5", len(plan2.frames))
	}
	d.uvm.rollbackFaultMigration(tx2, plan2)
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d, want 0 after rollback", got)
	}
	for _, page := range missing2 {
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d still in flight after rollback", page)
		}
		blockLocal := (reg.Base + page*basePageSize - reg.VABlocks[0].StartVA) /
			basePageSize
		if reg.VABlocks[0].Pages[blockLocal].GPUPhysicalPage != 0 {
			t.Errorf("page %d destination frame not reset after rollback", page)
		}
	}
	// A second rollback is a no-op (released exactly once).
	d.uvm.rollbackFaultMigration(tx2, plan2)
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d after double rollback, want 0", got)
	}

	// Reservation failure: with a 64 KB UVM capacity, a second 60 KB
	// migration cannot reserve; nothing is mutated.
	d2, _, _ := buildFaultDriverWithDRAM(t, 4*mem.GB)
	d2.uvm = NewUVMManager(DefaultUVMConfig(), 64*mem.KB)
	d2.uvm.SetFrameAllocator(d2)
	ctx2 := d2.Init()
	pid2 := ctx2.pid
	d2.AllocateManagedMemory(ctx2, 64*mem.KB)
	reg2 := d2.uvm.registrations[0]
	full := pagesRange(0, 14)
	txA := &faultTransaction{PID: pid2, GPU: 1, RegionBase: 0, reg: reg2}
	if _, err := d2.uvm.prepareFaultMigration(txA, full); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	txB := &faultTransaction{PID: pid2, GPU: 1, RegionBase: 0, reg: reg2}
	if _, err := d2.uvm.prepareFaultMigration(txB, full); err == nil {
		t.Fatal("second prepare must fail: R+I+N+60KB > 64KB capacity")
	}
	if got := d2.uvm.Reservation().ReservedBytes(); got != 15*basePageSize {
		t.Errorf("reserved N = %d after rejected prepare, want %d",
			got, 15*basePageSize)
	}
	// The rejected prepare mutated no in-flight state.
	for page := uint64(0); page < 15; page++ {
		if !maskBit(reg2.InFlightMask, page) {
			t.Errorf("page %d in-flight cleared by a rejected prepare", page)
		}
	}

	// Frame release: with a 16-frame GPU device, four unreleased 5-page
	// prepares over distinct pages exhaust the device; with a rollback
	// between prepares the frames are returned and every prepare succeeds.
	d3, _, _ := buildFaultDriverWithDRAM(t, 64*mem.KB)
	ctx3 := d3.Init()
	pid3 := ctx3.pid
	d3.AllocateManagedMemory(ctx3, 256*mem.KB)
	reg3 := d3.uvm.registrations[0]
	for i := 0; i < 4; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("prepare %d panicked (frames not released): %v", i, r)
				}
			}()
			tx3 := &faultTransaction{PID: pid3, GPU: 1, RegionBase: 0, reg: reg3}
			plan3, err := d3.uvm.prepareFaultMigration(
				tx3, pagesRange(uint64(5*i), uint64(5*i+4)))
			if err != nil {
				t.Fatalf("prepare %d: %v", i, err)
			}
			d3.uvm.rollbackFaultMigration(tx3, plan3)
		}()
	}
	// Without rollback, the device exhausts: 4 x 5 distinct pages > 16 frames.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("prepare without rollback must exhaust the 16-frame device")
			}
		}()
		for i := 0; i < 4; i++ {
			tx3 := &faultTransaction{PID: pid3, GPU: 1, RegionBase: 0, reg: reg3}
			if _, err := d3.uvm.prepareFaultMigration(
				tx3, pagesRange(uint64(5*i), uint64(5*i+4))); err != nil {
				t.Fatalf("prepare %d: %v", i, err)
			}
		}
	}()
}

// TestUVMSecondRunRollback proves the injected-failure contract: when the
// second run of a two-run migration fails, the unpublished state (reservation,
// in-flight marks, allocated frames) rolls back exactly once, no residency or
// PTE is published before ALL runs succeed, and the transaction retries.
func TestUVMSecondRunRollback(t *testing.T) {
	d, mw, gpuTables := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	// Two maximal runs: pages 0..4 have pre-assigned frames at
	// 0x2_0000_0000+; pages 5..14 get allocated frames (non-contiguous with
	// the pre-assigned group). sbin_codex (todo 17): the TBN selection
	// expands to the 2 MB block, so page 15 joins the fresh-frame group:
	// run 1 covers pages 5..15 (11 pages).
	block := reg.VABlocks[0]
	for page := uint64(0); page <= 4; page++ {
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		block.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}

	intakeFault(t, d, pid, 1, uint64(ptr))
	tx := mw.queue[0]
	mw.Tick()
	mw.Handle(tx.latencyEvent)

	// Inject a failure after the first run's request: the second run is not
	// emitted and the unpublished state rolls back.
	mw.failAfterRuns = 1
	mw.Tick()

	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("emitted reqs = %d, want 1 (first run only)", len(reqs))
	}
	run0, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok || run0.DstAddress != 0x2_0000_0000 ||
		len(run0.SrcBuffer) != 5*basePageSize {
		t.Fatalf("first run req = %+v, want 0x2_0000_0000+20KB", reqs[0])
	}

	// Rollback: reservation released, in-flight cleared, no residency, no PTE
	// publication (commit happens only after ALL runs succeed).
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d after rollback, want 0", got)
	}
	for page := uint64(0); page < 16; page++ {
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d still in flight after rollback", page)
		}
		if maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d resident despite failed migration", page)
		}
	}
	for page := uint64(5); page <= 15; page++ {
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		if block.Pages[blockLocal].GPUPhysicalPage != 0 {
			t.Errorf("page %d destination frame not released after rollback", page)
		}
	}
	if pte, found := gpuTables[0].Find(pid, uint64(ptr)); found &&
		pte.Location == vm.MemoryLocationGPU_LOCAL {
		t.Error("PTE published despite failed migration")
	}

	// The transaction retries on the next tick: the migration re-prepares
	// (re-reserves, re-allocates) and this time emits both runs.
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 2 {
		t.Fatalf("retry reqs = %d, want 2 (both runs)", len(reqs))
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 16*basePageSize {
		t.Errorf("reserved N = %d after retry, want %d", got, 16*basePageSize)
	}

	// Complete both runs -> residency + PTE + one TLB invalidation.
	for _, req := range reqs {
		deliverGeneralRsp(t, d, req)
	}
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	if _, ok := reqs[0].(*protocol.UVMTLBInvalidateReq); !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	for page := uint64(0); page < 16; page++ {
		if !maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d not resident after retry", page)
		}
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 16*basePageSize {
		t.Errorf("resident R = %d, want %d", got, 16*basePageSize)
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d, want 0 after commit", got)
	}
}