package driver

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: reactive 64 KB eviction contract tests (todo 19 of plan
// mgpusim-uvm-manager, uvm-manager.md §18, §19, §21.4, §31.2). The QA regex
// 'TestUVMReactiveEviction(BlockBarrier|LRUOrdinal|CachePTEAndTLBOrder|
// FragmentedD2H|PinnedExclusion|CopyVictimExclusion|CopyEvictionNoCycle|
// StageFailure)' runs the fixtures in this file: deterministic migration-recency
// LRU victim selection (§18.1/§31.2), the EVICTING marker, the one commandID
// block barrier with each gate's Todo-8 watermark ack, the exact
// block -> WB+INV -> transition PTE/generation -> TLB -> D2H -> final PTE ->
// free -> replay -> unblock order (§19/§21.4), every-valid-page D2H even when
// clean (§18.3), pinned/COPY-owned victim exclusion (§18.2, todo 5), no
// ownership cycle for waiting copies, and injected stage failures that
// preserve authority and prevent premature free/PTE publication.

// buildEvictionDriver builds a real driver (real allocator, real CPU + GPU
// page tables, real UVM manager, host storage) with the reactive eviction
// middleware wired, plus a registered GPU port.
func buildEvictionDriver(t *testing.T, accessCounter bool) (
	*Driver, *evictionMiddleware, []vm.PageTable,
) {
	t.Helper()

	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = accessCounter

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
	d.RegisterGPU(gpuPort, DeviceProperties{CUCount: 4, DRAMSize: 4 * mem.GB})

	return d, d.uvmEviction, gpuTables
}

// makeRegionGPUResidentViaFault drives the FIFO fault service to make the
// 64 KB region containing vaddr GPU-resident through the real migration path:
// real device frames, real GPU_LOCAL PTEs, real committed reservation. The
// helper handles any number of H2D runs (fragmented pre-assigned frames).
func makeRegionGPUResidentViaFault(
	t *testing.T,
	d *Driver,
	pid vm.PID,
	vaddr uint64,
) {
	t.Helper()

	mw := d.uvmFault
	intakeFault(t, d, pid, 1, vaddr)
	if !mw.Tick() {
		t.Fatal("fault tick did not start the transaction")
	}
	if mw.active == nil {
		t.Fatal("no active fault transaction")
	}
	mw.Handle(mw.active.latencyEvent)
	if !mw.Tick() {
		t.Fatal("fault tick did not service the transaction")
	}
	reqs := drainRequests(d)
	if len(reqs) == 0 {
		t.Fatal("fault service issued no H2D requests")
	}
	for _, req := range reqs {
		h2d, ok := req.(*protocol.MemCopyH2DReq)
		if !ok {
			t.Fatalf("fault service request = %T, want MemCopyH2DReq", req)
		}
		deliverGeneralRsp(t, d, h2d)
	}
	if !mw.Tick() {
		t.Fatal("fault tick did not complete the migration")
	}
	reqs = drainRequests(d)
	// sbin_codex (todo 25): §21.2 — an AC-off fault migration is
	// INVALID -> GPU_LOCAL, which needs no TLB invalidation; only the AC-on
	// (REMOTE -> GPU_LOCAL) path invalidates the cached REMOTE translation.
	if d.uvm.config.AccessCounter {
		if len(reqs) != 1 {
			t.Fatalf("post-DMA requests = %d, want 1 TLB", len(reqs))
		}
		tlb, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
		if !ok {
			t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
		}
		deliverTLBAck(t, d, tlb)
		if !mw.Tick() {
			t.Fatal("fault tick did not start the replay")
		}
		reqs = drainRequests(d)
	}
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 replay", len(reqs))
	}
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay)
	if !mw.Tick() {
		t.Fatal("fault tick did not retire the transaction")
	}
	if mw.active != nil {
		t.Fatal("fault transaction not retired")
	}
}

// driveEvictionToEnd drives the active eviction transaction through every
// stage until it is done (a completed eviction or an injected-stage abort).
// Each stage's request is asserted to be the exactly-one expected message.
func driveEvictionToEnd(
	t *testing.T,
	d *Driver,
	mw *evictionMiddleware,
	pid vm.PID,
) {
	t.Helper()

	tx := mw.active
	if tx == nil {
		t.Fatal("no active eviction transaction")
	}

	// Claim -> block (or abort at the blocking stage).
	mw.Tick()
	if tx.phase == evictionStageDone {
		return
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 block", len(reqs))
	}
	block, ok := reqs[0].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request = %T, want BlockRange", reqs[0])
	}
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	if tx.phase == evictionStageDone {
		return
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 flush", len(reqs))
	}
	flush, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("request = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	deliverFlushRsp(t, d, flush)
	mw.Tick()
	if tx.phase == evictionStageDone {
		return
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 TLB", len(reqs))
	}
	tlb, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, tlb)
	mw.Tick()
	if tx.phase == evictionStageDone {
		return
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 D2H", len(reqs))
	}
	d2h, ok := reqs[0].(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyD2HReq", reqs[0])
	}
	deliverGeneralRsp(t, d, d2h)
	mw.Tick()
	if tx.phase == evictionStageDone {
		return
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 replay", len(reqs))
	}
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay)
	mw.Tick()
	if tx.phase == evictionStageDone {
		return
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 unblock", len(reqs))
	}
	unblock, ok := reqs[0].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("request = %T, want UnblockRange", reqs[0])
	}
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()
}

// TestUVMReactiveEvictionBlockBarrier proves the eviction transaction issues
// the one commandID BlockRange FIRST and waits each gate's Todo-8 watermark
// ack before any WB+INV/PTE/TLB/D2H work, then follows the exact §19/§21.4
// order to completion with the EVICTING marker held throughout.
func TestUVMReactiveEvictionBlockBarrier(t *testing.T) {
	d, mw, gpuTables := buildEvictionDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 60*mem.KB)
	reg := d.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr))
	region := reg.VABlocks[0].SubBlocks[0]
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}

	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	tx := mw.active

	// EVICTING marker: region state + ownership slot + coalescing entry.
	if region.State != RegionEvictPending {
		t.Errorf("region state = %s, want EVICT_PENDING", region.State)
	}
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipEviction || owner != tx.Ticket {
		t.Errorf("owner = %v/%d, want EVICTION/%d", typ, owner, tx.Ticket)
	}
	if d.uvm.evictByKey[key] != tx {
		t.Error("evictByKey entry missing")
	}

	// Claim -> the one commandID BlockRange is the FIRST request.
	if !mw.Tick() {
		t.Fatal("tick did not claim and block")
	}
	if tx.phase != evictionStageBlocking {
		t.Fatalf("phase = %v, want blocking", tx.phase)
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 BlockRange", len(reqs))
	}
	block, ok := reqs[0].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request = %T, want BlockRange", reqs[0])
	}
	if block.CommandID != tx.Ticket {
		t.Errorf("block commandID = %d, want ticket %d", block.CommandID, tx.Ticket)
	}
	if block.StartVA != 0 || block.Size != 64*mem.KB {
		t.Errorf("block range = %#x+%d, want 0+64KB", block.StartVA, block.Size)
	}

	// The transaction waits for the watermark completion before any work.
	if mw.Tick() {
		t.Error("transaction progressed without the block completion")
	}
	if len(d.requestsToSend) != 0 {
		t.Error("requests queued without the block ack")
	}
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationGPU_LOCAL {
		t.Error("PTE changed before the block completion")
	}

	// Block ack -> cache WB+INV (Todo 13) over the region's valid pages.
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-block requests = %d, want 1 flush", len(reqs))
	}
	flush, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("post-block request = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	if flush.Operation != cache.UVMCacheRangeFlushWritebackInvalidate {
		t.Errorf("flush op = %v, want WB+INV", flush.Operation)
	}
	if flush.VABase != 0 {
		t.Errorf("flush VABase = %#x, want 0", flush.VABase)
	}
	// The misaligned 64 KB allocation has 15 valid pages (VA 4096..65536):
	// region-local bits 1..15.
	if flush.ValidPageMask != 0xFFFE {
		t.Errorf("flush mask = %#x, want 0xFFFE", flush.ValidPageMask)
	}
	total := uint64(0)
	for _, run := range flush.PhysicalRuns {
		total += run.Length
	}
	if total != 15*basePageSize {
		t.Errorf("flush runs cover %d bytes, want 60KB", total)
	}

	// No PTE transition / TLB / D2H before the flush ack.
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationGPU_LOCAL {
		t.Error("PTE transitioned before the flush completion")
	}
	if got := d.uvm.Generation(); got != 0 {
		t.Errorf("generation = %d, want 0 before the flush ack", got)
	}

	// Flush ack -> PTE transition (GPU_LOCAL -> INVALID) + generation + TLB.
	deliverFlushRsp(t, d, flush)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-flush requests = %d, want 1 TLB", len(reqs))
	}
	tlb, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-flush request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	if tlb.StartVA != 0 || tlb.Size != 64*mem.KB {
		t.Errorf("TLB = %#x+%d, want 0+64KB", tlb.StartVA, tlb.Size)
	}
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationINVALID {
		t.Errorf("PTE = %+v/%v, want INVALID after the transition", pte, found)
	}
	if got := d.uvm.Generation(); got != 1 {
		t.Errorf("generation = %d, want 1 after the transition", got)
	}
	if region.State != RegionMigratingToCPU {
		t.Errorf("region state = %s, want MIGRATING_TO_CPU", region.State)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 0 {
		t.Errorf("resident R = %d, want 0 after StartMigration", got)
	}
	if got := d.uvm.Reservation().InFlightBytes(); got != 15*basePageSize {
		t.Errorf("in-flight I = %d, want 60KB", got)
	}

	// TLB ack -> D2H of every valid page.
	deliverTLBAck(t, d, tlb)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 D2H", len(reqs))
	}
	d2h, ok := reqs[0].(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want MemCopyD2HReq", reqs[0])
	}
	if len(d2h.DstBuffer) != 15*basePageSize {
		t.Errorf("D2H payload = %d bytes, want 60KB", len(d2h.DstBuffer))
	}
	if tx.phase != evictionStageD2H {
		t.Fatalf("phase = %v, want d2h", tx.phase)
	}

	// D2H ack -> final PTE (INVALID, AC off) + free + CPU_RESIDENT + replay.
	deliverGeneralRsp(t, d, d2h)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-D2H requests = %d, want 1 replay", len(reqs))
	}
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-D2H request = %T, want UVMFaultReplayReq", reqs[0])
	}
	if replay.StartVA != 0 || replay.Size != 64*mem.KB {
		t.Errorf("replay = %#x+%d, want 0+64KB", replay.StartVA, replay.Size)
	}
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationINVALID {
		t.Errorf("final PTE = %+v/%v, want INVALID (AC off)", pte, found)
	}
	if ps := d.uvm.pageStateLocked(reg, 0); ps.GPUPhysicalPage != 0 {
		t.Errorf("GPU frame not freed: %#x", ps.GPUPhysicalPage)
	}
	if maskBit(reg.ResidentMask, 0) {
		t.Error("resident bit still set after the eviction")
	}
	if region.State != RegionCPUResident {
		t.Errorf("region state = %s, want CPU_RESIDENT", region.State)
	}
	if got := d.uvm.Reservation().InFlightBytes(); got != 0 {
		t.Errorf("in-flight I = %d, want 0 after CompleteMigrationToCPU", got)
	}

	// Replay ack -> unblock; the ownership slot is released before the
	// unblock completes.
	deliverReplayAck(t, d, replay)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-replay requests = %d, want 1 UnblockRange", len(reqs))
	}
	unblock, ok := reqs[0].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request = %T, want UnblockRange", reqs[0])
	}
	if unblock.CommandID != tx.Ticket {
		t.Errorf("unblock commandID = %d, want ticket %d", unblock.CommandID, tx.Ticket)
	}
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipIdle {
		t.Errorf("owner = %v/%d, want IDLE before the unblock completes", typ, owner)
	}

	// Unblock ack -> done: coalescing entry removed, tickets woken.
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()
	if tx.phase != evictionStageDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	if mw.active != nil {
		t.Error("transaction not retired")
	}
	if d.uvm.evictByKey[key] != nil {
		t.Error("evictByKey entry not removed")
	}
}

// TestUVMReactiveEvictionLRUOrdinal proves deterministic migration-recency LRU
// victim selection (§18.1/§31.2): the oldest GPU-resident region is selected,
// equal recency breaks to the lower VA, the EVICTING mark does not update
// recency, and a completed eviction never selects the same region twice.
func TestUVMReactiveEvictionLRUOrdinal(t *testing.T) {
	d, mw, _ := buildEvictionDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg := d.uvm.registrations[0]
	// The fault on region 1 (VA 64KB) selects the 128 KB node [0, 128KB): the
	// migration makes region 1 (16 demand pages) and region 0 (15 prefetched
	// pages) GPU-resident.
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr)+64*mem.KB)
	region0 := reg.VABlocks[0].SubBlocks[0]
	region1 := reg.VABlocks[0].SubBlocks[1]
	if region0.State != RegionGPUResident || region1.State != RegionGPUResident {
		t.Fatalf("regions = %s/%s, want GPU_RESIDENT/GPU_RESIDENT",
			region0.State, region1.State)
	}
	// Distinct recency: region 0 older (10) than region 1 (20).
	region0.LastMigrationTime = 10
	region1.LastMigrationTime = 20

	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	tx := mw.active
	if tx.RegionBase != 0 {
		t.Errorf("first victim = %#x, want region 0 (older)", tx.RegionBase)
	}
	if region0.State != RegionEvictPending {
		t.Errorf("region 0 state = %s, want EVICT_PENDING", region0.State)
	}
	if region1.State != RegionGPUResident {
		t.Errorf("region 1 state = %s, want GPU_RESIDENT", region1.State)
	}
	// The EVICTING mark is not a migration/admission: recency is unchanged.
	if region0.LastMigrationTime != 10 {
		t.Errorf("recency changed by the EVICTING mark: %v",
			region0.LastMigrationTime)
	}

	// Complete the first eviction; the same region is never selected twice.
	driveEvictionToEnd(t, d, mw, pid)
	if tx.phase != evictionStageDone {
		t.Fatalf("first eviction phase = %v, want done", tx.phase)
	}
	if region0.State != RegionCPUResident {
		t.Errorf("region 0 state = %s, want CPU_RESIDENT", region0.State)
	}

	// The second eviction selects the remaining GPU-resident region.
	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("second intake: %v", err)
	}
	tx2 := mw.active
	if tx2.RegionBase != 64*mem.KB {
		t.Errorf("second victim = %#x, want region 1", tx2.RegionBase)
	}
	if region1.State != RegionEvictPending {
		t.Errorf("region 1 state = %s, want EVICT_PENDING", region1.State)
	}

	// Tie-break: equal recency selects the lower VA region deterministically.
	d2, mw2, _ := buildEvictionDriver(t, false)
	ctx2 := d2.Init()
	pid2 := ctx2.pid
	ptr2 := d2.AllocateManagedMemory(ctx2, 128*mem.KB)
	reg2 := d2.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d2, pid2, uint64(ptr2)+64*mem.KB)
	reg2.VABlocks[0].SubBlocks[0].LastMigrationTime = 10
	reg2.VABlocks[0].SubBlocks[1].LastMigrationTime = 10
	if err := mw2.intake(pid2, 1); err != nil {
		t.Fatalf("tie intake: %v", err)
	}
	if mw2.active.RegionBase != 0 {
		t.Errorf("tie victim = %#x, want region 0 (lower VA)",
			mw2.active.RegionBase)
	}
}

// TestUVMReactiveEvictionCachePTEAndTLBOrder proves the exact §21.4 ordering
// with the Access Counter on: WB+INV precedes the PTE transition, the
// transition PTE (INVALID) and generation precede the TLB invalidate, the D2H
// follows the TLB ack, and the final REMOTE PTE (pointing at the CPU backing)
// is published only after the D2H completes — followed by free, replay,
// unblock.
func TestUVMReactiveEvictionCachePTEAndTLBOrder(t *testing.T) {
	d, mw, gpuTables := buildEvictionDriver(t, true)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 60*mem.KB)
	reg := d.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr))
	region := reg.VABlocks[0].SubBlocks[0]
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}

	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	tx := mw.active
	mw.Tick()
	reqs := drainRequests(d)
	block := reqs[0].(*vm.BlockRange)
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	reqs = drainRequests(d)
	flush := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if flush.Operation != cache.UVMCacheRangeFlushWritebackInvalidate {
		t.Fatalf("flush op = %v, want WB+INV", flush.Operation)
	}
	// WB+INV precedes the PTE transition: the mapping is still GPU_LOCAL.
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationGPU_LOCAL {
		t.Error("PTE transitioned before the WB+INV completion")
	}

	deliverFlushRsp(t, d, flush)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-flush requests = %d, want 1 TLB", len(reqs))
	}
	tlb := reqs[0].(*protocol.UVMTLBInvalidateReq)
	// The transition PTE (INVALID) and generation precede the TLB invalidate.
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationINVALID {
		t.Errorf("transition PTE = %+v/%v, want INVALID", pte, found)
	}
	if got := d.uvm.Generation(); got != 1 {
		t.Errorf("generation = %d, want 1", got)
	}

	// No D2H before the TLB ack.
	deliverTLBAck(t, d, tlb)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 D2H", len(reqs))
	}
	d2h := reqs[0].(*protocol.MemCopyD2HReq)
	// The final PTE is NOT published before the D2H completes.
	if pte, found := gpuTables[0].Find(pid, reg.Base); !found ||
		pte.Location != vm.MemoryLocationINVALID {
		t.Error("final PTE published before the D2H completion")
	}

	deliverGeneralRsp(t, d, d2h)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-D2H requests = %d, want 1 replay", len(reqs))
	}
	replay := reqs[0].(*protocol.UVMFaultReplayReq)
	// Final PTE by mode: REMOTE (AC on), pointing at the CPU backing.
	for page := uint64(0); page < 15; page++ {
		pte, found := gpuTables[0].Find(pid, reg.Base+page*basePageSize)
		if !found || pte.Location != vm.MemoryLocationCPU_REMOTE {
			t.Errorf("page %d final PTE = %+v/%v, want CPU_REMOTE",
				page, pte, found)
		}
		if pte.PAddr != reg.CPUBackingPages[page] {
			t.Errorf("page %d final PAddr = %#x, want CPU backing %#x",
				page, pte.PAddr, reg.CPUBackingPages[page])
		}
	}
	if region.State != RegionCPUResident {
		t.Errorf("region state = %s, want CPU_RESIDENT", region.State)
	}
	if got := d.uvm.Reservation().InFlightBytes(); got != 0 {
		t.Errorf("in-flight I = %d, want 0", got)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 0 {
		t.Errorf("resident R = %d, want 0 after the eviction", got)
	}

	deliverReplayAck(t, d, replay)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-replay requests = %d, want 1 unblock", len(reqs))
	}
	unblock := reqs[0].(*vm.UnblockRange)
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipIdle {
		t.Errorf("owner = %v/%d, want IDLE", typ, owner)
	}
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()
	if tx.phase != evictionStageDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
}

// TestUVMReactiveEvictionFragmentedD2H proves every valid page of the region
// is copied D2H even when the GPU frames are fragmented and the region is
// clean (§18.3: the clean D2H is never omitted): one flush run and one D2H
// request per fragmented page, exact 64 KB-region accounting, and the CPU
// backing receives every page's bytes.
func TestUVMReactiveEvictionFragmentedD2H(t *testing.T) {
	d, mw, _ := buildEvictionDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 60*mem.KB)
	reg := d.uvm.registrations[0]

	// Fragment the GPU frames BEFORE the fault: pre-assigned frames are used
	// as-is by the migration (todo 16), so the D2H forms one run per page.
	for i := uint64(0); i < 15; i++ {
		d.uvm.pageStateLocked(reg, i).GPUPhysicalPage =
			0x1_0000_0000 + i*2*basePageSize
	}
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr))

	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	tx := mw.active
	mw.Tick()
	reqs := drainRequests(d)
	block := reqs[0].(*vm.BlockRange)
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	reqs = drainRequests(d)
	flush := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if len(flush.PhysicalRuns) != 15 {
		t.Errorf("flush runs = %d, want 15 fragmented runs",
			len(flush.PhysicalRuns))
	}
	if flush.ValidPageMask != 0xFFFE {
		t.Errorf("flush mask = %#x, want 0xFFFE", flush.ValidPageMask)
	}
	deliverFlushRsp(t, d, flush)
	mw.Tick()
	reqs = drainRequests(d)
	tlb := reqs[0].(*protocol.UVMTLBInvalidateReq)
	deliverTLBAck(t, d, tlb)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 15 {
		t.Fatalf("D2H reqs = %d, want 15 (one per fragmented page)", len(reqs))
	}
	// Every valid page is copied — the clean D2H is never omitted (§18.3).
	total := uint64(0)
	for _, req := range reqs {
		d2h, ok := req.(*protocol.MemCopyD2HReq)
		if !ok {
			t.Fatalf("D2H request = %T", req)
		}
		total += uint64(len(d2h.DstBuffer))
		// Simulate the DMA result: distinct bytes per page.
		for j := range d2h.DstBuffer {
			d2h.DstBuffer[j] = byte(j % 251)
		}
	}
	if total != 15*basePageSize {
		t.Errorf("D2H bytes = %d, want 60KB (every valid page)", total)
	}
	for _, req := range reqs {
		deliverGeneralRsp(t, d, req)
	}
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-D2H requests = %d, want 1 replay", len(reqs))
	}
	replay := reqs[0].(*protocol.UVMFaultReplayReq)
	// The CPU backing received every page's bytes.
	for i := uint64(0); i < 15; i++ {
		data, err := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		if err != nil {
			t.Fatal(err)
		}
		for j := range data {
			if data[j] != byte(j%251) {
				t.Fatalf("CPU backing page %d byte %d = %d, want %d",
					i, j, data[j], byte(j%251))
			}
		}
	}
	deliverReplayAck(t, d, replay)
	mw.Tick()
	reqs = drainRequests(d)
	unblock := reqs[0].(*vm.UnblockRange)
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()
	if tx.phase != evictionStageDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
}

// TestUVMReactiveEvictionPinnedExclusion proves a pinned region is never
// selected as an eviction victim (§18.2), and that no eligible victim yields a
// deterministic error instead of a transaction.
func TestUVMReactiveEvictionPinnedExclusion(t *testing.T) {
	d, mw, _ := buildEvictionDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr)+64*mem.KB)
	key0 := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}
	key1 := copyRegionKey{PID: pid, GPU: 1, RegionBase: 64 * mem.KB}

	// The tie-break winner (region 0) is pinned: it is never selected.
	d.uvm.PinRegion(key0)
	if !d.uvm.IsPinned(key0) {
		t.Fatal("region 0 not pinned")
	}
	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	if mw.active.RegionBase != 64*mem.KB {
		t.Errorf("victim = %#x, want region 1 (pinned region 0 excluded)",
			mw.active.RegionBase)
	}

	// Both regions pinned -> no eligible victim.
	first := mw.active
	d.uvm.PinRegion(key1)
	if err := mw.intake(pid, 1); err == nil {
		t.Error("intake succeeded with every region pinned")
	}
	if mw.active != first {
		t.Error("a new transaction started with no eligible victim")
	}
}

// TestUVMReactiveEvictionCopyVictimExclusion proves a COPY-owned region is
// never an eviction victim (todo 5 ownership): the eviction selects the next
// eligible region and leaves the COPY ownership untouched.
func TestUVMReactiveEvictionCopyVictimExclusion(t *testing.T) {
	d, mw, _ := buildEvictionDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr)+64*mem.KB)
	key0 := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}

	// Region 0 (the tie-break winner) is COPY-owned: it is never a victim.
	if !d.uvm.AcquireOwnership(key0, OwnershipCopy, 777) {
		t.Fatal("could not claim region 0 as COPY")
	}
	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	if mw.active.RegionBase != 64*mem.KB {
		t.Errorf("victim = %#x, want region 1 (COPY-owned region 0 excluded)",
			mw.active.RegionBase)
	}
	if typ, owner := d.uvm.OwnerOf(key0); typ != OwnershipCopy || owner != 777 {
		t.Errorf("region 0 owner = %v/%d, want COPY/777 (untouched)",
			typ, owner)
	}
}

// TestUVMReactiveEvictionCopyEvictionNoCycle proves a waiting copy holds no
// victim/key while it waits, and the eviction's release/wakeup claims the copy
// after the unblock — release and wakeup cannot form a dependency cycle.
func TestUVMReactiveEvictionCopyEvictionNoCycle(t *testing.T) {
	d, mw, _ := buildEvictionDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 60*mem.KB)
	reg := d.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr))
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}

	// A copy waits for the region: it holds NO key while waiting.
	copyTx := &copyTransaction{
		Ticket:  d.uvm.NextCopyTicket(),
		PID:     pid,
		GPU:     1,
		StartVA: 0,
		Size:    64 * mem.KB,
		Keys:    []copyRegionKey{key},
		phase:   copyPhaseWaiting,
	}
	d.uvm.enqueueCopy(copyTx)
	if copyTx.claimed {
		t.Fatal("waiting copy claimed its keys")
	}
	if typ, _ := d.uvm.OwnerOf(key); typ != OwnershipIdle {
		t.Fatalf("key owned by %v while the copy waits", typ)
	}

	// The eviction runs to completion on the region the copy wants.
	if err := mw.intake(pid, 1); err != nil {
		t.Fatalf("intake: %v", err)
	}
	tx := mw.active
	driveEvictionToEnd(t, d, mw, pid)
	if tx.phase != evictionStageDone {
		t.Fatalf("eviction phase = %v, want done", tx.phase)
	}
	if region := reg.VABlocks[0].SubBlocks[0]; region.State != RegionCPUResident {
		t.Errorf("region state = %s, want CPU_RESIDENT", region.State)
	}

	// The eviction's release/wakeup claims the waiting copy: no cycle.
	if !copyTx.claimed {
		t.Fatal("waiting copy not claimed after the eviction wakeup")
	}
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipCopy || owner != copyTx.Ticket {
		t.Errorf("key owner = %v/%d, want COPY/%d", typ, owner, copyTx.Ticket)
	}
	if len(d.uvm.copyWaiters) != 0 {
		t.Errorf("copyWaiters = %d, want 0", len(d.uvm.copyWaiters))
	}
}

// TestUVMReactiveEvictionStageFailure injects a failure at every stage of the
// eviction transaction: the abort preserves the region-state authority (no
// illegal transition, the state equals the completed stages' output), prevents
// premature frame free and premature final-PTE publication, restores the
// capacity reservation, and releases the ownership slot and coalescing entry.
func TestUVMReactiveEvictionStageFailure(t *testing.T) {
	cases := []struct {
		name        string
		stage       evictionStage
		regionState RegionState
		pteLocation vm.MemoryLocation
		framesFreed bool
		residentR   uint64
		generation  uint64
	}{
		{"blocking", evictionStageBlocking, RegionEvictPending,
			vm.MemoryLocationGPU_LOCAL, false, 60 * mem.KB, 0},
		{"flushing", evictionStageFlushing, RegionEvictPending,
			vm.MemoryLocationGPU_LOCAL, false, 60 * mem.KB, 0},
		{"transitioning", evictionStageTransitioning, RegionEvictPending,
			vm.MemoryLocationGPU_LOCAL, false, 60 * mem.KB, 0},
		{"tlbi", evictionStageTLBI, RegionMigratingToCPU,
			vm.MemoryLocationINVALID, false, 60 * mem.KB, 1},
		{"d2h", evictionStageD2H, RegionMigratingToCPU,
			vm.MemoryLocationINVALID, false, 60 * mem.KB, 1},
		{"finalPTE", evictionStageFinalPTE, RegionMigratingToCPU,
			vm.MemoryLocationINVALID, false, 60 * mem.KB, 1},
		{"freeing", evictionStageFreeing, RegionMigratingToCPU,
			vm.MemoryLocationCPU_REMOTE, false, 60 * mem.KB, 1},
		{"replaying", evictionStageReplaying, RegionCPUResident,
			vm.MemoryLocationCPU_REMOTE, true, 0, 1},
		{"unblocking", evictionStageUnblocking, RegionCPUResident,
			vm.MemoryLocationCPU_REMOTE, true, 0, 1},
		{"done", evictionStageDone, RegionCPUResident,
			vm.MemoryLocationCPU_REMOTE, true, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// AC on: the final REMOTE PTE is distinguishable from the
			// transition INVALID PTE.
			d, mw, gpuTables := buildEvictionDriver(t, true)
			ctx := d.Init()
			pid := ctx.pid
			ptr := d.AllocateManagedMemory(ctx, 60*mem.KB)
			reg := d.uvm.registrations[0]
			makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr))
			region := reg.VABlocks[0].SubBlocks[0]
			key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}

			mw.failAfterStage = tc.stage
			if err := mw.intake(pid, 1); err != nil {
				t.Fatalf("intake: %v", err)
			}
			tx := mw.active
			driveEvictionToEnd(t, d, mw, pid)

			if tx.phase != evictionStageDone {
				t.Errorf("phase = %v, want done after the abort", tx.phase)
			}
			if mw.active != nil {
				t.Error("aborted transaction not retired")
			}
			if region.State != tc.regionState {
				t.Errorf("region state = %s, want %s", region.State,
					tc.regionState)
			}
			pte, found := gpuTables[0].Find(pid, reg.Base)
			if !found || pte.Location != tc.pteLocation {
				t.Errorf("PTE = %+v/%v, want %s", pte, found, tc.pteLocation)
			}
			ps := d.uvm.pageStateLocked(reg, 0)
			if tc.framesFreed {
				if ps.GPUPhysicalPage != 0 {
					t.Errorf("frame not freed: GPU PA = %#x",
						ps.GPUPhysicalPage)
				}
				if maskBit(reg.ResidentMask, 0) {
					t.Error("resident bit still set after the free")
				}
			} else {
				if ps.GPUPhysicalPage == 0 {
					t.Error("frame freed prematurely")
				}
				if !maskBit(reg.ResidentMask, 0) {
					t.Error("resident bit cleared prematurely")
				}
			}
			if got := d.uvm.Reservation().ResidentBytes(); got != tc.residentR {
				t.Errorf("resident R = %d, want %d", got, tc.residentR)
			}
			if got := d.uvm.Generation(); got != tc.generation {
				t.Errorf("generation = %d, want %d", got, tc.generation)
			}
			if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipIdle {
				t.Errorf("owner = %v/%d, want IDLE after the abort", typ, owner)
			}
			if d.uvm.evictByKey[key] != nil {
				t.Error("evictByKey entry not removed after the abort")
			}
		})
	}
}