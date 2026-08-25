package uvmtest

// sbin_codex: Scenario G — concurrent pre-eviction (uvm-manager.md §17.1,
// §21.4, §31). Usage reaches the configured capacity; the driver selects the
// oldest eligible 64 KB LRU victim, marks it EVICTING, issues the 64 KB cache
// WB+INV and the TLB invalidation, and begins the D2H pre-eviction while an
// independent H2D migration is already in progress: the transfers overlap and
// contend for the modeled DMA resources. Pre-eviction must not globally stall
// the active migration or unrelated GPU execution, and the freed capacity
// becomes available for future migrations.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// TestScenarioG locks the concurrent pre-eviction specification scenario:
// H2D and D2H overlap, no global stall, and the headroom is freed.
func TestScenarioG(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = false
	cfg.GPUMemoryCapacity = 192 * mem.KB
	cfg.CapacitySet = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)

	// Allocation 1: 128 KB at VA 4096. The fault on region 1 selects the
	// [0,256KB) TBN node: 32 pages (16 demand + 16 prefetch) migrate,
	// R = 128 KB — usage reaches the configured capacity.
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
	replay1Req, ok := replay1.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("fault-1 post-DMA request = %T, want replay (AC off: no TLB)",
			replay1)
	}
	deliverReplayAck(t, f.gpuPort, replay1Req)
	runEngineFlush(t, f)
	if snap := f.d.UVMStats(); snap.PeakResidentBytes != 124*mem.KB {
		t.Fatalf("R after fault 1 = %d, want 124KB", snap.PeakResidentBytes)
	}

	// Allocation 2: 64 KB at VA 139264. The fault selects the [0,256KB)
	// node: 14 demand + 2 prefetch = 64 KB. The admission fits (128 + 64 <=
	// 192) but the headroom is short: the H2D is launched IMMEDIATELY (no
	// admission wait — no global stall) and the deterministic LRU victim
	// (region 0, 60 KB) is launched concurrently.
	ptr2 := f.d.AllocateManagedMemory(ctx, 64*mem.KB)
	deliverFault(t, f, pid, uint64(ptr2))
	runEngineFlush(t, f)

	reqs := drainReqs(f.registeredPort)
	if len(reqs) != 2 {
		t.Fatalf("fault-2 service requests = %d, want 2 (H2D + victim block)",
			len(reqs))
	}
	h2d2, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("fault-2 request 0 = %T, want MemCopyH2DReq", reqs[0])
	}
	if uint64(len(h2d2.SrcBuffer)) != 64*mem.KB {
		t.Errorf("fault-2 H2D bytes = %d, want 64KB", len(h2d2.SrcBuffer))
	}
	block, ok := reqs[1].(*vm.BlockRange)
	if !ok {
		t.Fatalf("fault-2 request 1 = %T, want BlockRange", reqs[1])
	}
	if block.StartVA != 0 || block.Size != 64*mem.KB {
		t.Errorf("victim block = %#x+%d, want 0+64KB (region 0 LRU)",
			block.StartVA, block.Size)
	}
	if got := f.d.UVMStats().NumPreEvictionsOverlappedWithH2D; got != 1 {
		t.Errorf("pre-evictions overlapped with H2D = %d, want 1", got)
	}
	if got := f.d.UVMStats().MaxConcurrentPreEvictions; got != 1 {
		t.Errorf("max concurrent pre-evictions = %d, want 1", got)
	}

	// The victim is marked EVICTING: block -> WB+INV -> TLB -> D2H. The H2D
	// is still outstanding when the D2H begins — the transfers overlap.
	deliverGeneralRsp(t, f.gpuPort, block)
	runEngineFlush(t, f)
	flush := retrieveReq(t, f.registeredPort)
	flushReq, ok := flush.(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("victim request = %T, want UVMCacheRangeFlushReq", flush)
	}
	if flushReq.ValidPageMask != 0xFFFE {
		t.Errorf("victim flush mask = %#x, want 0xFFFE", flushReq.ValidPageMask)
	}
	deliverFlushRsp(t, f.gpuPort, flushReq)
	runEngineFlush(t, f)
	tlb := retrieveReq(t, f.registeredPort)
	tlbReq, ok := tlb.(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("victim request = %T, want UVMTLBInvalidateReq", tlb)
	}
	deliverTLBAck(t, f.gpuPort, tlbReq)
	runEngineFlush(t, f)
	d2h := retrieveReq(t, f.registeredPort)
	d2hReq, ok := d2h.(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("victim request = %T, want MemCopyD2HReq", d2h)
	}
	if uint64(len(d2hReq.DstBuffer)) != 60*mem.KB {
		t.Errorf("victim D2H bytes = %d, want 60KB", len(d2hReq.DstBuffer))
	}
	// Both transfers are in flight: the H2D has not completed.
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("unexpected requests while H2D+D2H overlap: %d", len(reqs))
	}

	// The H2D completes independently (no stall from the pre-eviction).
	deliverGeneralRsp(t, f.gpuPort, h2d2)
	runEngineFlush(t, f)
	replay2 := retrieveReq(t, f.registeredPort)
	replay2Req, ok := replay2.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("fault-2 post-DMA request = %T, want replay", replay2)
	}
	deliverReplayAck(t, f.gpuPort, replay2Req)
	runEngineFlush(t, f)

	// The pre-eviction victim completes its D2H: the frames are freed, the
	// final INVALID mapping (AC off) is installed, and the range unblocks.
	deliverGeneralRsp(t, f.gpuPort, d2hReq)
	runEngineFlush(t, f)
	victimReplay := retrieveReq(t, f.registeredPort)
	victimReplayReq, ok := victimReplay.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("victim post-D2H request = %T, want UVMFaultReplayReq",
			victimReplay)
	}
	deliverReplayAck(t, f.gpuPort, victimReplayReq)
	runEngineFlush(t, f)
	unblock := retrieveReq(t, f.registeredPort)
	unblockReq, ok := unblock.(*vm.UnblockRange)
	if !ok {
		t.Fatalf("victim post-replay request = %T, want UnblockRange", unblock)
	}
	deliverGeneralRsp(t, f.gpuPort, unblockReq)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("post-completion requests = %d, want 0", len(reqs))
	}

	// Final residency: the victim region is INVALID (AC off), the migrated
	// regions are GPU_LOCAL.
	for va := uint64(ptr1); va < uint64(ptr1)+60*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if page.Valid || page.Location != vm.MemoryLocationINVALID {
			t.Errorf("victim PTE at %#x = %+v, want INVALID", va, page)
		}
	}
	for va := uint64(ptr1) + 64*mem.KB; va < uint64(ptr1)+124*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if !page.Valid || page.Location != vm.MemoryLocationGPU_LOCAL {
			t.Errorf("resident PTE at %#x = %+v, want GPU_LOCAL", va, page)
		}
	}
	for va := uint64(ptr2); va < uint64(ptr2)+64*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if !page.Valid || page.Location != vm.MemoryLocationGPU_LOCAL {
			t.Errorf("incoming PTE at %#x = %+v, want GPU_LOCAL", va, page)
		}
	}

	// Exact counters: the pre-eviction freed the headroom (R dropped by the
	// victim's 60 KB) and no global stall occurred (the fault never waited
	// for capacity).
	snap := f.d.UVMStats()
	if snap.NumPreEvictions != 1 || snap.BytesPreEvicted != 60*mem.KB {
		t.Errorf("pre-evictions = %d/%d bytes, want 1/60KB",
			snap.NumPreEvictions, snap.BytesPreEvicted)
	}
	if snap.NumEvictions != 1 || snap.BytesEvicted != 60*mem.KB {
		t.Errorf("evictions = %d/%d bytes, want 1/60KB",
			snap.NumEvictions, snap.BytesEvicted)
	}
	if snap.NumGPUToCPUMigrations != 1 || snap.BytesGPUToCPU != 60*mem.KB {
		t.Errorf("D2H = %d migrations/%d bytes, want 1/60KB",
			snap.NumGPUToCPUMigrations, snap.BytesGPUToCPU)
	}
	if snap.NumCPUToGPUMigrations != 2 || snap.BytesCPUToGPU != 188*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 2/188KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumUVMTLBRangeInvalidations != 1 {
		t.Errorf("TLB range invalidations = %d, want 1 (eviction only)",
			snap.NumUVMTLBRangeInvalidations)
	}
	if snap.NumLocalPTEInstalls != 47 {
		t.Errorf("local PTE installs = %d, want 47", snap.NumLocalPTEInstalls)
	}
	if snap.MigrationWaitCyclesForCapacity != 0 {
		t.Errorf("capacity wait cycles = %d, want 0 (no global stall)",
			snap.MigrationWaitCyclesForCapacity)
	}
	// The freed 60 KB became available: the resident bytes dropped by the
	// victim's logical bytes (128 KB = 124 + 64 - 60), and the peak resident
	// reflects the post-eviction admission commit (124 KB then 128 KB).
	if snap.PeakResidentBytes != 128*mem.KB {
		t.Errorf("peak resident = %d, want 128KB", snap.PeakResidentBytes)
	}
}
