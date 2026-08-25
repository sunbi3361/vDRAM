package uvmtest

// sbin_codex: transition edge cases (plan todo 25 of mgpusim-uvm-manager):
// block-barrier, fragmented-PFN virtual dirty eviction, late fault delta,
// partial mask, in-flight old remote read, atomic rejection, managed-copy
// transition, and DMA rollback. Each subtest locks one deterministic edge of
// the §21 transition machinery through the public seams.

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	uvm "github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// TestScenarioTransitionEdges locks the eight transition edge cases.
func TestScenarioTransitionEdges(t *testing.T) {
	t.Run("block-barrier", testTransitionBlockBarrier)
	t.Run("fragmented-pfn-virtual-dirty-eviction", testTransitionFragmentedPFNEviction)
	t.Run("late-fault-delta", testTransitionLateFaultDelta)
	t.Run("partial-mask", testTransitionPartialMask)
	t.Run("in-flight-old-remote-read", testTransitionInflightRemoteRead)
	t.Run("atomic-rejection", testTransitionAtomicRejection)
	t.Run("managed-copy-transition", testTransitionManagedCopy)
	t.Run("dma-rollback", testTransitionDMARollback)
}

// testTransitionBlockBarrier proves the migration's block-barrier: no DMA is
// issued before the BlockRange watermark completion (uvm-manager.md §21.3
// step 2: "prevent creation of duplicate migration transactions"; §8.3).
func testTransitionBlockBarrier(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	f.d.AllocateManagedMemory(ctx, 64*mem.KB)

	deliverNotification(t, f, pid, 0, 8)
	runEngineFlush(t, f)
	block := retrieveReq(t, f.registeredPort)
	if _, ok := block.(*vm.BlockRange); !ok {
		t.Fatalf("migration request = %T, want BlockRange", block)
	}

	// The transaction waits for the watermark completion before any DMA.
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("DMA issued before the block completion: %d requests",
			len(reqs))
	}

	deliverGeneralRsp(t, f.gpuPort, block)
	runEngineFlush(t, f)
	h2d := retrieveReq(t, f.registeredPort)
	if _, ok := h2d.(*protocol.MemCopyH2DReq); !ok {
		t.Fatalf("post-block request = %T, want MemCopyH2DReq", h2d)
	}
}

// testTransitionFragmentedPFNEviction proves the eviction data path over the
// victim's physical pages: the range WB+INV carries page-aligned, sorted,
// non-overlapping physical runs covering exactly the valid pages, the clean
// D2H copies every valid page (never omitted, §18.3), the CPU backing
// receives the bytes, and the dirty accounting is exact. (Truly fragmented
// PFNs need the driver-internal pre-assignment hook — covered by the
// driver-package TestUVMReactiveEvictionFragmentedD2H; the GPU-side virtual
// dirtiness is handled by the WB+INV flush request itself.)
func testTransitionFragmentedPFNEviction(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = false
	cfg.GPUMemoryCapacity = 128 * mem.KB
	cfg.CapacitySet = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)

	// Region 0 (60 KB) becomes GPU-resident; the second admission (56 KB)
	// fits but the headroom is short, so the LRU victim (region 0) is
	// pre-evicted concurrently.
	ptr1 := f.d.AllocateManagedMemory(ctx, 60*mem.KB)
	deliverFault(t, f, pid, uint64(ptr1))
	runEngineFlush(t, f)
	h2d1 := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, h2d1)
	runEngineFlush(t, f)
	replay1 := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, replay1.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)

	ptr2 := f.d.AllocateManagedMemory(ctx, 128*mem.KB)
	deliverFault(t, f, pid, uint64(ptr2))
	runEngineFlush(t, f)
	reqs := drainReqs(f.registeredPort)
	if len(reqs) != 2 {
		t.Fatalf("service requests = %d, want 2 (H2D + victim block)",
			len(reqs))
	}
	h2d2, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request 0 = %T, want MemCopyH2DReq", reqs[0])
	}
	block, ok := reqs[1].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request 1 = %T, want BlockRange", reqs[1])
	}

	// The WB+INV flush: the runs cover exactly the 15 valid pages (60 KB),
	// page-aligned, sorted, and non-overlapping.
	deliverGeneralRsp(t, f.gpuPort, block)
	runEngineFlush(t, f)
	flush := retrieveReq(t, f.registeredPort)
	flushReq, ok := flush.(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("post-block request = %T, want UVMCacheRangeFlushReq", flush)
	}
	if flushReq.Operation != cache.UVMCacheRangeFlushWritebackInvalidate {
		t.Errorf("flush operation = %v, want WB+INV", flushReq.Operation)
	}
	var runBytes uint64
	prevEnd := uint64(0)
	for i, run := range flushReq.PhysicalRuns {
		if run.Start%4096 != 0 || run.Length%4096 != 0 {
			t.Errorf("run %d = %#x+%d, not page-aligned", i, run.Start, run.Length)
		}
		if i > 0 && run.Start < prevEnd {
			t.Errorf("run %d start %#x overlaps the previous run end %#x",
				i, run.Start, prevEnd)
		}
		if i > 0 && run.Start < flushReq.PhysicalRuns[i-1].Start {
			t.Errorf("runs not sorted at %d", i)
		}
		prevEnd = run.Start + run.Length
		runBytes += run.Length
	}
	if runBytes != 60*mem.KB {
		t.Errorf("flush runs cover %d bytes, want 60KB (15 valid pages)",
			runBytes)
	}
	deliverFlushRsp(t, f.gpuPort, flushReq)
	runEngineFlush(t, f)

	// The 64 KB TLB invalidation, then the D2H of every valid page.
	tlb := retrieveReq(t, f.registeredPort)
	deliverTLBAck(t, f.gpuPort, tlb.(*protocol.UVMTLBInvalidateReq))
	runEngineFlush(t, f)
	d2h := retrieveReq(t, f.registeredPort)
	d2hReq, ok := d2h.(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want MemCopyD2HReq", d2h)
	}
	if uint64(len(d2hReq.DstBuffer)) != 60*mem.KB {
		t.Errorf("D2H bytes = %d, want 60KB (every valid page)", len(d2hReq.DstBuffer))
	}
	for j := range d2hReq.DstBuffer {
		d2hReq.DstBuffer[j] = byte(j%251 + 1)
	}
	deliverGeneralRsp(t, f.gpuPort, d2hReq)
	runEngineFlush(t, f)

	// The CPU backing received every page's bytes (the D2H buffer is filled
	// with the global byte index, so each page's local check uses its global
	// offset).
	pageIdx := uint64(0)
	for va := uint64(ptr1); va < uint64(ptr1)+60*mem.KB; va += 4096 {
		cpuPA := cpuBackingPA(t, f, pid, va)
		data, err := f.storage.Read(cpuPA, 4096)
		if err != nil {
			t.Fatal(err)
		}
		for j := range data {
			want := byte((pageIdx*4096+uint64(j))%251 + 1)
			if data[j] != want {
				t.Fatalf("CPU backing page %#x byte %d = %d, want %d",
					va, j, data[j], want)
			}
		}
		pageIdx++
	}

	// The dirty accounting: the driver-side dirty marks are never set (the
	// GPU-side dirtiness is invisible to the driver; the WB+INV flush is the
	// virtual dirty handling).
	if snap := f.d.UVMStats(); snap.NumDirtyEvictions != 0 {
		t.Errorf("dirty evictions = %d, want 0", snap.NumDirtyEvictions)
	}
	// The incoming H2D was never stalled by the eviction.
	if snap := f.d.UVMStats(); snap.MigrationWaitCyclesForCapacity != 0 {
		t.Errorf("capacity wait = %d, want 0", snap.MigrationWaitCyclesForCapacity)
	}
	_ = h2d2
}

// testTransitionLateFaultDelta proves a fault arriving while the region's
// migration is in flight coalesces into the live transaction: no duplicate
// DMA, one replay, and the exact raw/unique/coalesced counts.
func testTransitionLateFaultDelta(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)

	deliverFault(t, f, pid, uint64(ptr))
	runEngineFlush(t, f)
	h2d := retrieveReq(t, f.registeredPort)
	if _, ok := h2d.(*protocol.MemCopyH2DReq); !ok {
		t.Fatalf("service request = %T, want MemCopyH2DReq", h2d)
	}

	// A late fault on a different page of the same 64 KB region while the
	// H2D is in flight: coalesced, no duplicate DMA.
	deliverFault(t, f, pid, uint64(ptr)+8192)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("duplicate DMA for the coalesced fault: %d requests",
			len(reqs))
	}

	deliverGeneralRsp(t, f.gpuPort, h2d)
	runEngineFlush(t, f)
	replay := retrieveReq(t, f.registeredPort)
	replayReq, ok := replay.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMFaultReplayReq", replay)
	}
	deliverReplayAck(t, f.gpuPort, replayReq)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("post-replay requests = %d, want 0", len(reqs))
	}

	snap := f.d.UVMStats()
	if snap.NumGPUPageFaultRequests != 2 || snap.NumUniqueFaultServices != 1 ||
		snap.NumCoalescedFaults != 1 {
		t.Errorf("fault counts = %d/%d/%d, want 2/1/1",
			snap.NumGPUPageFaultRequests, snap.NumUniqueFaultServices,
			snap.NumCoalescedFaults)
	}
	if snap.NumCPUToGPUMigrations != 1 || snap.BytesCPUToGPU != 64*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 1/64KB (no duplicate DMA)",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
}

// testTransitionPartialMask proves the eviction flush mask of a region with
// a partial valid-page set: the 1-valid-page victim (a 4 KB allocation)
// flushes with mask bit 0 set and a single 4 KB run.
func testTransitionPartialMask(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = false
	cfg.GPUMemoryCapacity = 128 * mem.KB
	cfg.CapacitySet = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)

	// Region 0 holds a single 4 KB page (VA 4096): R = 4 KB.
	ptr1 := f.d.AllocateManagedMemory(ctx, 4*mem.KB)
	deliverFault(t, f, pid, uint64(ptr1))
	runEngineFlush(t, f)
	h2d1 := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, h2d1)
	runEngineFlush(t, f)
	replay1 := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, replay1.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)

	// The second fault on the full 128 KB allocation selects the 2 MB root
	// (16/30 > 51% at every level): 120 KB migrate. The admission fits with
	// 4 KB free, so the headroom trigger pre-evicts the LRU victim (region
	// 0 — the only resident region, 1 valid page).
	ptr2 := f.d.AllocateManagedMemory(ctx, 128*mem.KB)
	deliverFault(t, f, pid, uint64(ptr2)+64*mem.KB)
	runEngineFlush(t, f)
	reqs := drainReqs(f.registeredPort)
	if len(reqs) != 2 {
		t.Fatalf("service requests = %d, want 2 (H2D + victim block)",
			len(reqs))
	}
	h2d2, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request 0 = %T, want MemCopyH2DReq", reqs[0])
	}
	if uint64(len(h2d2.SrcBuffer)) != 120*mem.KB {
		t.Errorf("H2D bytes = %d, want 120KB", len(h2d2.SrcBuffer))
	}
	block, ok := reqs[1].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request 1 = %T, want BlockRange", reqs[1])
	}
	if block.StartVA != 0 {
		t.Errorf("victim = %#x, want region 0 (LRU)", block.StartVA)
	}

	// The partial mask: only the allocation's page (VA 4096 = region-local
	// bit 1) of the 64 KB region is valid.
	deliverGeneralRsp(t, f.gpuPort, block)
	runEngineFlush(t, f)
	flush := retrieveReq(t, f.registeredPort)
	flushReq, ok := flush.(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("post-block request = %T, want UVMCacheRangeFlushReq", flush)
	}
	if flushReq.ValidPageMask != 0x2 {
		t.Errorf("partial flush mask = %#x, want 0x2 (1 valid page, VA 4096)",
			flushReq.ValidPageMask)
	}
	if uint64(len(flushReq.PhysicalRuns)) != 1 ||
		flushReq.PhysicalRuns[0].Length != 4*mem.KB {
		t.Errorf("partial flush runs = %+v, want one 4KB run",
			flushReq.PhysicalRuns)
	}
	deliverFlushRsp(t, f.gpuPort, flushReq)
	runEngineFlush(t, f)
	tlb := retrieveReq(t, f.registeredPort)
	deliverTLBAck(t, f.gpuPort, tlb.(*protocol.UVMTLBInvalidateReq))
	runEngineFlush(t, f)
	d2h := retrieveReq(t, f.registeredPort)
	d2hReq, ok := d2h.(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want MemCopyD2HReq", d2h)
	}
	if uint64(len(d2hReq.DstBuffer)) != 4*mem.KB {
		t.Errorf("partial D2H bytes = %d, want 4KB", len(d2hReq.DstBuffer))
	}
}

// testTransitionInflightRemoteRead proves an old remote read that already
// resolved to the CPU physical address completes concurrently with the
// REMOTE -> GPU_LOCAL migration and is allowed to finish using that remote
// address (uvm-manager.md §21.3: "A remote read that has already resolved to
// a CPU physical address before the mapping transition is allowed to finish
// using that remote address").
func testTransitionInflightRemoteRead(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)
	cpuPA := cpuBackingPA(t, f, pid, uint64(ptr))
	hostData := []byte("in-flight-old-remote-read-data")
	if err := f.storage.Write(cpuPA, hostData); err != nil {
		t.Fatalf("storage.Write: %v", err)
	}

	counter, endpoint, requesterPort :=
		buildCounterAndEndpoint(t, engine, 8)

	// The old remote read resolves to the CPU PA and is forwarded over PCIe
	// (in flight — not yet answered).
	deliverRemoteRead(t, endpoint, requesterPort, pid,
		uint64(ptr), cpuPA, uint64(len(hostData)))
	runEngineFlush(t, f)
	forwarded := retrieveReq(t, endpoint.ToRDMA)
	readReq, ok := forwarded.(*mem.ReadReq)
	if !ok {
		t.Fatalf("forwarded request = %T, want *mem.ReadReq", forwarded)
	}

	// The migration runs to completion while the read is in flight.
	for i := 0; i < 7; i++ {
		counter.RecordRemoteAccess(pid, 1, uint64(ptr))
	}
	counter.Tick()
	retrieveReq(t, counter.ToCP)
	deliverNotification(t, f, pid, 0, 8)
	runEngineFlush(t, f)
	block := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, block)
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
	unblock := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, unblock)
	runEngineFlush(t, f)

	// The mapping transitioned to GPU_LOCAL.
	page := pteOf(t, f.gpuTables[0], pid, uint64(ptr))
	if page.Location != vm.MemoryLocationGPU_LOCAL {
		t.Fatalf("mapping after migration = %s, want GPU_LOCAL", page.Location)
	}

	// The old read still completes using its remote address.
	rsp := readReq.GenerateRsp(hostData)
	if err := endpoint.ToRDMA.Deliver(rsp); err != nil {
		t.Fatalf("Deliver data response: %v", err)
	}
	runEngineFlush(t, f)
	got := retrieveReq(t, requesterPort)
	dataRsp, ok := got.(*mem.DataReadyRsp)
	if !ok {
		t.Fatalf("requester response = %T, want *mem.DataReadyRsp", got)
	}
	if !bytes.Equal(dataRsp.Data, hostData) {
		t.Error("old remote read did not complete with its remote address")
	}
}

// unsupportedAtomicReq is a memory request the selected protocol cannot
// represent as a remote atomic (uvm-manager.md §15.1): it implements
// mem.AccessReq but is neither a read nor a write.
type unsupportedAtomicReq struct {
	*mem.ReadReq
}

// testTransitionAtomicRejection proves an unsupported remote atomic is
// rejected explicitly by the endpoint and never forwarded to host memory.
func testTransitionAtomicRejection(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 64*mem.KB)
	cpuPA := cpuBackingPA(t, f, pid, uint64(ptr))

	_, endpoint, requesterPort :=
		buildCounterAndEndpoint(t, engine, 8)

	req := &unsupportedAtomicReq{
		ReadReq: mem.ReadReqBuilder{}.
			WithSrc(requesterPort.AsRemote()).
			WithDst(endpoint.ToGPU.AsRemote()).
			WithAddress(uint64(ptr)).
			WithByteSize(8).
			WithPID(pid).
			Build(),
	}
	req.Info = &uvm.RemoteAccessAnnotation{
		Location: vm.MemoryLocationCPU_REMOTE,
		PAddr:    cpuPA,
	}
	if err := endpoint.ToGPU.Deliver(req); err != nil {
		t.Fatalf("Deliver atomic: %v", err)
	}
	runEngineFlush(t, f)

	if got := endpoint.RejectedAtomicCount(); got != 1 {
		t.Errorf("rejected atomics = %d, want 1", got)
	}
	if reqs := drainReqs(endpoint.ToRDMA); len(reqs) != 0 {
		t.Errorf("atomic forwarded to host memory: %d requests", len(reqs))
	}
	if got := endpoint.ToGPU.PeekIncoming(); got != nil {
		t.Errorf("atomic not consumed: %+v", got)
	}
}

// testTransitionManagedCopy proves the managed-copy transition: a copy over
// a region owned by an in-flight fault waits by ticket, claims the ownership
// slot only after the fault completes, and then runs block -> WB+INV -> H2D
// DMA -> unblock with the exact span data (todo 5 ownership model).
func testTransitionManagedCopy(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)
	ptr := f.d.AllocateManagedMemory(ctx, 60*mem.KB)

	// The fault's H2D is in flight (the region is fault-owned).
	deliverFault(t, f, pid, uint64(ptr))
	runEngineFlush(t, f)
	h2d := retrieveReq(t, f.registeredPort)
	if _, ok := h2d.(*protocol.MemCopyH2DReq); !ok {
		t.Fatalf("service request = %T, want MemCopyH2DReq", h2d)
	}

	// The managed H2D copy over the fault-owned span waits by ticket: no
	// copy request is emitted while the fault owns the region.
	copyData := make([]byte, 60*mem.KB)
	for i := range copyData {
		copyData[i] = byte(i*13 + 7)
	}
	queue := f.d.CreateCommandQueue(ctx)
	f.d.EnqueueMemCopyH2D(queue, ptr, copyData)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("copy issued while the fault owns the region: %d requests",
			len(reqs))
	}

	// The fault completes and releases the ownership; the copy claims the
	// slot and blocks.
	deliverGeneralRsp(t, f.gpuPort, h2d)
	runEngineFlush(t, f)
	replay := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, replay.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)
	block := retrieveReq(t, f.registeredPort)
	blockReq, ok := block.(*vm.BlockRange)
	if !ok {
		t.Fatalf("copy request = %T, want BlockRange", block)
	}
	if blockReq.StartVA != uint64(ptr) || blockReq.Size != 60*mem.KB {
		t.Errorf("copy block = %#x+%d, want the exact span",
			blockReq.StartVA, blockReq.Size)
	}

	// WB+INV over the resident span, then the per-page H2D DMA.
	deliverGeneralRsp(t, f.gpuPort, blockReq)
	runEngineFlush(t, f)
	flush := retrieveReq(t, f.registeredPort)
	flushReq, ok := flush.(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("copy request = %T, want UVMCacheRangeFlushReq", flush)
	}
	if flushReq.ValidPageMask != 0xFFFE {
		t.Errorf("copy flush mask = %#x, want 0xFFFE", flushReq.ValidPageMask)
	}
	deliverFlushRsp(t, f.gpuPort, flushReq)
	runEngineFlush(t, f)

	reqs := drainReqs(f.registeredPort)
	if len(reqs) != 15 {
		t.Fatalf("copy DMA requests = %d, want 15 (one per page)", len(reqs))
	}
	for i, r := range reqs {
		dma, ok := r.(*protocol.MemCopyH2DReq)
		if !ok {
			t.Fatalf("copy DMA request %d = %T, want MemCopyH2DReq", i, r)
		}
		want := copyData[i*4096 : (i+1)*4096]
		if !bytes.Equal(dma.SrcBuffer, want) {
			t.Errorf("copy DMA request %d payload mismatch", i)
		}
		deliverGeneralRsp(t, f.gpuPort, dma)
	}
	runEngineFlush(t, f)

	unblock := retrieveReq(t, f.registeredPort)
	unblockReq, ok := unblock.(*vm.UnblockRange)
	if !ok {
		t.Fatalf("copy request = %T, want UnblockRange", unblock)
	}
	deliverGeneralRsp(t, f.gpuPort, unblockReq)
	runEngineFlush(t, f)
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("post-copy requests = %d, want 0", len(reqs))
	}
	if queue.NumCommand() != 0 {
		t.Error("copy command did not complete")
	}
	// The copy is residency-neutral: no migration/eviction counters moved.
	snap := f.d.UVMStats()
	if snap.NumCPUToGPUMigrations != 1 || snap.NumEvictions != 0 {
		t.Errorf("copy touched migration/eviction counters: %d/%d",
			snap.NumCPUToGPUMigrations, snap.NumEvictions)
	}
}

// testTransitionDMARollback proves the failed-admission rollback: a hard
// capacity shortage releases the reservation (the retry succeeds, proving
// nothing leaked), emits no H2D, changes no PTE, does not re-run the TBN
// selection, and re-prepares the same missing pages once capacity is freed.
// (The mid-DMA failAfterRuns hook is driver-internal — covered by the
// driver-package TestUVMSecondRunRollback.)
func testTransitionDMARollback(t *testing.T) {
	engine := newEngine(t)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = false
	cfg.GPUMemoryCapacity = 128 * mem.KB
	cfg.CapacitySet = true
	f := buildScenarioFixture(t, engine, cfg)

	ctx := f.d.Init()
	pid := contextPID(ctx)

	// R = 124 KB (the 128 KB allocation's fault on region 1 migrates 31
	// pages via the [0,128KB) TBN node).
	ptr1 := f.d.AllocateManagedMemory(ctx, 128*mem.KB)
	deliverFault(t, f, pid, uint64(ptr1)+64*mem.KB)
	runEngineFlush(t, f)
	h2d1 := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, h2d1)
	runEngineFlush(t, f)
	replay1 := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, replay1.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)

	// The 8 KB allocation's fault (2 pages = 8 KB) cannot fit: the
	// admission rolls back — no H2D, no PTE change — and the LRU victim is
	// launched.
	ptr2 := f.d.AllocateManagedMemory(ctx, 8*mem.KB)
	deliverFault(t, f, pid, uint64(ptr2))
	runEngineFlush(t, f)
	block := retrieveReq(t, f.registeredPort)
	if _, ok := block.(*vm.BlockRange); !ok {
		t.Fatalf("rollback request = %T, want BlockRange (victim only)", block)
	}
	if reqs := drainReqs(f.registeredPort); len(reqs) != 0 {
		t.Errorf("H2D emitted on the failed admission: %d requests", len(reqs))
	}
	// No PTE change on the failed attempt.
	page := pteOf(t, f.gpuTables[0], pid, uint64(ptr2))
	if page.Valid {
		t.Errorf("PTE published by the failed admission: %+v", page)
	}
	if got := f.d.UVMStats().MigrationWaitCyclesForCapacity; got < 1 {
		t.Errorf("capacity wait cycles = %d, want >= 1", got)
	}

	// The eviction frees capacity; the retry re-prepares the SAME missing
	// pages (the TBN selection is not re-run) and the admission succeeds —
	// proving the failed attempt released its reservation.
	deliverGeneralRsp(t, f.gpuPort, block)
	runEngineFlush(t, f)
	flush := retrieveReq(t, f.registeredPort)
	deliverFlushRsp(t, f.gpuPort, flush.(*protocol.UVMCacheRangeFlushReq))
	runEngineFlush(t, f)
	tlb := retrieveReq(t, f.registeredPort)
	deliverTLBAck(t, f.gpuPort, tlb.(*protocol.UVMTLBInvalidateReq))
	runEngineFlush(t, f)
	d2h := retrieveReq(t, f.registeredPort)
	deliverGeneralRsp(t, f.gpuPort, d2h)
	runEngineFlush(t, f)
	victimReplay := retrieveReq(t, f.registeredPort)
	deliverReplayAck(t, f.gpuPort, victimReplay.(*protocol.UVMFaultReplayReq))
	runEngineFlush(t, f)

	// The frames were freed before the replay, so the retry now fits: the
	// H2D is emitted in the same tick as the victim's UnblockRange.
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
		t.Errorf("retry H2D bytes = %d, want 8KB (the original missing set)",
			len(h2d2.SrcBuffer))
	}
	unblock, ok := reqs[1].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request 1 = %T, want UnblockRange", reqs[1])
	}
	deliverGeneralRsp(t, f.gpuPort, unblock)
	runEngineFlush(t, f)

	// The headroom trigger launches the next LRU victim (region 1).
	block2 := retrieveReq(t, f.registeredPort)
	if _, ok := block2.(*vm.BlockRange); !ok {
		t.Fatalf("post-unblock request = %T, want BlockRange", block2)
	}

	snap := f.d.UVMStats()
	if snap.NumTBNFaultEvents != 2 {
		t.Errorf("TBN fault events = %d, want 2 (selection not re-run on retry)",
			snap.NumTBNFaultEvents)
	}
	if snap.NumGPUPageFaultRequests != 2 {
		t.Errorf("raw faults = %d, want 2", snap.NumGPUPageFaultRequests)
	}
}
