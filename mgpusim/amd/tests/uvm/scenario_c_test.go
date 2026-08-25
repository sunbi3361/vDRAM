package uvmtest

// sbin_codex: Scenario C — remote read reaches the threshold (uvm-manager.md
// §14, §16, §21.3). Access Counter enabled, threshold 8. The counter
// notifies the CP at the crossing; the driver checks capacity and migrates
// the 64 KB region CPU -> GPU; the PTEs change REMOTE -> GPU_LOCAL and the
// mandatory 64 KB TLB range invalidation removes the cached REMOTE
// translation; future accesses use GPU-local memory.

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// TestScenarioC locks the threshold-triggered migration: notification,
// capacity check, migration, PTE REMOTE -> GPU_LOCAL, TLB range
// invalidation (REQUIRED for REMOTE -> GPU_LOCAL), and GPU-local future
// accesses.
func TestScenarioC(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)

	// Pre-fill the CPU backing so the H2D payload is observable.
	firstCPU := cpuBackingPA(t, f, pid, uint64(ptr))
	pattern := make([]byte, 60*mem.KB)
	for i := range pattern {
		pattern[i] = byte(i*11 + 5)
	}
	if err := f.storage.Write(firstCPU, pattern); err != nil {
		t.Fatalf("storage.Write: %v", err)
	}

	counter, _, _ := buildCounterAndEndpoint(t, engine, 8)

	// Step 1-4: eight remote reads increment the region counter; the eighth
	// reaches the threshold and the GPU Access Counter emits the immediate
	// equality notification to the CP.
	for i := 0; i < 8; i++ {
		counter.RecordRemoteAccess(pid, 1, uint64(ptr))
	}
	if got := counter.Count(pid, 1, 0); got != 8 {
		t.Fatalf("counter = %d, want 8", got)
	}
	counter.Tick()

	notif := retrieveReq(t, counter.ToCP)
	notification, ok := notif.(*protocol.AccessCounterNotification)
	if !ok {
		t.Fatalf("counter output = %T, want AccessCounterNotification", notif)
	}
	if notification.PID != pid || notification.GPU != 1 ||
		notification.VAddr != 0 || notification.AccessCount != 8 {
		t.Errorf("notification = %+v, want pid=%d gpu=1 va=0 count=8",
			notification, pid)
	}

	// Step 5-7: the driver checks capacity and migrates the region; the
	// migration blocks the range first (block-barrier before any DMA).
	deliverNotification(t, f, pid, 0, 8)
	runEngineFlush(t, f)

	block := retrieveReq(t, f.registeredPort)
	blockReq, ok := block.(*vm.BlockRange)
	if !ok {
		t.Fatalf("migration request = %T, want BlockRange", block)
	}
	if blockReq.StartVA != 0 || blockReq.Size != 64*mem.KB {
		t.Errorf("block range = %#x+%d, want 0+64KB",
			blockReq.StartVA, blockReq.Size)
	}
	deliverGeneralRsp(t, f.gpuPort, blockReq)
	runEngineFlush(t, f)

	h2d := retrieveReq(t, f.registeredPort)
	h2dReq, ok := h2d.(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("post-block request = %T, want MemCopyH2DReq", h2d)
	}
	if !bytes.Equal(h2dReq.SrcBuffer, pattern) {
		t.Error("H2D payload does not match the CPU backing content")
	}
	if uint64(len(h2dReq.SrcBuffer)) != 60*mem.KB {
		t.Errorf("H2D bytes = %d, want 60KB", len(h2dReq.SrcBuffer))
	}

	// Step 8-9: the DMA completes, the PTEs become GPU_LOCAL, and the
	// mandatory 64 KB TLB invalidation removes the cached REMOTE translation
	// (§21.3: the previous REMOTE translation may be cached in the L2 TLB).
	deliverGeneralRsp(t, f.gpuPort, h2dReq)
	runEngineFlush(t, f)

	tlb := retrieveReq(t, f.registeredPort)
	tlbReq, ok := tlb.(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq "+
			"(REMOTE -> GPU_LOCAL REQUIRES it)", tlb)
	}
	if tlbReq.StartVA != 0 || tlbReq.Size != 64*mem.KB {
		t.Errorf("TLB range = %#x+%d, want 0+64KB",
			tlbReq.StartVA, tlbReq.Size)
	}
	deliverTLBAck(t, f.gpuPort, tlbReq)
	runEngineFlush(t, f)

	// Step 10: the blocked accesses are released/replayed.
	replay := retrieveReq(t, f.registeredPort)
	replayReq, ok := replay.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", replay)
	}
	deliverReplayAck(t, f.gpuPort, replayReq)
	runEngineFlush(t, f)

	unblock := retrieveReq(t, f.registeredPort)
	unblockReq, ok := unblock.(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request = %T, want UnblockRange", unblock)
	}
	deliverGeneralRsp(t, f.gpuPort, unblockReq)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("post-unblock requests = %d, want 0", len(reqs))
	}

	// Final residency: the 15 valid pages of region 0 are GPU_LOCAL.
	for va := uint64(ptr); va < uint64(ptr)+60*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if !page.Valid || page.Location != vm.MemoryLocationGPU_LOCAL ||
			page.PAddr == 0 {
			t.Errorf("final GPU PTE at %#x = %+v, want GPU_LOCAL", va, page)
		}
	}

	// Exact counters.
	snap := f.d.UVMStats()
	if snap.NumAccessCounterIncrements != 8 ||
		snap.NumAccessCounterNotifications != 1 ||
		snap.NumAccessCounterThresholdHits != 1 {
		t.Errorf("access counter stats = %d/%d/%d, want 8/1/1",
			snap.NumAccessCounterIncrements, snap.NumAccessCounterNotifications,
			snap.NumAccessCounterThresholdHits)
	}
	if snap.NumRemoteReads != 8 || snap.BytesRemoteRead != 32*mem.KB ||
		snap.PCIeRemoteReadTransactions != 8 {
		t.Errorf("remote read stats = %d/%d/%d, want 8/32KB/8",
			snap.NumRemoteReads, snap.BytesRemoteRead,
			snap.PCIeRemoteReadTransactions)
	}
	if snap.NumAccessCounterMigrations != 1 ||
		snap.BytesAccessCounterMigrated != 60*mem.KB {
		t.Errorf("AC migration = %d/%d, want 1/60KB",
			snap.NumAccessCounterMigrations, snap.BytesAccessCounterMigrated)
	}
	if snap.NumCPUToGPUMigrations != 1 || snap.BytesCPUToGPU != 60*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 1/60KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumUVMTLBRangeInvalidations != 1 {
		t.Errorf("TLB range invalidations = %d, want 1 "+
			"(REMOTE -> GPU_LOCAL REQUIRES it)", snap.NumUVMTLBRangeInvalidations)
	}
	if snap.NumLocalPTEInstalls != 15 {
		t.Errorf("local PTE installs = %d, want 15", snap.NumLocalPTEInstalls)
	}

	// Future accesses are GPU-local: the published translation is GPU_LOCAL.
	page := pteOf(t, f.gpuTables[0], pid, uint64(ptr))
	if page.Location != vm.MemoryLocationGPU_LOCAL {
		t.Errorf("future access translation = %s, want GPU_LOCAL",
			page.Location)
	}
}
