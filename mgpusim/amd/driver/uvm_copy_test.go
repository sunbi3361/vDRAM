package driver

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: residency-neutral managed H2D/D2H copy contract tests (todo 5 of
// plan mgpusim-uvm-manager). The QA regex
// 'TestManaged(H2D|D2H|CopyTransition|CopyReject|DirtyGPUOverwrite|
// DirtyGPUReadback|CopyBlockBarrier|RemoteReadH2DRace|AtomicMultiRegionClaim|
// InverseOverlapNoDeadlock|GenericTransitionOwnership)' runs the fixtures in
// this file: all-or-none ownership, ticket fairness, exact byte/cache
// visibility, no duplicate work, and unchanged residency/counters/global
// controls.

type payload16K struct{ Data [16 * mem.KB]byte }
type payload64K struct{ Data [64 * mem.KB]byte }
type payload128K struct{ Data [128 * mem.KB]byte }

// buildCopyDriver builds a real driver (real allocator, real CPU + GPU page
// tables, real UVM manager, host storage) plus a registered GPU port and a
// standalone managed copy middleware.
func buildCopyDriver(t *testing.T, accessCounter bool) (
	*Driver, *managedMemoryCopyMiddleware,
) {
	t.Helper()

	d, _, _ := buildManagedDriver(t, accessCounter)
	gpuPort := sim.NewPort(d, 1, 1, "TestGPU")
	d.RegisterGPU(gpuPort, DeviceProperties{CUCount: 4, DRAMSize: 4 * mem.GB})
	mw := &managedMemoryCopyMiddleware{driver: d}
	return d, mw
}

// drainRequests takes every request the middleware queued for the GPU.
func drainRequests(d *Driver) []sim.Msg {
	reqs := d.requestsToSend
	d.requestsToSend = nil
	return reqs
}

// deliverGeneralRsp injects the CP-style completion for a request into the
// driver's GPU port.
func deliverGeneralRsp(t *testing.T, d *Driver, originalReq sim.Msg) {
	t.Helper()

	rsp := sim.GeneralRspBuilder{}.
		WithSrc(d.gpuPort.AsRemote()).
		WithDst(d.gpuPort.AsRemote()).
		WithOriginalReq(originalReq).
		Build()
	if err := d.gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// deliverFlushRsp injects the CP-style range-flush completion for a request.
func deliverFlushRsp(t *testing.T, d *Driver, req *protocol.UVMCacheRangeFlushReq) {
	t.Helper()

	rsp := protocol.NewUVMCacheRangeFlushRsp(d.gpuPort, d.gpuPort, req.ID)
	if err := d.gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// allZero reports whether every byte of data is zero.
func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// makeRegionGPUResident transitions one 64 KB region to GPU_RESIDENT through
// the Todo 4 fixture (RegionStateMachine + AdmissionReservation): the legal
// §23 chain, resident masks, HBM PAs for the region's valid pages, and a
// committed capacity reservation.
func makeRegionGPUResident(
	t *testing.T,
	d *Driver,
	reg *ManagedAllocationRegistration,
	blockIdx, regionIdx uint64,
) {
	t.Helper()

	block := reg.VABlocks[blockIdx]
	region := block.SubBlocks[regionIdx]
	sm := NewRegionStateMachine(
		RegionContext{PID: reg.PID, GPU: 1, Block: blockIdx, Region: regionIdx},
		region)
	now := sim.VTimeInSec(1)
	for _, to := range []RegionState{
		RegionFaultPending, RegionMigratingToGPU, RegionGPUResident,
	} {
		if err := sm.Transition(to, now); err != nil {
			t.Fatalf("Transition(%s): %v", to, err)
		}
	}

	allocStart, valid := (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		setResident(reg, page, true)
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		block.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}

	if err := d.uvm.Reservation().ReserveAdmission(64 * mem.KB); err != nil {
		t.Fatalf("ReserveAdmission: %v", err)
	}
	d.uvm.Reservation().CommitAdmission(64 * mem.KB)
}

// TestManagedH2D drives a full managed H2D copy over CPU_REMOTE pages (Access
// Counter on): one global ticket, atomic claim of both region keys, BlockRange
// before any CPU mutation, exact bytes in the CPU backing, atomic release,
// unblock, wake, and unchanged residency/counters.
func TestManagedH2D(t *testing.T) {
	d, mw := buildCopyDriver(t, true)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	src := payload64K{}
	for i := range src.Data {
		src.Data[i] = byte(i)
	}

	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "managed-h2d", Dst: ptr, Src: src}
	q.Enqueue(cmd)

	processed, handled := mw.tryProcess(cmd, q)
	if !processed || !handled {
		t.Fatalf("tryProcess = %v/%v, want true/true", processed, handled)
	}
	if !q.IsRunning {
		t.Error("queue not running after copy start")
	}

	tx := mw.copies[0]
	if !tx.claimed {
		t.Fatal("copy not claimed")
	}
	if len(tx.Keys) != 2 {
		t.Fatalf("keys = %d, want 2 (regions 0 and 64KB)", len(tx.Keys))
	}
	if tx.Keys[0].RegionBase != 0 || tx.Keys[1].RegionBase != 64*mem.KB {
		t.Errorf("keys = %+v, want region bases 0 and 64KB", tx.Keys)
	}
	for _, key := range tx.Keys {
		if d.uvm.IsKeyIdle(key) {
			t.Errorf("key %+v not owned by the copy", key)
		}
		typ, owner := d.uvm.OwnerOf(key)
		if typ != OwnershipCopy || owner != tx.Ticket {
			t.Errorf("key %+v owner = %v/%d, want COPY/%d", key, typ, owner, tx.Ticket)
		}
	}

	// BlockRange is sent before any data mutation.
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 BlockRange", len(reqs))
	}
	block, ok := reqs[0].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request type = %T, want *vm.BlockRange", reqs[0])
	}
	if block.CommandID != tx.Ticket || block.PID != pid ||
		block.StartVA != uint64(ptr) || block.Size != 64*mem.KB {
		t.Errorf("BlockRange = %+v, want commandID=%d pid=%d va=%x size=64KB",
			block, tx.Ticket, pid, uint64(ptr))
	}

	// Pre-watermark old-remote reads complete: the CPU backing must not be
	// mutated while the block is in flight.
	for i := uint64(0); i < reg.PageCount; i++ {
		got, _ := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		if !allZero(got) {
			t.Fatal("CPU backing mutated before block completion")
		}
	}

	deliverGeneralRsp(t, d, block)
	mw.Tick()

	// Data phase: every page is CPU-backed (no residency), so the copy writes
	// the CPU backing directly; no flush, no DMA, no global flush.
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests after block = %d, want 1 UnblockRange", len(reqs))
	}
	if _, ok := reqs[0].(*vm.UnblockRange); !ok {
		t.Fatalf("request type = %T, want *vm.UnblockRange", reqs[0])
	}
	if tx.phase != copyPhaseUnblocking {
		t.Errorf("phase = %v, want unblocking", tx.phase)
	}
	for _, key := range tx.Keys {
		if !d.uvm.IsKeyIdle(key) {
			t.Errorf("key %+v still owned after atomic release", key)
		}
	}

	// The CPU backing now holds the exact source bytes, one page each.
	for i := uint64(0); i < reg.PageCount; i++ {
		got, _ := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		want := src.Data[i*basePageSize : (i+1)*basePageSize]
		if !bytes.Equal(got, want) {
			t.Fatalf("CPU backing page %d mismatch", i)
		}
	}

	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if q.NumCommand() != 0 {
		t.Error("command not dequeued after completion")
	}
	if q.IsRunning {
		t.Error("queue still running after completion")
	}
	if tx.phase != copyPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	for w := range reg.ResidentMask {
		if reg.ResidentMask[w] != 0 || reg.InFlightMask[w] != 0 || reg.DirtyMask[w] != 0 {
			t.Error("residency masks mutated by the copy")
		}
	}
	for _, block := range reg.VABlocks {
		for _, sub := range block.SubBlocks {
			if sub.AccessCounter != 0 {
				t.Error("access counter mutated by the copy")
			}
		}
	}
}

// TestManagedD2H drives a full managed D2H readback over CPU_REMOTE pages:
// the exact CPU backing bytes reach the host destination and the queue
// completes.
func TestManagedD2H(t *testing.T) {
	d, mw := buildCopyDriver(t, true)
	ctx := d.Init()
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	want := make([]byte, 64*mem.KB)
	for i := range want {
		want[i] = byte(i*13 + 1)
	}
	for i := uint64(0); i < reg.PageCount; i++ {
		d.globalStorage.Write(reg.CPUBackingPages[i], want[i*basePageSize:(i+1)*basePageSize])
	}

	dst := &payload64K{}
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyD2HCommand{ID: "managed-d2h", Dst: dst, Src: ptr}
	q.Enqueue(cmd)

	processed, handled := mw.tryProcess(cmd, q)
	if !processed || !handled {
		t.Fatalf("tryProcess = %v/%v, want true/true", processed, handled)
	}

	tx := mw.copies[0]
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 BlockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests after block = %d, want 1 UnblockRange", len(reqs))
	}
	if tx.phase != copyPhaseUnblocking {
		t.Errorf("phase = %v, want unblocking", tx.phase)
	}

	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if tx.phase != copyPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	if q.NumCommand() != 0 {
		t.Error("command not dequeued after completion")
	}
	if !bytes.Equal(dst.Data[:], want) {
		t.Error("D2H readback mismatch")
	}
}

// TestManagedCopyTransition drives the ownership/phase transitions of two
// overlapping copies: one global monotonic ticket per copy, all-or-none claim,
// enqueue-once by ticket, and claim after release.
func TestManagedCopyTransition(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	a := d.AllocateManagedMemory(ctx, 64*mem.KB) // keys {0, 1}
	b := d.AllocateManagedMemory(ctx, 64*mem.KB) // keys {1, 2}

	qa := d.CreateCommandQueue(ctx)
	cmdA := &MemCopyH2DCommand{ID: "copy-a", Dst: a, Src: payload64K{}}
	qa.Enqueue(cmdA)
	mw.tryProcess(cmdA, qa)

	qb := d.CreateCommandQueue(ctx)
	cmdB := &MemCopyH2DCommand{ID: "copy-b", Dst: b, Src: payload64K{}}
	qb.Enqueue(cmdB)
	mw.tryProcess(cmdB, qb)

	txA := mw.copies[0]
	txB := mw.copies[1]
	if txA.Ticket >= txB.Ticket {
		t.Errorf("tickets not monotonic: %d >= %d", txA.Ticket, txB.Ticket)
	}
	if !txA.claimed || txB.claimed {
		t.Fatalf("claimed = %v/%v, want true/false", txA.claimed, txB.claimed)
	}
	for _, key := range txA.Keys {
		typ, owner := d.uvm.OwnerOf(key)
		if typ != OwnershipCopy || owner != txA.Ticket {
			t.Errorf("A does not own key %+v", key)
		}
	}
	for _, key := range txB.Keys {
		typ, owner := d.uvm.OwnerOf(key)
		if typ == OwnershipCopy && owner == txB.Ticket {
			t.Errorf("waiting copy B owns key %+v", key)
		}
	}
	if len(d.uvm.copyWaiters) != 1 || d.uvm.copyWaiters[0] != txB {
		t.Error("B not enqueued exactly once by ticket")
	}

	// Complete A: block -> data -> unblock -> release -> wake.
	reqs := drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()
	reqs = drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if !txB.claimed {
		t.Fatal("B not claimed after A released")
	}
	if len(d.uvm.copyWaiters) != 0 {
		t.Error("B still waiting after claim")
	}
	for _, key := range txB.Keys {
		typ, owner := d.uvm.OwnerOf(key)
		if typ != OwnershipCopy || owner != txB.Ticket {
			t.Errorf("B does not own key %+v after claim", key)
		}
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("B requests = %d, want 1 BlockRange", len(reqs))
	}
	if _, ok := reqs[0].(*vm.BlockRange); !ok {
		t.Fatalf("B request = %T, want BlockRange", reqs[0])
	}
}

// TestManagedCopyReject verifies span classification: fully managed spans take
// the UVM path, fully unmanaged spans fall through, and mixed/gapped spans are
// rejected.
func TestManagedCopyReject(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	managed := d.AllocateManagedMemory(ctx, 64*mem.KB) // [4096, 69632)
	unmanaged := d.AllocateMemory(ctx, 64*mem.KB)      // [69632, 135168)

	allManaged, err := d.uvm.classifySpan(pid, uint64(managed), 64*mem.KB)
	if err != nil || !allManaged {
		t.Fatalf("managed span: %v/%v, want true/nil", allManaged, err)
	}
	allManaged, err = d.uvm.classifySpan(pid, uint64(unmanaged), 64*mem.KB)
	if err != nil || allManaged {
		t.Fatalf("unmanaged span: %v/%v, want false/nil", allManaged, err)
	}
	if _, err = d.uvm.classifySpan(pid, uint64(managed), 128*mem.KB); err == nil {
		t.Error("mixed span not rejected")
	}

	// Gapped span: two managed registrations with a VA hole between them.
	m := NewUVMManager(DefaultUVMConfig(), 4*mem.GB)
	g1 := buildTestRegistration(pid, 4096, 16)  // [4096, 69632)
	g2 := buildTestRegistration(pid, 81920, 16) // [81920, 147456)
	m.registrations = append(m.registrations, g1, g2)
	if _, err = m.classifySpan(pid, 4096, 147456-4096); err == nil {
		t.Error("gapped span not rejected")
	}

	// A mixed-span copy panics at the branch.
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "mixed", Dst: managed, Src: payload128K{}}
	q.Enqueue(cmd)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("mixed-span copy did not panic")
			}
		}()
		mw.tryProcess(cmd, q)
	}()
}

// TestManagedDirtyGPUOverwrite drives an H2D copy over a GPU-resident region:
// WB+INV flush before the HBM overwrite, exact mask/runs, and residency
// preserved.
func TestManagedDirtyGPUOverwrite(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]
	block := reg.VABlocks[0]
	region := block.SubBlocks[0]

	// Region 0 of this allocation covers allocation pages 0..14 (VA
	// 4096..65536); make it GPU-resident through the Todo 4 fixture.
	makeRegionGPUResident(t, d, reg, 0, 0)
	if region.State != RegionGPUResident {
		t.Fatalf("region state = %s, want GPU_RESIDENT", region.State)
	}

	src := payload64K{}
	for i := range src.Data {
		src.Data[i] = byte(i)
	}
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "dirty-h2d", Dst: ptr, Src: src}
	q.Enqueue(cmd)
	mw.tryProcess(cmd, q)

	tx := mw.copies[0]
	reqs := drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	// Flush phase: exactly one WB+INV request for region 0 with the 15
	// resident pages (mask bits 1..15: allocation page i sits at VA
	// 4096+i*4KB, i.e. region-local page i+1) mapped to one contiguous HBM
	// run.
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("flush reqs = %d, want 1", len(reqs))
	}
	flush, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("request = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	if flush.Operation != cache.UVMCacheRangeFlushWritebackInvalidate {
		t.Errorf("op = %v, want WRITEBACK_INVALIDATE", flush.Operation)
	}
	if flush.VABase != 0 || flush.ValidPageMask != 0xFFFE {
		t.Errorf("flush = %#x/%#x, want VABase 0 mask 0xFFFE",
			flush.VABase, flush.ValidPageMask)
	}
	if len(flush.PhysicalRuns) != 1 ||
		flush.PhysicalRuns[0].Start != 0x2_0000_0000 ||
		flush.PhysicalRuns[0].Length != 15*basePageSize {
		t.Errorf("runs = %+v, want one 60KB run at 0x20000000", flush.PhysicalRuns)
	}
	if tx.phase != copyPhaseFlushing {
		t.Errorf("phase = %v, want flushing", tx.phase)
	}
	if len(tx.dmaReqs) != 0 {
		t.Error("HBM overwrite issued before the flush completed")
	}

	deliverFlushRsp(t, d, flush)
	mw.Tick()

	// Data phase: 15 HBM overwrites (resident pages) + 1 CPU backing write
	// (page 15 of the span is not resident).
	reqs = drainRequests(d)
	if len(reqs) != 15 {
		t.Fatalf("DMA reqs = %d, want 15", len(reqs))
	}
	for i, req := range reqs {
		h2d, ok := req.(*protocol.MemCopyH2DReq)
		if !ok {
			t.Fatalf("req %d = %T, want MemCopyH2DReq", i, req)
		}
		wantPA := 0x2_0000_0000 + uint64(i)*basePageSize
		if h2d.DstAddress != wantPA {
			t.Errorf("DMA %d dst = %#x, want %#x", i, h2d.DstAddress, wantPA)
		}
		want := src.Data[i*basePageSize : (i+1)*basePageSize]
		if !bytes.Equal(h2d.SrcBuffer, want) {
			t.Errorf("DMA %d payload mismatch", i)
		}
	}
	// Page 15 (VA 65536) is CPU-backed: its bytes land in the CPU backing.
	got, _ := d.globalStorage.Read(reg.CPUBackingPages[15], basePageSize)
	want := src.Data[15*basePageSize : 16*basePageSize]
	if !bytes.Equal(got, want) {
		t.Error("CPU backing page 15 mismatch")
	}

	for _, req := range reqs {
		deliverGeneralRsp(t, d, req)
	}
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA reqs = %d, want 1 UnblockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if tx.phase != copyPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	for i := uint64(0); i < pagesPerSubBlock; i++ {
		if maskBit(reg.ResidentMask, i) != (i < 15) {
			t.Errorf("residency changed for page %d", i)
		}
	}
	if region.State != RegionGPUResident {
		t.Errorf("region state = %s, want GPU_RESIDENT preserved", region.State)
	}
	if d.uvm.Reservation().ResidentBytes() != 64*mem.KB {
		t.Errorf("reservation R = %d, want 64KB preserved",
			d.uvm.Reservation().ResidentBytes())
	}
}

// TestManagedDirtyGPUReadback drives a D2H readback over a GPU-resident
// region: writeback-only flush, then HBM reads; dirty cache data reaches HBM
// before the readback.
func TestManagedDirtyGPUReadback(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]
	makeRegionGPUResident(t, d, reg, 0, 0)

	// CPU backing for the non-resident page 15.
	cpuData := make([]byte, basePageSize)
	for i := range cpuData {
		cpuData[i] = 0xAB
	}
	d.globalStorage.Write(reg.CPUBackingPages[15], cpuData)

	dst := &payload64K{}
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyD2HCommand{ID: "dirty-d2h", Dst: dst, Src: ptr}
	q.Enqueue(cmd)
	mw.tryProcess(cmd, q)

	tx := mw.copies[0]
	reqs := drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	// Flush phase: exactly one writeback-only request for region 0.
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("flush reqs = %d, want 1", len(reqs))
	}
	flush, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("request = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	if flush.Operation != cache.UVMCacheRangeFlushWritebackOnly {
		t.Errorf("op = %v, want WRITEBACK_ONLY", flush.Operation)
	}
	if flush.VABase != 0 || flush.ValidPageMask != 0xFFFE {
		t.Errorf("flush = %#x/%#x, want VABase 0 mask 0xFFFE",
			flush.VABase, flush.ValidPageMask)
	}
	if len(flush.PhysicalRuns) != 1 ||
		flush.PhysicalRuns[0].Start != 0x2_0000_0000 ||
		flush.PhysicalRuns[0].Length != 15*basePageSize {
		t.Errorf("runs = %+v, want one 60KB run at 0x20000000", flush.PhysicalRuns)
	}
	if len(tx.dmaReqs) != 0 {
		t.Error("HBM read issued before the writeback completed")
	}

	deliverFlushRsp(t, d, flush)
	mw.Tick()

	// Data phase: 15 HBM reads (resident pages) + 1 CPU backing read.
	reqs = drainRequests(d)
	if len(reqs) != 15 {
		t.Fatalf("DMA reqs = %d, want 15", len(reqs))
	}
	for i, req := range reqs {
		d2h, ok := req.(*protocol.MemCopyD2HReq)
		if !ok {
			t.Fatalf("req %d = %T, want MemCopyD2HReq", i, req)
		}
		wantPA := 0x2_0000_0000 + uint64(i)*basePageSize
		if d2h.SrcAddress != wantPA {
			t.Errorf("DMA %d src = %#x, want %#x", i, d2h.SrcAddress, wantPA)
		}
		// Simulate the DMA engine filling the destination buffer.
		for j := range d2h.DstBuffer {
			d2h.DstBuffer[j] = byte(i*31 + j)
		}
	}
	for _, req := range reqs {
		deliverGeneralRsp(t, d, req)
	}
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA reqs = %d, want 1 UnblockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if tx.phase != copyPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	for i := 0; i < 15; i++ {
		for j := 0; j < basePageSize; j++ {
			want := byte(i*31 + j)
			if dst.Data[i*basePageSize+j] != want {
				t.Fatalf("HBM readback page %d byte %d = %#x, want %#x",
					i, j, dst.Data[i*basePageSize+j], want)
			}
		}
	}
	if !bytes.Equal(dst.Data[15*basePageSize:16*basePageSize], cpuData) {
		t.Error("CPU backing readback page 15 mismatch")
	}
}

// TestManagedCopyBlockBarrier verifies the block barrier: no data mutation
// before the block completes, and the unblock only after the data phase.
func TestManagedCopyBlockBarrier(t *testing.T) {
	d, mw := buildCopyDriver(t, true)
	ctx := d.Init()
	ptr := d.AllocateManagedMemory(ctx, 16*mem.KB)
	reg := d.uvm.registrations[0]

	src := payload16K{}
	for i := range src.Data {
		src.Data[i] = byte(i)
	}
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "barrier", Dst: ptr, Src: src}
	q.Enqueue(cmd)
	mw.tryProcess(cmd, q)

	tx := mw.copies[0]
	if tx.phase != copyPhaseBlocking {
		t.Fatalf("phase = %v, want blocking", tx.phase)
	}
	for i := uint64(0); i < reg.PageCount; i++ {
		got, _ := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		if !allZero(got) {
			t.Fatal("data mutated before block completion")
		}
	}

	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 BlockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if tx.phase != copyPhaseUnblocking {
		t.Errorf("phase = %v, want unblocking after data", tx.phase)
	}
	for i := uint64(0); i < reg.PageCount; i++ {
		got, _ := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		want := src.Data[i*basePageSize : (i+1)*basePageSize]
		if !bytes.Equal(got, want) {
			t.Fatalf("CPU backing page %d mismatch after block", i)
		}
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-data requests = %d, want 1 UnblockRange", len(reqs))
	}
	if _, ok := reqs[0].(*vm.UnblockRange); !ok {
		t.Fatalf("post-data request = %T, want UnblockRange", reqs[0])
	}
	for _, key := range tx.Keys {
		if !d.uvm.IsKeyIdle(key) {
			t.Error("keys not released after the data phase")
		}
	}
}

// TestManagedRemoteReadH2DRace verifies the H2D race contract: while the block
// is in flight the pre-watermark old-remote reads complete against the
// untouched CPU backing, and the CPU mutation happens only after the watermark
// acks; later faults are retained by the block (no mutation before it).
func TestManagedRemoteReadH2DRace(t *testing.T) {
	d, mw := buildCopyDriver(t, true)
	ctx := d.Init()
	ptr := d.AllocateManagedMemory(ctx, 16*mem.KB)
	reg := d.uvm.registrations[0]

	src := payload16K{}
	for i := range src.Data {
		src.Data[i] = byte(0x5A)
	}
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "race", Dst: ptr, Src: src}
	q.Enqueue(cmd)
	mw.tryProcess(cmd, q)

	tx := mw.copies[0]
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 BlockRange", len(reqs))
	}
	block := reqs[0].(*vm.BlockRange)

	// Old-remote reads (pre-watermark) still see the untouched CPU backing.
	for i := uint64(0); i < reg.PageCount; i++ {
		got, _ := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		if !allZero(got) {
			t.Fatal("CPU backing mutated while old-remote reads are in flight")
		}
	}
	if len(tx.flushReqs) != 0 || len(tx.dmaReqs) != 0 {
		t.Error("data path entered before the block completed")
	}

	// Watermark acks arrive: the block completes and the CPU mutation happens.
	deliverGeneralRsp(t, d, block)
	mw.Tick()

	for i := uint64(0); i < reg.PageCount; i++ {
		got, _ := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		want := src.Data[i*basePageSize : (i+1)*basePageSize]
		if !bytes.Equal(got, want) {
			t.Fatalf("CPU backing page %d not mutated after watermark acks", i)
		}
	}
	// CPU_REMOTE data is non-cacheable: no cache flush is ever issued.
	reqs = drainRequests(d)
	for _, req := range reqs {
		if _, ok := req.(*protocol.UVMCacheRangeFlushReq); ok {
			t.Error("cache flush issued for CPU_REMOTE pages")
		}
	}
}

// TestManagedAtomicMultiRegionClaim verifies the all-or-none multi-region
// claim: a copy spanning two regions claims both or none, and a waiting copy
// claims its whole set only after every key is free.
func TestManagedAtomicMultiRegionClaim(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	a := d.AllocateManagedMemory(ctx, 64*mem.KB) // keys {0, 1}
	b := d.AllocateManagedMemory(ctx, 64*mem.KB) // keys {1, 2}

	qa := d.CreateCommandQueue(ctx)
	cmdA := &MemCopyH2DCommand{ID: "a", Dst: a, Src: payload64K{}}
	qa.Enqueue(cmdA)
	mw.tryProcess(cmdA, qa)
	txA := mw.copies[0]

	qb := d.CreateCommandQueue(ctx)
	cmdB := &MemCopyH2DCommand{ID: "b", Dst: b, Src: payload64K{}}
	qb.Enqueue(cmdB)
	mw.tryProcess(cmdB, qb)
	txB := mw.copies[1]

	if len(txA.Keys) != 2 || len(txB.Keys) != 2 {
		t.Fatalf("key counts = %d/%d, want 2/2", len(txA.Keys), len(txB.Keys))
	}
	for _, key := range txA.Keys {
		typ, owner := d.uvm.OwnerOf(key)
		if typ != OwnershipCopy || owner != txA.Ticket {
			t.Errorf("A missing key %+v", key)
		}
	}
	for _, key := range txB.Keys {
		typ, owner := d.uvm.OwnerOf(key)
		if typ == OwnershipCopy && owner == txB.Ticket {
			t.Errorf("B holds key %+v while waiting", key)
		}
	}
	if len(d.uvm.copyWaiters) != 1 {
		t.Fatalf("waiters = %d, want 1", len(d.uvm.copyWaiters))
	}

	reqs := drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()
	reqs = drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if !txB.claimed {
		t.Fatal("B not claimed after A released")
	}
	for _, key := range txB.Keys {
		typ, owner := d.uvm.OwnerOf(key)
		if typ != OwnershipCopy || owner != txB.Ticket {
			t.Errorf("B missing key %+v after claim", key)
		}
	}
	if len(d.uvm.copyWaiters) != 0 {
		t.Error("B still waiting after claim")
	}
}

// TestManagedInverseOverlapNoDeadlock runs two copies with inverse region
// overlap over a GPU-resident region (Todo 4 fixture): they finish in ticket
// order without deadlock, a waiting copy holds no key, and the region state
// and reservation survive.
func TestManagedInverseOverlapNoDeadlock(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	a := d.AllocateManagedMemory(ctx, 64*mem.KB) // [4096, 69632): keys {0, 1}
	b := d.AllocateManagedMemory(ctx, 64*mem.KB) // [69632, 135168): keys {1, 2}

	// Region 1 of allocation B (all 16 pages) is GPU-resident with a
	// committed reservation; both copies overlap it.
	regB := d.uvm.registrations[1]
	makeRegionGPUResident(t, d, regB, 0, 1)
	region := regB.VABlocks[0].SubBlocks[1]

	qa := d.CreateCommandQueue(ctx)
	cmdA := &MemCopyH2DCommand{ID: "a", Dst: a, Src: payload64K{}}
	qa.Enqueue(cmdA)
	mw.tryProcess(cmdA, qa)
	txA := mw.copies[0]

	qb := d.CreateCommandQueue(ctx)
	cmdB := &MemCopyH2DCommand{ID: "b", Dst: b, Src: payload64K{}}
	qb.Enqueue(cmdB)
	mw.tryProcess(cmdB, qb)
	txB := mw.copies[1]

	if !txA.claimed || txB.claimed {
		t.Fatalf("claimed = %v/%v, want true/false", txA.claimed, txB.claimed)
	}
	if txA.Ticket >= txB.Ticket {
		t.Error("ticket order violated")
	}

	// Finish A (all CPU-backed pages: no flush, no DMA).
	reqs := drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("A post-block reqs = %d, want 1 UnblockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if !txB.claimed {
		t.Fatal("B did not claim after A finished")
	}

	// Finish B: block -> flush (region 1, 15 resident pages) -> DMA -> unblock.
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("B reqs = %d, want 1 BlockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("B flush reqs = %d, want 1", len(reqs))
	}
	flush, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("B flush = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	// B's allocation starts at VA 69632, so region 1 covers allocation pages
	// 0..14 at region-local bits 1..15.
	if flush.VABase != 64*mem.KB || flush.ValidPageMask != 0xFFFE {
		t.Errorf("B flush = %#x/%#x, want VABase 64KB mask 0xFFFE",
			flush.VABase, flush.ValidPageMask)
	}
	deliverFlushRsp(t, d, flush)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 15 {
		t.Fatalf("B DMA reqs = %d, want 15", len(reqs))
	}
	for _, req := range reqs {
		deliverGeneralRsp(t, d, req)
	}
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("B post-DMA reqs = %d, want 1 UnblockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()

	if txA.phase != copyPhaseDone || txB.phase != copyPhaseDone {
		t.Errorf("phases = %v/%v, want done/done", txA.phase, txB.phase)
	}
	if region.State != RegionGPUResident {
		t.Errorf("region state = %s, want GPU_RESIDENT preserved", region.State)
	}
	if d.uvm.Reservation().ResidentBytes() != 64*mem.KB {
		t.Errorf("reservation R = %d, want 64KB preserved",
			d.uvm.Reservation().ResidentBytes())
	}
}

// TestManagedGenericTransitionOwnership proves the ownership table is shared:
// a generic transition (fault-like holder) blocks a copy, the copy enqueues
// once and holds no key, and after the generic release the copy claims; a
// generic acquire on a COPY-owned key fails.
func TestManagedGenericTransitionOwnership(t *testing.T) {
	d, mw := buildCopyDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 16*mem.KB)
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}

	if !d.uvm.AcquireOwnership(key, OwnershipFault, 42) {
		t.Fatal("generic acquire failed on an idle key")
	}
	typ, owner := d.uvm.OwnerOf(key)
	if typ != OwnershipFault || owner != 42 {
		t.Errorf("owner = %v/%d, want FAULT/42", typ, owner)
	}

	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "wait", Dst: ptr, Src: payload16K{}}
	q.Enqueue(cmd)
	mw.tryProcess(cmd, q)
	tx := mw.copies[0]

	if tx.claimed {
		t.Fatal("copy claimed a key owned by a generic transition")
	}
	if len(d.uvm.copyWaiters) != 1 {
		t.Fatalf("waiters = %d, want 1", len(d.uvm.copyWaiters))
	}
	for _, k := range tx.Keys {
		typ, owner := d.uvm.OwnerOf(k)
		if typ == OwnershipCopy && owner == tx.Ticket {
			t.Errorf("waiting copy holds key %+v", k)
		}
	}
	if len(d.requestsToSend) != 0 {
		t.Error("block sent while waiting for a generic transition")
	}

	d.uvm.ReleaseOwnership(key, 42)
	d.uvm.wakeTickets()

	if !tx.claimed {
		t.Fatal("copy did not claim after the generic release")
	}
	typ, owner = d.uvm.OwnerOf(key)
	if typ != OwnershipCopy || owner != tx.Ticket {
		t.Errorf("owner = %v/%d, want COPY/%d", typ, owner, tx.Ticket)
	}
	if len(d.uvm.copyWaiters) != 0 {
		t.Error("copy still waiting after claim")
	}

	if d.uvm.AcquireOwnership(key, OwnershipEviction, 7) {
		t.Error("generic acquire succeeded on a COPY-owned key")
	}
}
