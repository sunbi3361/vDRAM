package uvmtest

// sbin_codex: Scenario E — oversubscription (uvm-manager.md §17, §18, §19,
// §21.4). Access Counter disabled, capacity 128 KB. A demand migration
// requests new GPU capacity that does not fit: the driver selects the
// least-recently-used GPU-resident region, blocks it, issues the 64 KB cache
// WB+INV and the TLB range invalidation, migrates the victim GPU -> CPU,
// releases the GPU physical pages, installs the final INVALID mapping (AC
// off), and only then admits the incoming migration and installs the new
// local mapping.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// TestScenarioE locks the oversubscription specification scenario: capacity
// insufficient, LRU victim, block, WB+INV + TLB invalidate, D2H, frame
// release, final INVALID mapping (AC off), incoming migration, and the new
// local mapping.
func TestScenarioE(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = false
	cfg.GPUMemoryCapacity = 128 * mem.KB
	cfg.CapacitySet = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)

	// Allocation 1: 128 KB at VA 4096 (32 valid pages: 15 in region 0, 16 in
	// region 1, 1 in region 2). The fault on region 1 selects the [0,256KB)
	// TBN node (16/31 > 51% expands, 16/32 <= 51% stops): 32 pages (16
	// demand + 16 prefetch) migrate, R = 128 KB = the capacity.
	ptr1 := f.d.AllocateManagedMemory(ctx, 128*mem.KB)
	deliverFault(t, f, pid, uint64(ptr1)+64*mem.KB)
	runEngineFlush(t, f)
	h2d1 := retrieveReq(t, f.registeredPort)
	h2d1Req, ok := h2d1.(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("fault-1 service request = %T, want MemCopyH2DReq", h2d1)
	}
	if uint64(len(h2d1Req.SrcBuffer)) != 124*mem.KB {
		t.Errorf("fault-1 H2D bytes = %d, want 124KB", len(h2d1Req.SrcBuffer))
	}
	deliverGeneralRsp(t, f.gpuPort, h2d1Req)
	runEngineFlush(t, f)
	replay1 := retrieveReq(t, f.registeredPort)
	if _, ok := replay1.(*protocol.UVMFaultReplayReq); !ok {
		t.Fatalf("fault-1 post-DMA request = %T, want replay (AC off: no TLB)",
			replay1)
	}
	deliverReplayAck(t, f.gpuPort, replay1.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)
	if snap := f.d.UVMStats(); snap.PeakResidentBytes != 124*mem.KB {
		t.Fatalf("R after fault 1 = %d, want 124KB", snap.PeakResidentBytes)
	}

	// Allocation 2: 8 KB at VA 135168 (region 2: 2 valid pages). The fault
	// selects the [0,256KB) node: 2 demand pages (all other selected pages
	// are resident and suppressed) = 8 KB. Capacity 128 KB with R = 128 KB
	// cannot fit 8 KB: hard shortage — the admission queues and the LRU
	// victim (region 0, 60 KB) is launched.
	ptr2 := f.d.AllocateManagedMemory(ctx, 8*mem.KB)
	deliverFault(t, f, pid, uint64(ptr2))
	runEngineFlush(t, f)

	// The victim's block barrier is the first request of the eviction.
	block0 := retrieveReq(t, f.registeredPort)
	block0Req, ok := block0.(*vm.BlockRange)
	if !ok {
		t.Fatalf("eviction request = %T, want BlockRange", block0)
	}
	if block0Req.StartVA != 0 || block0Req.Size != 64*mem.KB {
		t.Errorf("victim block range = %#x+%d, want 0+64KB (region 0 LRU)",
			block0Req.StartVA, block0Req.Size)
	}
	if got := f.d.UVMStats().MigrationWaitCyclesForCapacity; got < 1 {
		t.Errorf("capacity wait cycles = %d, want >= 1", got)
	}

	// WB+INV over the victim's 15 valid pages (mask bits 1-15 = 0xFFFE).
	deliverGeneralRsp(t, f.gpuPort, block0Req)
	runEngineFlush(t, f)
	flush0 := retrieveReq(t, f.registeredPort)
	flush0Req, ok := flush0.(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("post-block request = %T, want UVMCacheRangeFlushReq", flush0)
	}
	if flush0Req.Operation != cache.UVMCacheRangeFlushWritebackInvalidate {
		t.Errorf("flush operation = %v, want WB+INV", flush0Req.Operation)
	}
	if flush0Req.VABase != 0 {
		t.Errorf("flush VA base = %#x, want 0", flush0Req.VABase)
	}
	if flush0Req.ValidPageMask != 0xFFFE {
		t.Errorf("flush mask = %#x, want 0xFFFE (15 valid pages)", flush0Req.ValidPageMask)
	}
	deliverFlushRsp(t, f.gpuPort, flush0Req)
	runEngineFlush(t, f)

	// The 64 KB TLB range invalidation (GPU_LOCAL -> INVALID REQUIRES it).
	tlb0 := retrieveReq(t, f.registeredPort)
	tlb0Req, ok := tlb0.(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-flush request = %T, want UVMTLBInvalidateReq", tlb0)
	}
	deliverTLBAck(t, f.gpuPort, tlb0Req)
	runEngineFlush(t, f)

	// D2H of every valid victim page (60 KB, one maximal run).
	d2h0 := retrieveReq(t, f.registeredPort)
	d2h0Req, ok := d2h0.(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want MemCopyD2HReq", d2h0)
	}
	if uint64(len(d2h0Req.DstBuffer)) != 60*mem.KB {
		t.Errorf("victim D2H bytes = %d, want 60KB", len(d2h0Req.DstBuffer))
	}
	for j := range d2h0Req.DstBuffer {
		d2h0Req.DstBuffer[j] = byte(j % 251)
	}
	deliverGeneralRsp(t, f.gpuPort, d2h0Req)
	runEngineFlush(t, f)

	// The victim's replay completes; the frames were already freed, so the
	// retry now fits (R = 64 KB, 12 KB admission): the H2D is emitted in the
	// same tick as the victim's UnblockRange, and the headroom trigger
	// launches the next LRU victim (region 1, 64 KB) concurrently.
	replay0 := retrieveReq(t, f.registeredPort)
	replay0Req, ok := replay0.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-D2H request = %T, want UVMFaultReplayReq", replay0)
	}
	deliverReplayAck(t, f.gpuPort, replay0Req)
	runEngineFlush(t, f)
	reqs := drainReqs(f.registeredPort)
	if len(reqs) != 2 {
		t.Fatalf("post-replay requests = %d, want 2 (H2D + victim unblock)",
			len(reqs))
	}
	h2d2, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("post-replay request 0 = %T, want MemCopyH2DReq", reqs[0])
	}
	if uint64(len(h2d2.SrcBuffer)) != 8*mem.KB {
		t.Errorf("incoming H2D bytes = %d, want 8KB", len(h2d2.SrcBuffer))
	}
	unblock0, ok := reqs[1].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request 1 = %T, want UnblockRange", reqs[1])
	}
	deliverGeneralRsp(t, f.gpuPort, unblock0)
	runEngineFlush(t, f)

	// The victim's GPU frames were released: R dropped by 60 KB, and the
	// CPU backing received the D2H bytes (the D2H buffer is filled with the
	// global byte index, so each page's local check uses its global offset).
	if snap := f.d.UVMStats(); snap.BytesGPUToCPU != 60*mem.KB {
		t.Errorf("bytes GPU->CPU = %d, want 60KB", snap.BytesGPUToCPU)
	}
	pageIdx := uint64(0)
	for va := uint64(ptr1); va < uint64(ptr1)+60*mem.KB; va += 4096 {
		cpuPA := cpuBackingPA(t, f, pid, va)
		data, err := f.storage.Read(cpuPA, 4096)
		if err != nil {
			t.Fatal(err)
		}
		for j := range data {
			want := byte((pageIdx*4096 + uint64(j)) % 251)
			if data[j] != want {
				t.Fatalf("CPU backing page %#x byte %d = %d, want %d",
					va, j, data[j], want)
			}
		}
		pageIdx++
	}

	// The victim-1 block barrier opens the second eviction.
	block1 := retrieveReq(t, f.registeredPort)
	block1Req, ok := block1.(*vm.BlockRange)
	if !ok {
		t.Fatalf("second victim request = %T, want BlockRange", block1)
	}
	if block1Req.StartVA != 64*mem.KB || block1Req.Size != 64*mem.KB {
		t.Errorf("second victim block = %#x+%d, want 64KB+64KB (region 1)",
			block1Req.StartVA, block1Req.Size)
	}

	// Drive the second victim's eviction to completion.
	deliverGeneralRsp(t, f.gpuPort, block1Req)
	runEngineFlush(t, f)
	flush1 := retrieveReq(t, f.registeredPort)
	flush1Req, ok := flush1.(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("second victim request = %T, want UVMCacheRangeFlushReq", flush1)
	}
	if flush1Req.ValidPageMask != 0xFFFF {
		t.Errorf("second flush mask = %#x, want 0xFFFF (16 valid pages)",
			flush1Req.ValidPageMask)
	}
	deliverFlushRsp(t, f.gpuPort, flush1Req)
	runEngineFlush(t, f)
	tlb1 := retrieveReq(t, f.registeredPort)
	tlb1Req, ok := tlb1.(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("second victim request = %T, want UVMTLBInvalidateReq", tlb1)
	}
	deliverTLBAck(t, f.gpuPort, tlb1Req)
	runEngineFlush(t, f)
	d2h1 := retrieveReq(t, f.registeredPort)
	d2h1Req, ok := d2h1.(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("second victim request = %T, want MemCopyD2HReq", d2h1)
	}
	if uint64(len(d2h1Req.DstBuffer)) != 64*mem.KB {
		t.Errorf("second victim D2H bytes = %d, want 64KB",
			len(d2h1Req.DstBuffer))
	}
	deliverGeneralRsp(t, f.gpuPort, d2h1Req)
	runEngineFlush(t, f)
	replay1b := retrieveReq(t, f.registeredPort)
	replay1bReq, ok := replay1b.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("second victim request = %T, want UVMFaultReplayReq", replay1b)
	}
	deliverReplayAck(t, f.gpuPort, replay1bReq)
	runEngineFlush(t, f)
	unblock1 := retrieveReq(t, f.registeredPort)
	unblock1Req, ok := unblock1.(*vm.UnblockRange)
	if !ok {
		t.Fatalf("second victim request = %T, want UnblockRange", unblock1)
	}
	deliverGeneralRsp(t, f.gpuPort, unblock1Req)
	runEngineFlush(t, f)

	// The incoming migration completes: the H2D ack publishes the new local
	// mapping (AC off: replay directly, no TLB).
	deliverGeneralRsp(t, f.gpuPort, h2d2)
	runEngineFlush(t, f)
	replay2 := retrieveReq(t, f.registeredPort)
	replay2Req, ok := replay2.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("incoming post-DMA request = %T, want replay (AC off: no TLB)",
			replay2)
	}
	deliverReplayAck(t, f.gpuPort, replay2Req)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("post-completion requests = %d, want 0", len(reqs))
	}

	// Final residency: the evicted regions (0 and 1) and the never-migrated
	// page 31 are INVALID (AC off); the incoming region 2 pages are
	// GPU_LOCAL.
	for va := uint64(ptr1); va < uint64(ptr1)+124*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if page.Valid || page.Location != vm.MemoryLocationINVALID {
			t.Errorf("evicted PTE at %#x = %+v, want INVALID", va, page)
		}
	}
	for va := uint64(ptr2); va < uint64(ptr2)+8*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if !page.Valid || page.Location != vm.MemoryLocationGPU_LOCAL ||
			page.PAddr == 0 {
			t.Errorf("incoming PTE at %#x = %+v, want GPU_LOCAL", va, page)
		}
	}
	// Page 31 (VA 131072) was never migrated (the fault-1 selection stopped
	// at the [0,128KB) node and the fault-2 selection is per-registration):
	// it stays INVALID.
	page31 := pteOf(t, f.gpuTables[0], pid, uint64(ptr1)+124*mem.KB)
	if page31.Valid || page31.Location != vm.MemoryLocationINVALID {
		t.Errorf("never-migrated PTE at %#x = %+v, want INVALID",
			uint64(ptr1)+124*mem.KB, page31)
	}

	// Exact counters.
	snap := f.d.UVMStats()
	if snap.NumEvictions != 2 || snap.BytesEvicted != 124*mem.KB ||
		snap.NumDirtyEvictions != 0 {
		t.Errorf("evictions = %d/%d bytes/%d dirty, want 2/124KB/0",
			snap.NumEvictions, snap.BytesEvicted, snap.NumDirtyEvictions)
	}
	if snap.NumGPUToCPUMigrations != 2 || snap.BytesGPUToCPU != 124*mem.KB {
		t.Errorf("D2H = %d migrations/%d bytes, want 2/124KB",
			snap.NumGPUToCPUMigrations, snap.BytesGPUToCPU)
	}
	if snap.NumCPUToGPUMigrations != 2 || snap.BytesCPUToGPU != 132*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 2/132KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumUVMTLBRangeInvalidations != 2 {
		t.Errorf("TLB range invalidations = %d, want 2 (evictions only; "+
			"AC-off faults need none)", snap.NumUVMTLBRangeInvalidations)
	}
	if snap.NumRemotePTEInstalls != 0 {
		t.Errorf("remote PTE installs = %d, want 0 (AC off -> INVALID)",
			snap.NumRemotePTEInstalls)
	}
	if snap.NumLocalPTEInstalls != 33 {
		t.Errorf("local PTE installs = %d, want 33", snap.NumLocalPTEInstalls)
	}
	if snap.NumPreEvictions != 2 || snap.BytesPreEvicted != 124*mem.KB {
		t.Errorf("pre-evictions = %d/%d bytes, want 2/124KB",
			snap.NumPreEvictions, snap.BytesPreEvicted)
	}
	if snap.PeakResidentBytes != 124*mem.KB || snap.CapacityBytes != 128*mem.KB {
		t.Errorf("oversubscription = %d/%d, want 124KB/128KB",
			snap.PeakResidentBytes, snap.CapacityBytes)
	}
	if snap.NumGPUPageFaultRequests != 2 || snap.NumUniqueFaultServices != 2 {
		t.Errorf("fault stats = %d/%d, want 2/2",
			snap.NumGPUPageFaultRequests, snap.NumUniqueFaultServices)
	}
}
