package uvmtest

// sbin_codex: Scenario D — remote write (uvm-manager.md §15, §21.3). A GPU
// write to a CPU_REMOTE mapping is NOT sent as a normal remote store: the
// remote endpoint parks it (never committed to host memory) and the region
// is migrated CPU -> GPU; the PTE becomes GPU_LOCAL, the mandatory TLB range
// invalidation runs, the write is replayed and completes in GPU memory, and
// no host commit ever happens.
//
// The driver-side write-trigger seam (migrationMiddleware.intakeRemoteWrite)
// is driver-internal with no protocol envelope, so the external fixture
// drives the same migration machinery through the AccessCounterNotification
// seam; the write-park behavior itself is asserted GPU-side against the
// remote endpoint. The num_write_triggered_migrations counter is covered by
// the driver-package TestUVMRemoteWriteMigration.

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// TestScenarioD locks the remote-write specification scenario: the write
// stalls (parked, never committed), the migration runs, the PTE becomes
// GPU_LOCAL, the TLB invalidates, the write is replayed, and the host memory
// is untouched.
func TestScenarioD(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)

	// The CPU backing holds known host data that the write must never
	// modify.
	hostData := make([]byte, 64)
	for i := range hostData {
		hostData[i] = 0xAB
	}
	cpuPA := cpuBackingPA(t, f, pid, uint64(ptr))
	if err := f.storage.Write(cpuPA, hostData); err != nil {
		t.Fatalf("storage.Write: %v", err)
	}

	_, endpoint, requesterPort :=
		buildCounterAndEndpoint(t, engine, 8)

	// Step 1-4: the GPU issues a write; the translation returns the REMOTE
	// PTE; the write is NOT sent as a normal remote store — the endpoint
	// parks it and the request stalls.
	writeData := bytes.Repeat([]byte{0xCD}, 64)
	deliverRemoteWrite(t, endpoint, requesterPort, pid,
		uint64(ptr), cpuPA, 64, writeData)
	runEngineFlush(t, f)

	if reqs := drainReqs(endpoint.ToRDMA); len(reqs) != 0 {
		t.Errorf("parked write forwarded to PCIe: %d requests", len(reqs))
	}
	if got := endpoint.ToGPU.PeekIncoming(); got != nil {
		t.Errorf("parked write not consumed by the endpoint: %+v", got)
	}
	// No host commit: the CPU backing still holds the original data.
	got, err := f.storage.Read(cpuPA, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, hostData) {
		t.Error("remote write committed to host memory")
	}

	// Step 5-7: the migration request is generated and the CPU -> GPU
	// migration occurs (driven through the notification seam; the write
	// trigger itself is driver-internal). The migration blocks the range,
	// transfers the 15 valid pages, and publishes GPU_LOCAL PTEs.
	deliverNotification(t, f, pid, 0, 8)
	runEngineFlush(t, f)

	block := retrieveReq(t, f.registeredPort)
	blockReq, ok := block.(*vm.BlockRange)
	if !ok {
		t.Fatalf("migration request = %T, want BlockRange", block)
	}
	deliverGeneralRsp(t, f.gpuPort, blockReq)
	runEngineFlush(t, f)

	h2d := retrieveReq(t, f.registeredPort)
	h2dReq, ok := h2d.(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("post-block request = %T, want MemCopyH2DReq", h2d)
	}
	deliverGeneralRsp(t, f.gpuPort, h2dReq)
	runEngineFlush(t, f)

	// Step 8-9: the PTE becomes GPU_LOCAL and the TLB range invalidation
	// occurs (REMOTE -> GPU_LOCAL REQUIRES it).
	tlb := retrieveReq(t, f.registeredPort)
	tlbReq, ok := tlb.(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", tlb)
	}
	deliverTLBAck(t, f.gpuPort, tlbReq)
	runEngineFlush(t, f)

	// Step 10: the write is replayed and completes in GPU memory.
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

	// The write now completes in GPU memory: the mapping is GPU_LOCAL.
	page := pteOf(t, f.gpuTables[0], pid, uint64(ptr))
	if !page.Valid || page.Location != vm.MemoryLocationGPU_LOCAL ||
		page.PAddr == 0 {
		t.Errorf("GPU PTE after migration = %+v, want GPU_LOCAL", page)
	}

	// No host commit at any point.
	got, err = f.storage.Read(cpuPA, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, hostData) {
		t.Error("host memory modified by the remote write")
	}

	// Exact counters.
	snap := f.d.UVMStats()
	if snap.NumCPUToGPUMigrations != 1 || snap.BytesCPUToGPU != 60*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 1/60KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumUVMTLBRangeInvalidations != 1 {
		t.Errorf("TLB range invalidations = %d, want 1",
			snap.NumUVMTLBRangeInvalidations)
	}
	if snap.NumLocalPTEInstalls != 15 {
		t.Errorf("local PTE installs = %d, want 15", snap.NumLocalPTEInstalls)
	}
	// The write-trigger seam is driver-internal (no protocol envelope); the
	// driver-package TestUVMRemoteWriteMigration covers the trigger counter.
	if snap.NumWriteTriggeredMigrations != 0 {
		t.Errorf("write-triggered migrations = %d, want 0 (seam is "+
			"driver-internal; notification-driven migration here)",
			snap.NumWriteTriggeredMigrations)
	}
}
