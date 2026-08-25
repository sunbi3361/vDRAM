package uvmtest

// sbin_codex: Scenario B — remote read below threshold (uvm-manager.md §9,
// §14, §15). Access Counter enabled, threshold 8. A GPU read of a
// CPU-resident managed page resolves through the CPU_REMOTE PTE, is served
// by the remote endpoint over modeled PCIe (RDMA), the data bypasses the GPU
// data caches, the access counter increments below the threshold (no
// notification), and the page stays CPU-resident.

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
)

// TestScenarioB locks the below-threshold remote read: the read completes
// via PCIe with the host data, the counter stays below the threshold, no
// notification is emitted, no migration is created, and the GPU PTE remains
// CPU_REMOTE (the page stays CPU-resident).
func TestScenarioB(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)

	// The initial GPU PTE is CPU_REMOTE (Access Counter enabled): the page
	// is CPU-resident and the translation is consumable by the remote
	// endpoint.
	page := pteOf(t, f.gpuTables[0], pid, uint64(ptr))
	if !page.Valid || page.Location != vm.MemoryLocationCPU_REMOTE ||
		page.PAddr == 0 {
		t.Fatalf("initial GPU PTE = %+v, want CPU_REMOTE", page)
	}
	cpuPA := cpuBackingPA(t, f, pid, uint64(ptr))

	// The CPU backing holds known host data.
	hostData := []byte("remote-read-below-threshold-payload")
	if err := f.storage.Write(cpuPA, hostData); err != nil {
		t.Fatalf("storage.Write: %v", err)
	}

	counter, endpoint, requesterPort :=
		buildCounterAndEndpoint(t, engine, 8)

	// Step 1-5: the GPU read of the managed VA is marked CPU_REMOTE, the
	// access counter increments, and the cache-line request is sent through
	// PCIe (the endpoint's RDMA seam — before any GPU data cache).
	deliverRemoteRead(t, endpoint, requesterPort, pid,
		uint64(ptr), cpuPA, uint64(len(hostData)))
	runEngine(t, f)

	forwarded := retrieveReq(t, endpoint.ToRDMA)
	readReq, ok := forwarded.(*mem.ReadReq)
	if !ok {
		t.Fatalf("forwarded request = %T, want *mem.ReadReq", forwarded)
	}
	if readReq.GetAddress() != cpuPA {
		t.Errorf("forwarded read address = %#x, want the CPU backing PA %#x",
			readReq.GetAddress(), cpuPA)
	}
	if readReq.GetByteSize() != uint64(len(hostData)) {
		t.Errorf("forwarded read size = %d, want %d",
			readReq.GetByteSize(), len(hostData))
	}

	// Step 6-8: the CPU memory returns the data; the endpoint routes it back
	// to the original GPU requester (the data bypassed the GPU caches).
	rsp := readReq.GenerateRsp(hostData)
	if err := endpoint.ToRDMA.Deliver(rsp); err != nil {
		t.Fatalf("Deliver data response: %v", err)
	}
	runEngine(t, f)

	got := retrieveReq(t, requesterPort)
	dataRsp, ok := got.(*mem.DataReadyRsp)
	if !ok {
		t.Fatalf("requester response = %T, want *mem.DataReadyRsp", got)
	}
	if !bytes.Equal(dataRsp.Data, hostData) {
		t.Error("requester did not receive the host data")
	}

	// Step 9: the page remains CPU-resident — the counter is below the
	// threshold, no notification is emitted, and the driver created no
	// migration.
	if got := counter.Count(pid, 1, 0); got != 1 {
		t.Errorf("access counter = %d, want 1", got)
	}
	if got := counter.Threshold(); got != 8 {
		t.Errorf("threshold = %d, want 8", got)
	}
	if got := counter.Count(pid, 1, 0); got >= counter.Threshold() {
		t.Errorf("counter %d reached the threshold %d below-threshold",
			got, counter.Threshold())
	}
	// Flush any queued notification (there must be none below threshold).
	counter.Tick()
	if reqs := drainReqs(counter.ToCP); len(reqs) != 0 {
		t.Errorf("notifications below threshold = %d, want 0", len(reqs))
	}

	page = pteOf(t, f.gpuTables[0], pid, uint64(ptr))
	if !page.Valid || page.Location != vm.MemoryLocationCPU_REMOTE {
		t.Errorf("GPU PTE after remote read = %+v, want CPU_REMOTE "+
			"(page stays CPU-resident)", page)
	}
	snap := f.d.UVMStats()
	if snap.NumAccessCounterMigrations != 0 || snap.NumCPUToGPUMigrations != 0 ||
		snap.NumAccessCounterNotifications != 0 {
		t.Errorf("driver migration/notification stats = %d/%d/%d, want 0/0/0",
			snap.NumAccessCounterMigrations, snap.NumCPUToGPUMigrations,
			snap.NumAccessCounterNotifications)
	}
}
