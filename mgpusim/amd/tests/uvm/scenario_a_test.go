package uvmtest

// sbin_codex: Scenario A — demand read fault (uvm-manager.md §8, §21.2).
// Access Counter disabled: the GPU PTE is INVALID, so the fault migration is
// INVALID -> GPU_LOCAL and per §21.2/§21.5 NO TLB invalidation is issued
// (invalid/non-resident translations are not cached in the TLB hierarchy).
// The scenario text's step 12 ("Driver requests TLB range invalidation") is
// STALE and must be asserted as zero TLB invalidates (normative precedence,
// plan todo 25). The required path is DMA H2D -> PTE install -> Fault
// Replay, with no data-cache flush and no GPU-wide control.

import (
	"bytes"
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// TestScenarioA locks the demand-fault specification scenario: event order,
// final residency/PTE/TLB/cache/data, exact migration bytes and counters,
// elapsed-time constraints, and the absence of prohibited global controls.
// Step 12 of the scenario text is stale: the test asserts ZERO TLB
// invalidates for the INVALID -> GPU_LOCAL transition (§21.2, §21.5, §31).
func TestScenarioA(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = false
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)
	if uint64(ptr) != 4096 {
		t.Fatalf("allocation base = %#x, want 4096", uint64(ptr))
	}

	// The allocation covers 16 valid 4 KB pages (VA 4096..69632): 15 in
	// region 0, 1 (VA 65536) in region 1. Pre-fill the CPU backing with a
	// known pattern so the H2D payload is observable.
	firstCPU := cpuBackingPA(t, f, pid, uint64(ptr))
	pattern := make([]byte, 64*mem.KB)
	for i := range pattern {
		pattern[i] = byte(i*7 + 3)
	}
	if err := f.storage.Write(firstCPU, pattern); err != nil {
		t.Fatalf("storage.Write: %v", err)
	}

	// The initial GPU PTE is INVALID (Access Counter disabled).
	page := pteOf(t, f.gpuTables[0], pid, uint64(ptr))
	if page.Valid || page.Location != vm.MemoryLocationINVALID {
		t.Errorf("initial GPU PTE = %+v, want INVALID", page)
	}

	// Step 1-7: the GPU read faults; the driver coalesces and services the
	// 64 KB region (TBN selects the 2 MB root: 15 demand + 1 prefetch page).
	deliverFault(t, f, pid, uint64(ptr))
	runEngineFlush(t, f)

	reqs := drainReqs(f.registeredPort)
	if len(reqs) != 1 {
		t.Fatalf("service requests = %d, want 1 H2D", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("service request = %T, want MemCopyH2DReq", reqs[0])
	}
	if !bytes.Equal(h2d.SrcBuffer, pattern) {
		t.Error("H2D payload does not match the CPU backing content")
	}
	if uint64(len(h2d.SrcBuffer)) != 64*mem.KB {
		t.Errorf("H2D bytes = %d, want 64KB", len(h2d.SrcBuffer))
	}
	if h2d.DstAddress == 0 {
		t.Error("H2D destination is not a GPU frame")
	}

	// Step 10-11: the DMA completes; the PTEs become GPU_LOCAL. Per §21.2
	// the next step is the fault replay — NOT a TLB invalidation (the
	// scenario text's step 12 is stale).
	deliverGeneralRsp(t, f.gpuPort, h2d)
	runEngineFlush(t, f)

	reqs = drainReqs(f.registeredPort)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 replay (no TLB)", len(reqs))
	}
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMFaultReplayReq "+
			"(INVALID -> GPU_LOCAL needs no TLB invalidation)", reqs[0])
	}
	if replay.StartVA != 0 || replay.Size != 64*mem.KB {
		t.Errorf("replay range = %#x+%d, want 0+64KB",
			replay.StartVA, replay.Size)
	}

	// Step 13-15: the fault completion reaches the GPU and the access is
	// replayed; the read completes from GPU memory.
	deliverReplayAck(t, f.gpuPort, replay)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("post-replay requests = %d, want 0", len(reqs))
	}

	// Final residency: every valid page is GPU_LOCAL with a GPU frame.
	for va := uint64(ptr); va < uint64(ptr)+64*mem.KB; va += 4096 {
		page := pteOf(t, f.gpuTables[0], pid, va)
		if !page.Valid || page.Location != vm.MemoryLocationGPU_LOCAL ||
			page.PAddr == 0 {
			t.Errorf("final GPU PTE at %#x = %+v, want GPU_LOCAL", va, page)
		}
	}
	// The CPU table keeps the authoritative backing truth.
	truth := pteOf(t, f.pageTable, pid, uint64(ptr))
	if !truth.Valid || truth.Location != vm.MemoryLocationCPU_REMOTE {
		t.Errorf("CPU truth PTE = %+v, want CPU_REMOTE", truth)
	}

	// TLB/cache: ZERO TLB invalidations and ZERO range flushes.
	snap := f.d.UVMStats()
	if snap.NumUVMTLBRangeInvalidations != 0 {
		t.Errorf("TLB range invalidations = %d, want 0 "+
			"(INVALID -> GPU_LOCAL needs none, §21.2)", snap.NumUVMTLBRangeInvalidations)
	}

	// Exact migration bytes and counters.
	if snap.NumGPUPageFaultRequests != 1 || snap.NumUniqueFaultServices != 1 ||
		snap.NumCoalescedFaults != 0 {
		t.Errorf("fault stats = %d/%d/%d, want 1/1/0",
			snap.NumGPUPageFaultRequests, snap.NumUniqueFaultServices,
			snap.NumCoalescedFaults)
	}
	if snap.NumCPUToGPUMigrations != 1 || snap.BytesCPUToGPU != 64*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 1/64KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumDemandMigrations != 1 || snap.BytesDemandMigrated != 60*mem.KB {
		t.Errorf("demand = %d migrations/%d bytes, want 1/60KB",
			snap.NumDemandMigrations, snap.BytesDemandMigrated)
	}
	if snap.NumPrefetchMigrations != 1 || snap.BytesPrefetched != 4*mem.KB {
		t.Errorf("prefetch = %d migrations/%d bytes, want 1/4KB",
			snap.NumPrefetchMigrations, snap.BytesPrefetched)
	}
	if snap.NumLocalPTEInstalls != 16 {
		t.Errorf("local PTE installs = %d, want 16", snap.NumLocalPTEInstalls)
	}
	if snap.PeakResidentBytes != 64*mem.KB {
		t.Errorf("peak resident = %d, want 64KB", snap.PeakResidentBytes)
	}

	// Elapsed-time constraints: the fixed 20 us fault-handling delay is
	// charged exactly once and the engine advanced past it.
	wantLatency := latencySeconds(20 * time.Microsecond)
	if snap.FaultServiceLatencyTotal != wantLatency ||
		snap.FaultServiceLatencyAvg != wantLatency {
		t.Errorf("fault latency = %v total/%v avg, want %v/%v",
			snap.FaultServiceLatencyTotal, snap.FaultServiceLatencyAvg,
			wantLatency, wantLatency)
	}
	if f.engine.CurrentTime() < wantLatency {
		t.Errorf("engine time = %v, want >= %v", f.engine.CurrentTime(),
			wantLatency)
	}

	// Absence of prohibited global controls: the whole stream is only the
	// H2D and the replay — no shootdown/restart/drain, no full flush, no
	// range flush, no TLB invalidate, no block/unblock.
	assertOnlyTypes(t, []sim.Msg{h2d, replay},
		"*protocol.MemCopyH2DReq", "*protocol.UVMFaultReplayReq")
}
