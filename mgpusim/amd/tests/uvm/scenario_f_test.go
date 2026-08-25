package uvmtest

// sbin_codex: Scenario F — ideal UVM (uvm-manager.md §25, §31). Ideal mode
// changes TIME, not the sequence of functional UVM decisions: the same state
// machine runs, duplicate faults coalesce normally, TBN selects the same
// region, oversubscription/LRU decisions run normally, migration byte
// counters update normally, PTE/TLB residency updates normally, and the
// faulting request is replayed — only the fixed 20 us fault-handling delay
// and the DMA transfer delay are zero.

import (
	"reflect"
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// parityTrace runs the predeclared trace (one demand fault + one
// threshold-triggered migration) and returns the ordered request stream and
// the final stats snapshot.
func parityTrace(t *testing.T, ideal bool) ([]string, driver.UVMStatsSnapshot) {
	t.Helper()

	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	cfg.Ideal = ideal
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)

	// Demand fault on the 64 KB allocation (regions 0+1): TBN selects the
	// 2 MB root, 16 pages (15 demand + 1 prefetch) migrate REMOTE ->
	// GPU_LOCAL with the mandatory TLB invalidation.
	ptr1 := f.d.AllocateManagedMemory(ctx, 64*mem.KB)
	deliverFault(t, f, pid, uint64(ptr1))
	runEngineFlush(t, f)
	h2d := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, h2d)
	runEngineFlush(t, f)
	tlb := retrieveReq(t, f.registeredPort)
	deliverTLBAck(t, f.gpuPort, tlb.(*protocol.UVMTLBInvalidateReq))
	runEngineFlush(t, f)
	replay := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, replay.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)

	// Threshold-triggered migration of the 64 KB allocation's region 2 page
	// (VA 131072, the allocation's last page): the notification carries the
	// 64 KB-aligned region base.
	f.d.AllocateManagedMemory(ctx, 64*mem.KB)
	deliverNotification(t, f, pid, 128*mem.KB, 8)
	runEngineFlush(t, f)
	block := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, block)
	runEngineFlush(t, f)
	h2d2 := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, h2d2)
	runEngineFlush(t, f)
	tlb2 := retrieveReq(t, f.registeredPort)
	deliverTLBAck(t, f.gpuPort, tlb2.(*protocol.UVMTLBInvalidateReq))
	runEngineFlush(t, f)
	replay2 := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, replay2.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)
	unblock := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, unblock)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Fatalf("post-trace requests = %d, want 0", len(reqs))
	}

	return []string{
		reflect.TypeOf(h2d).String(),
		reflect.TypeOf(tlb).String(),
		reflect.TypeOf(replay).String(),
		reflect.TypeOf(block).String(),
		reflect.TypeOf(h2d2).String(),
		reflect.TypeOf(tlb2).String(),
		reflect.TypeOf(replay2).String(),
		reflect.TypeOf(unblock).String(),
	}, f.d.UVMStats()
}

// TestScenarioF locks the ideal-parity contract: the identical predeclared
// trace in the normal and ideal modes produces the identical ordered
// functional trace and identical counters; only the mode flag and the
// fault-service latency rows differ (20 us vs zero).
func TestScenarioF(t *testing.T) {
	normalTrace, normal := parityTrace(t, false)
	idealTrace, ideal := parityTrace(t, true)

	// Identical ordered functional trace.
	if len(normalTrace) != len(idealTrace) {
		t.Fatalf("trace length = %d (normal) vs %d (ideal)",
			len(normalTrace), len(idealTrace))
	}
	for i := range normalTrace {
		if normalTrace[i] != idealTrace[i] {
			t.Fatalf("trace step %d = %s (normal) vs %s (ideal)",
				i, normalTrace[i], idealTrace[i])
		}
	}

	// Identical counters on every functional metric (the latency rows and
	// the mode flag are the only intentional differences).
	nt := reflect.TypeOf(normal)
	nv := reflect.ValueOf(normal)
	iv := reflect.ValueOf(ideal)
	for i := 0; i < nt.NumField(); i++ {
		name := nt.Field(i).Name
		switch name {
		case "IdealUVM", "FaultServiceLatencyTotal", "FaultServiceLatencyAvg":
			continue
		}
		if nv.Field(i).Interface() != iv.Field(i).Interface() {
			t.Errorf("cross-mode value of %s = %v (normal) vs %v (ideal)",
				name, nv.Field(i).Interface(), iv.Field(i).Interface())
		}
	}
	if normal.IdealUVM || !ideal.IdealUVM {
		t.Errorf("mode flags = %v/%v, want false/true",
			normal.IdealUVM, ideal.IdealUVM)
	}

	// The exact trace values (both modes).
	if normal.NumGPUPageFaultRequests != 1 || normal.NumUniqueFaultServices != 1 {
		t.Errorf("faults = %d/%d, want 1/1",
			normal.NumGPUPageFaultRequests, normal.NumUniqueFaultServices)
	}
	if normal.NumCPUToGPUMigrations != 2 || normal.BytesCPUToGPU != 68*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 2/68KB",
			normal.NumCPUToGPUMigrations, normal.BytesCPUToGPU)
	}
	if normal.BytesDemandMigrated != 60*mem.KB || normal.BytesPrefetched != 4*mem.KB {
		t.Errorf("demand/prefetch bytes = %d/%d, want 60KB/4KB",
			normal.BytesDemandMigrated, normal.BytesPrefetched)
	}
	if normal.NumAccessCounterMigrations != 1 ||
		normal.BytesAccessCounterMigrated != 4*mem.KB {
		t.Errorf("AC migration = %d/%d, want 1/4KB",
			normal.NumAccessCounterMigrations, normal.BytesAccessCounterMigrated)
	}
	if normal.NumUVMTLBRangeInvalidations != 2 ||
		normal.NumLocalPTEInstalls != 17 {
		t.Errorf("TLB/PTE = %d/%d, want 2/17",
			normal.NumUVMTLBRangeInvalidations, normal.NumLocalPTEInstalls)
	}
	if normal.NumAccessCounterIncrements != 8 ||
		normal.NumAccessCounterNotifications != 1 ||
		normal.NumAccessCounterThresholdHits != 1 {
		t.Errorf("access counter stats = %d/%d/%d, want 8/1/1",
			normal.NumAccessCounterIncrements, normal.NumAccessCounterNotifications,
			normal.NumAccessCounterThresholdHits)
	}

	// The latency rows: 20 us in the normal mode, zero in the ideal mode.
	if normal.FaultServiceLatencyTotal != latencySeconds(20*time.Microsecond) {
		t.Errorf("normal latency total = %v, want 20us",
			normal.FaultServiceLatencyTotal)
	}
	if ideal.FaultServiceLatencyTotal != 0 || ideal.FaultServiceLatencyAvg != 0 {
		t.Errorf("ideal latency = %v total/%v avg, want 0/0",
			ideal.FaultServiceLatencyTotal, ideal.FaultServiceLatencyAvg)
	}
}
