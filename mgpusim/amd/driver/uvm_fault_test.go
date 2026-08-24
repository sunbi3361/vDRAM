package driver

import (
	"bytes"
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: FIFO fault service, 64 KB coalescing, and scheduled software
// latency contract tests (todo 15 of plan mgpusim-uvm-manager). The QA regex
// 'TestUVMFault(FIFOABA|CoalescedWaiters|SoftwareLatency|QueuedSatisfiedByTBN|
// QueuedPartiallySatisfiedByTBN|CopyFaultOwnershipRace|DuplicateCompletion|
// IllegalState)' runs the fixtures in this file: strict FIFO with one active
// transaction, exact raw/unique/coalesced/latency totals, queued-B overlap
// (prefetched-satisfied -> zero DMA/PTE/TLB work + one replay; partial ->
// missing-page-only DMA), copy/fault ownership races, one replay per waiter,
// and no double accounting.

// buildFaultDriver builds a real driver (real allocator, real CPU + GPU page
// tables, real UVM manager, host storage) with the FIFO fault service wired,
// plus a registered GPU port. ideal selects -uvm-ideal (zero latency).
func buildFaultDriver(t *testing.T, ideal bool) (
	*Driver, *faultServiceMiddleware, []vm.PageTable,
) {
	t.Helper()

	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	cfg.Ideal = ideal

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

	return d, d.uvmFault, gpuTables
}

// intakeFault delivers one raw PageFaultReq through the driver's GPU port and
// consumes it via the fault intake seam.
func intakeFault(t *testing.T, d *Driver, pid vm.PID, gpu int, vaddr uint64) {
	t.Helper()

	req := protocol.PageFaultReqBuilder{}.
		WithSrc(d.gpuPort.AsRemote()).
		WithDst(d.gpuPort.AsRemote()).
		WithPID(pid).
		WithGPU(gpu).
		WithVAddr(vaddr).
		WithAccessType(vm.AccessKindRead).
		WithFaultPendingToken(vm.FaultPendingToken(1)).
		Build()
	if err := d.gpuPort.Deliver(req); err != nil {
		t.Fatalf("Deliver PageFaultReq: %v", err)
	}
	if !d.processReturnReq() {
		t.Fatalf("processReturnReq did not consume the fault at %#x", vaddr)
	}
}

// deliverReplayAck injects the CP-style replay completion for a replay request.
func deliverReplayAck(t *testing.T, d *Driver, req *protocol.UVMFaultReplayReq) {
	t.Helper()

	rsp := protocol.UVMFaultReplayRspBuilder{}.
		WithSrc(d.gpuPort.AsRemote()).
		WithDst(d.gpuPort.AsRemote()).
		WithRspTo(req.ID).
		Build()
	if err := d.gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver replay ack: %v", err)
	}
}

// deliverTLBAck injects the CP-style TLB-invalidation completion.
func deliverTLBAck(t *testing.T, d *Driver, req *protocol.UVMTLBInvalidateReq) {
	t.Helper()

	rsp := protocol.UVMTLBInvalidateRspBuilder{}.
		WithSrc(d.gpuPort.AsRemote()).
		WithDst(d.gpuPort.AsRemote()).
		WithRspTo(req.ID).
		Build()
	if err := d.gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver TLB ack: %v", err)
	}
}

// makeRegionResident transitions a region to GPU_RESIDENT from its current
// state (IDLE/CPU_RESIDENT/FAULT_PENDING) and publishes the resident bits,
// HBM PAs, and a committed capacity reservation. It does NOT publish GPU PTEs,
// so tests can observe the service's PTE work separately.
func makeRegionResident(
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
	for region.State != RegionGPUResident {
		switch region.State {
		case RegionIDLE, RegionCPUResident:
			if err := sm.Transition(RegionFaultPending, now); err != nil {
				t.Fatalf("Transition(%s): %v", region.State, err)
			}
		case RegionFaultPending:
			if err := sm.Transition(RegionMigratingToGPU, now); err != nil {
				t.Fatalf("Transition(%s): %v", region.State, err)
			}
		case RegionMigratingToGPU:
			if err := sm.Transition(RegionGPUResident, now); err != nil {
				t.Fatalf("Transition(%s): %v", region.State, err)
			}
		default:
			t.Fatalf("cannot reach GPU_RESIDENT from %s", region.State)
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

// TestUVMFaultFIFOABA drives strict FIFO with one active transaction: A's
// transaction is active while B waits; duplicate faults coalesce into the
// active/queued transaction; B starts only after A completes; both complete
// with exactly one replay each.
func TestUVMFaultFIFOABA(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg := d.uvm.registrations[0]

	// A1, A2 -> region 0; B1 -> region 1; A3 -> region 0 (coalesced).
	intakeFault(t, d, pid, 1, uint64(ptr))
	intakeFault(t, d, pid, 1, uint64(ptr)+basePageSize)
	intakeFault(t, d, pid, 1, uint64(ptr)+64*mem.KB)
	intakeFault(t, d, pid, 1, uint64(ptr)+2*basePageSize)

	if got := d.uvm.RawPageFaultCount(); got != 4 {
		t.Errorf("raw count = %d, want 4", got)
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 2 {
		t.Errorf("unique count = %d, want 2", got)
	}
	if got := d.uvm.CoalescedPageFaultCount(); got != 2 {
		t.Errorf("coalesced count = %d, want 2", got)
	}
	if len(mw.queue) != 2 {
		t.Fatalf("queue = %d transactions, want 2", len(mw.queue))
	}
	txA, txB := mw.queue[0], mw.queue[1]
	if txA.RegionBase != 0 || txB.RegionBase != 64*mem.KB {
		t.Errorf("FIFO order = %#x, %#x; want region 0 then region 64KB",
			txA.RegionBase, txB.RegionBase)
	}
	if mw.active != nil {
		t.Fatal("transaction active before any tick")
	}

	// A's demand becomes resident while A is queued (TBN effect).
	makeRegionResident(t, d, reg, 0, 0)

	if !mw.Tick() {
		t.Fatal("tick did not start the FIFO head")
	}
	if mw.active != txA {
		t.Fatal("A not active")
	}
	if len(mw.queue) != 1 || mw.queue[0] != txB {
		t.Fatal("B not queued behind A")
	}
	if txA.phase != faultPhaseLatency || txA.latencyEvent == nil {
		t.Error("A latency not scheduled")
	}
	if txB.phase != faultPhaseQueued || txB.latencyEvent != nil {
		t.Error("B must not charge latency while queued")
	}

	// Complete A: one latency, zero DMA/PTE/TLB work, one replay, then done.
	mw.Handle(txA.latencyEvent)
	mw.Tick()
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("A service requests = %d, want 1 replay", len(reqs))
	}
	replayA, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("A service request = %T, want UVMFaultReplayReq", reqs[0])
	}
	if replayA.StartVA != 0 || replayA.Size != 64*mem.KB {
		t.Errorf("A replay = %#x+%d, want 0+64KB", replayA.StartVA, replayA.Size)
	}
	deliverReplayAck(t, d, replayA)
	mw.Tick()

	if txA.phase != faultPhaseDone {
		t.Errorf("A phase = %v, want done", txA.phase)
	}
	if mw.active != txB {
		t.Fatal("B did not become active after A completed")
	}
	if txB.phase != faultPhaseLatency || txB.latencyEvent == nil {
		t.Error("B latency not scheduled at head")
	}

	// B's demand becomes resident while B is active (queued-B overlap).
	makeRegionResident(t, d, reg, 0, 1)

	mw.Handle(txB.latencyEvent)
	mw.Tick()
	reqs = drainRequests(d)
	// B's demand is fully local, but the TBN occupancy (31 of 32 valid
	// pages) expands to the 2 MB block and prefetches the one remaining
	// page 31 (uvm-manager.md §11.6-style expansion). // sbin_codex (todo 17)
	if len(reqs) != 1 {
		t.Fatalf("B service requests = %d, want 1 DMA (TBN prefetch of page 31)",
			len(reqs))
	}
	h2dB, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("B service request = %T, want MemCopyH2DReq", reqs[0])
	}
	if len(h2dB.SrcBuffer) != basePageSize {
		t.Errorf("B DMA bytes = %d, want 1 page", len(h2dB.SrcBuffer))
	}
	deliverGeneralRsp(t, d, h2dB)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbB, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, tlbB)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replayB, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replayB)
	mw.Tick()

	if txB.phase != faultPhaseDone {
		t.Errorf("B phase = %v, want done", txB.phase)
	}
	if mw.active != nil || len(mw.queue) != 0 {
		t.Error("queue not drained after both transactions completed")
	}
}

// TestUVMFaultCoalescedWaiters verifies the raw/unique/coalesced equations and
// the exact waiter/demand-page sets of each transaction.
func TestUVMFaultCoalescedWaiters(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)

	intakeFault(t, d, pid, 1, uint64(ptr))                        // A1: region 0
	intakeFault(t, d, pid, 1, uint64(ptr)+basePageSize)           // A2: coalesced
	intakeFault(t, d, pid, 1, uint64(ptr)+64*mem.KB-basePageSize) // B1: region 1
	intakeFault(t, d, pid, 1, uint64(ptr)+2*basePageSize)         // A3: coalesced

	if got := d.uvm.RawPageFaultCount(); got != 4 {
		t.Errorf("raw = %d, want 4", got)
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 2 {
		t.Errorf("unique = %d, want 2", got)
	}
	if got := d.uvm.CoalescedPageFaultCount(); got != 2 {
		t.Errorf("coalesced = %d, want 2", got)
	}
	if len(mw.queue) != 2 {
		t.Fatalf("queue = %d, want 2", len(mw.queue))
	}

	txA, txB := mw.queue[0], mw.queue[1]
	if len(txA.Waiters) != 3 {
		t.Fatalf("A waiters = %d, want 3", len(txA.Waiters))
	}
	for i, want := range []uint64{
		uint64(ptr), uint64(ptr) + basePageSize, uint64(ptr) + 2*basePageSize,
	} {
		if txA.Waiters[i].VAddr != want {
			t.Errorf("A waiter %d VA = %#x, want %#x", i, txA.Waiters[i].VAddr, want)
		}
	}
	if len(txB.Waiters) != 1 || txB.Waiters[0].VAddr != uint64(ptr)+64*mem.KB-basePageSize {
		t.Errorf("B waiters = %+v, want exactly the single B1 fault", txB.Waiters)
	}

	// The 64 KB allocation [4096, 69632) covers region 0 (15 valid pages,
	// allocation pages 0..14) and region 1 (1 page, allocation page 15).
	if len(txA.DemandPages) != 15 || txA.DemandPages[0] != 0 ||
		txA.DemandPages[14] != 14 {
		t.Errorf("A demand pages = %v, want allocation pages 0..14",
			txA.DemandPages)
	}
	if len(txB.DemandPages) != 1 || txB.DemandPages[0] != 15 {
		t.Errorf("B demand pages = %v, want allocation page 15", txB.DemandPages)
	}
}

// TestUVMFaultSoftwareLatency verifies the latency is charged exactly once
// per unique transaction, as a scheduled event of the configured duration,
// non-overlapping (one active at a time), and zero under -uvm-ideal.
func TestUVMFaultSoftwareLatency(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg := d.uvm.registrations[0]

	intakeFault(t, d, pid, 1, uint64(ptr))
	intakeFault(t, d, pid, 1, uint64(ptr)+64*mem.KB)
	txA, txB := mw.queue[0], mw.queue[1]

	if !mw.Tick() {
		t.Fatal("tick did not start A")
	}
	want := sim.VTimeInSec(float64(20*time.Microsecond) / float64(time.Second))
	if txA.latencyEvent == nil || txA.latencyEvent.Time() != want {
		t.Errorf("A latency event = %v, want %v", txA.latencyEvent, want)
	}
	if mw.latencyChargedCount != 1 {
		t.Errorf("latency charges = %d, want 1", mw.latencyChargedCount)
	}
	if txB.latencyEvent != nil {
		t.Error("B latency charged while queued")
	}

	// Complete A; B then charges its own latency.
	makeRegionResident(t, d, reg, 0, 0)
	mw.Handle(txA.latencyEvent)
	mw.Tick()
	reqs := drainRequests(d)
	deliverReplayAck(t, d, reqs[0].(*protocol.UVMFaultReplayReq))
	mw.Tick()

	if mw.active != txB {
		t.Fatal("B not active after A completed")
	}
	if txB.latencyEvent == nil || txB.latencyEvent.Time() != want {
		t.Errorf("B latency event = %v, want %v", txB.latencyEvent, want)
	}
	if txB.latencyEvent.ID == txA.latencyEvent.ID {
		t.Error("A and B share one latency event")
	}
	if mw.latencyChargedCount != 2 {
		t.Errorf("latency charges = %d, want 2 (one per unique transaction)",
			mw.latencyChargedCount)
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 2 {
		t.Errorf("unique count = %d, want 2", got)
	}

	// Ideal mode: the transaction still exists and is accounted, but the
	// scheduled latency is zero.
	d2, mw2, _ := buildFaultDriver(t, true)
	ctx2 := d2.Init()
	ptr2 := d2.AllocateManagedMemory(ctx2, 16*mem.KB)
	intakeFault(t, d2, ctx2.pid, 1, uint64(ptr2))
	mw2.Tick()
	tx := mw2.active
	if tx == nil {
		t.Fatal("no active transaction in ideal mode")
	}
	if tx.latencyEvent == nil || tx.latencyEvent.Time() != 0 {
		t.Errorf("ideal latency event = %v, want time 0", tx.latencyEvent)
	}
}

// TestUVMFaultQueuedSatisfiedByTBN proves the queued-B overlap contract: B
// stays in the FIFO even when an earlier transaction's TBN prefetch makes all
// of B's demand pages resident; at the head B charges its original one latency
// (no new unique count), issues zero DMA/PTE/TLB work, and replays its waiters
// once.
func TestUVMFaultQueuedSatisfiedByTBN(t *testing.T) {
	d, mw, gpuTables := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg := d.uvm.registrations[0]

	intakeFault(t, d, pid, 1, uint64(ptr))
	intakeFault(t, d, pid, 1, uint64(ptr)+64*mem.KB)
	txA, txB := mw.queue[0], mw.queue[1]

	mw.Tick() // A active, latency charged.

	// A's TBN prefetch effect: B's demand pages become resident while B is
	// queued. A's own pages too, so A's service is satisfied.
	makeRegionResident(t, d, reg, 0, 1)
	makeRegionResident(t, d, reg, 0, 0)

	mw.Handle(txA.latencyEvent)
	mw.Tick()
	reqs := drainRequests(d)
	// sbin_codex (todo 17): A's demand is fully local, but the TBN occupancy
	// (31 of 32 valid pages) expands to the 2 MB block and prefetches the
	// one remaining page 31 before the replay.
	if len(reqs) != 1 {
		t.Fatalf("A service requests = %d, want 1 DMA (TBN prefetch of page 31)",
			len(reqs))
	}
	h2dA, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("A service request = %T, want MemCopyH2DReq", reqs[0])
	}
	if len(h2dA.SrcBuffer) != basePageSize {
		t.Errorf("A DMA bytes = %d, want 1 page", len(h2dA.SrcBuffer))
	}
	deliverGeneralRsp(t, d, h2dA)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbA, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, tlbA)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replayA, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replayA)
	mw.Tick()

	if mw.active != txB {
		t.Fatal("B did not reach the head after A completed")
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 2 {
		t.Errorf("unique count = %d, want 2 (B counted at creation)", got)
	}
	if txB.latencyEvent == nil {
		t.Fatal("B latency not charged at head")
	}

	// B's GPU PTE is still the initial INVALID state before its service.
	if pte, found := gpuTables[0].Find(pid, uint64(ptr)+64*mem.KB); found &&
		pte.Location == vm.MemoryLocationGPU_LOCAL {
		t.Error("B PTE already GPU_LOCAL before service")
	}

	mw.Handle(txB.latencyEvent)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("B service requests = %d, want exactly 1 replay (zero DMA/PTE/TLB work)",
			len(reqs))
	}
	replayB, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("B service request = %T, want UVMFaultReplayReq", reqs[0])
	}
	if replayB.StartVA != 64*mem.KB || replayB.Size != 64*mem.KB {
		t.Errorf("B replay = %#x+%d, want 64KB+64KB", replayB.StartVA, replayB.Size)
	}
	if replayB.ReplayToken != txB.ReplayToken {
		t.Errorf("B replay token = %d, want %d", replayB.ReplayToken, txB.ReplayToken)
	}

	// No PTE work: B's PTE stays INVALID after the zero-work service.
	if pte, found := gpuTables[0].Find(pid, uint64(ptr)+64*mem.KB); found &&
		pte.Location == vm.MemoryLocationGPU_LOCAL {
		t.Error("B PTE published despite zero-work service")
	}

	deliverReplayAck(t, d, replayB)
	mw.Tick()

	if txB.phase != faultPhaseDone {
		t.Errorf("B phase = %v, want done", txB.phase)
	}
	if mw.active != nil || len(mw.queue) != 0 {
		t.Error("queue not drained after B completed")
	}
}

// TestUVMFaultQueuedPartiallySatisfiedByTBN proves the partial-overlap
// contract: when B reaches the head only part of its demand is resident, the
// service recomputes the missing pages from the current masks and migrates
// exactly those pages (missing-page-only DMA), then publishes PTEs, issues one
// 64 KB TLB invalidation, and replays once.
func TestUVMFaultQueuedPartiallySatisfiedByTBN(t *testing.T) {
	d, mw, gpuTables := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg := d.uvm.registrations[0]

	intakeFault(t, d, pid, 1, uint64(ptr))
	intakeFault(t, d, pid, 1, uint64(ptr)+64*mem.KB)
	txA, txB := mw.queue[0], mw.queue[1]

	mw.Tick() // A active.
	makeRegionResident(t, d, reg, 0, 0)
	mw.Handle(txA.latencyEvent)
	mw.Tick()
	reqs := drainRequests(d)
	deliverReplayAck(t, d, reqs[0].(*protocol.UVMFaultReplayReq))
	mw.Tick()

	// A's TBN prefetch effect: region 1 (allocation pages 15..30) is
	// mid-migration with 7 pages (15..21) already resident; the other 9
	// (22..30) are still at CPU. Destination HBM frames exist for all 16.
	block := reg.VABlocks[0]
	region := block.SubBlocks[1]
	sm := NewRegionStateMachine(
		RegionContext{PID: pid, GPU: 1, Block: 0, Region: 1}, region)
	if err := sm.Transition(RegionMigratingToGPU, sim.VTimeInSec(1)); err != nil {
		t.Fatalf("Transition(MIGRATING_TO_GPU): %v", err)
	}
	for page := uint64(15); page <= 21; page++ {
		setResident(reg, page, true)
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		block.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}
	for page := uint64(22); page <= 30; page++ {
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		block.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}
	if err := d.uvm.Reservation().ReserveAdmission(7 * basePageSize); err != nil {
		t.Fatalf("ReserveAdmission: %v", err)
	}
	d.uvm.Reservation().CommitAdmission(7 * basePageSize)

	// Distinct CPU backing bytes so the DMA payloads are observable.
	cpuData := make([][]byte, 16)
	for i := range cpuData {
		cpuData[i] = make([]byte, basePageSize)
		for j := range cpuData[i] {
			cpuData[i][j] = byte(15*31 + i*7 + j)
		}
		d.globalStorage.Write(reg.CPUBackingPages[15+i], cpuData[i])
	}

	// B reaches head: its original one latency, no new unique count.
	if mw.active != txB {
		t.Fatal("B did not reach the head")
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 2 {
		t.Errorf("unique count = %d, want 2", got)
	}
	mw.Handle(txB.latencyEvent)
	mw.Tick()

	// Missing-page-only DMA: exactly the 9 non-resident pages 22..30, emitted
	// as ONE maximal run (contiguous CPU backing PAs AND contiguous
	// pre-assigned destination frames -> one superior MemCopyH2DReq).
	// sbin_codex (todo 17): the TBN occupancy (31 of 32 valid pages) expands
	// to the 2 MB block and prefetches page 31 as a second run.
	reqs = drainRequests(d)
	if len(reqs) != 2 {
		t.Fatalf("B DMA reqs = %d, want 2 (run 22..30 + TBN prefetch page 31)",
			len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("B req 0 = %T, want MemCopyH2DReq", reqs[0])
	}
	wantPA := uint64(0x2_0000_0000) + 22*basePageSize
	if h2d.DstAddress != wantPA {
		t.Errorf("B DMA dst = %#x, want %#x (first page 22)", h2d.DstAddress, wantPA)
	}
	wantData := make([]byte, 0, 9*basePageSize)
	for page := uint64(22); page <= 30; page++ {
		wantData = append(wantData, cpuData[page-15]...)
	}
	if !bytes.Equal(h2d.SrcBuffer, wantData) {
		t.Errorf("B DMA payload mismatch: %d bytes, want the 9 pages 22..30",
			len(h2d.SrcBuffer))
	}
	h2d31, ok := reqs[1].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("B req 1 = %T, want MemCopyH2DReq", reqs[1])
	}
	if h2d31.DstAddress != uint64(4096)+4*mem.GB || len(h2d31.SrcBuffer) != basePageSize {
		t.Errorf("B prefetch run = %#x+%d, want the first GPU frame + 1 page",
			h2d31.DstAddress, len(h2d31.SrcBuffer))
	}
	for page := uint64(15); page <= 21; page++ {
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("pre-resident page %d marked in flight", page)
		}
	}
	for page := uint64(22); page <= 31; page++ {
		if !maskBit(reg.InFlightMask, page) {
			t.Errorf("missing page %d not marked in flight", page)
		}
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 10*basePageSize {
		t.Errorf("reserved N = %d, want 10 pages", got)
	}
	if txB.phase != faultPhaseMigrating {
		t.Errorf("B phase = %v, want migrating", txB.phase)
	}

	// DMA completions: residency + PTE publication for the 9 pages only, then
	// exactly one 64 KB TLB invalidation.
	for _, req := range reqs {
		deliverGeneralRsp(t, d, req)
	}
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbReq, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	if tlbReq.StartVA != 64*mem.KB || tlbReq.Size != 64*mem.KB {
		t.Errorf("TLB invalidate = %#x+%d, want 64KB+64KB", tlbReq.StartVA, tlbReq.Size)
	}
	for page := uint64(15); page <= 31; page++ {
		if !maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d not resident after migration", page)
		}
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d still in flight after migration", page)
		}
	}
	for page := uint64(22); page <= 31; page++ {
		pte, found := gpuTables[0].Find(pid, reg.Base+page*basePageSize)
		if !found || pte.Location != vm.MemoryLocationGPU_LOCAL {
			t.Errorf("migrated page %d PTE = %+v/%v, want GPU_LOCAL", page, pte, found)
		}
	}
	for page := uint64(15); page <= 21; page++ {
		if pte, found := gpuTables[0].Find(pid, reg.Base+page*basePageSize); found &&
			pte.Location == vm.MemoryLocationGPU_LOCAL {
			t.Errorf("pre-resident page %d PTE published by B's service", page)
		}
	}
	if region.State != RegionGPUResident {
		t.Errorf("region state = %s, want GPU_RESIDENT", region.State)
	}
	// sbin_codex (todo 17): R = region 0 (64 KB) + 7 pre-resident pages of
	// region 1 + B's 10 migrated pages (9 demand + TBN prefetch page 31).
	if got := d.uvm.Reservation().ResidentBytes(); got != 132*mem.KB {
		t.Errorf("resident R = %d, want 132KB (region 0 + all of B region + prefetch)", got)
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d, want 0 after commit", got)
	}

	// TLB ack -> exactly one replay -> ack -> complete.
	deliverTLBAck(t, d, tlbReq)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replayB, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replayB)
	mw.Tick()

	if txB.phase != faultPhaseDone {
		t.Errorf("B phase = %v, want done", txB.phase)
	}
	if mw.active != nil || len(mw.queue) != 0 {
		t.Error("queue not drained after B completed")
	}
}

// findManagedCopyMiddleware locates the managed copy handler by type (the
// middleware list gained the migration service, so positional indexing is
// brittle). // sbin_codex (todo 18)
func findManagedCopyMiddleware(d *Driver) *managedMemoryCopyMiddleware {
	for _, mw := range d.middlewares {
		if dm, ok := mw.(*defaultMemoryCopyMiddleware); ok {
			return dm.managed
		}
	}
	return nil
}

// TestUVMFaultCopyFaultOwnershipRace proves the shared ownership table
// contract: a COPY-owned region queues the later fault without duplicate
// service or ownership cycles, and an earlier active fault transition
// completes before a ticketed copy.
func TestUVMFaultCopyFaultOwnershipRace(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	copyMw := findManagedCopyMiddleware(d)

	// The copy claims the region first.
	q := d.CreateCommandQueue(ctx)
	cmd := &MemCopyH2DCommand{ID: "race-copy", Dst: ptr, Src: payload64K{}}
	q.Enqueue(cmd)
	copyMw.tryProcess(cmd, q)
	txCopy := copyMw.copies[0]
	if !txCopy.claimed {
		t.Fatal("copy not claimed")
	}

	// A fault arrives for the COPY-owned region: it queues without duplicate
	// service or ownership cycles.
	intakeFault(t, d, pid, 1, uint64(ptr))
	if len(mw.queue) != 1 {
		t.Fatalf("fault queue = %d, want 1", len(mw.queue))
	}
	txFault := mw.queue[0]
	if mw.active != nil {
		t.Fatal("fault active while the copy owns the region")
	}
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipCopy || owner != txCopy.Ticket {
		t.Errorf("owner = %v/%d, want COPY/%d", typ, owner, txCopy.Ticket)
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 1 {
		t.Errorf("unique count = %d, want 1", got)
	}

	// Complete the copy: block -> unblock.
	reqs := drainRequests(d)
	deliverGeneralRsp(t, d, reqs[0])
	copyMw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-block requests = %d, want 1 UnblockRange", len(reqs))
	}
	deliverGeneralRsp(t, d, reqs[0])
	copyMw.Tick()
	if txCopy.phase != copyPhaseDone {
		t.Fatalf("copy phase = %v, want done", txCopy.phase)
	}

	// The queued fault reaches the head and charges its one latency; no new
	// unique count.
	if !mw.Tick() {
		t.Fatal("fault did not start after the copy completed")
	}
	if mw.active != txFault {
		t.Fatal("fault not active")
	}
	if txFault.latencyEvent == nil {
		t.Fatal("fault latency not charged")
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 1 {
		t.Errorf("unique count = %d, want 1", got)
	}

	// The fault claims the released key and services its demand: destination
	// HBM frames exist for the 15 demand pages of region 0.
	reg := d.uvm.registrations[0]
	block := reg.VABlocks[0]
	for page := uint64(0); page < 15; page++ {
		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
		block.Pages[blockLocal].GPUPhysicalPage = 0x2_0000_0000 + page*basePageSize
	}
	mw.Handle(txFault.latencyEvent)
	mw.Tick()
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipFault || owner != txFault.Ticket {
		t.Errorf("owner = %v/%d, want FAULT/%d", typ, owner, txFault.Ticket)
	}
	reqs = drainRequests(d)
	// sbin_codex (todo 17): the TBN selection (15/16 valid pages occupied)
	// expands to the 2 MB block and prefetches page 15 as a second run.
	if len(reqs) != 2 {
		t.Fatalf("fault DMA reqs = %d, want 2 (run 0..14 + TBN prefetch page 15)",
			len(reqs))
	}
	if h2d, ok := reqs[0].(*protocol.MemCopyH2DReq); !ok ||
		h2d.DstAddress != 0x2_0000_0000 || len(h2d.SrcBuffer) != 15*basePageSize {
		t.Fatalf("fault DMA req 0 = %+v, want one 60 KB run at 0x2_0000_0000", reqs[0])
	}
	if h2d, ok := reqs[1].(*protocol.MemCopyH2DReq); !ok ||
		h2d.DstAddress != uint64(4096)+4*mem.GB || len(h2d.SrcBuffer) != basePageSize {
		t.Fatalf("fault DMA req 1 = %+v, want one 4 KB prefetch run at the first GPU frame",
			reqs[1])
	}

	// Scenario B: an earlier active fault transition completes before a
	// ticketed copy.
	d2, mw2, _ := buildFaultDriver(t, false)
	ctx2 := d2.Init()
	pid2 := ctx2.pid
	ptr2 := d2.AllocateManagedMemory(ctx2, 64*mem.KB)
	reg2 := d2.uvm.registrations[0]
	copyMw2 := findManagedCopyMiddleware(d2)

	intakeFault(t, d2, pid2, 1, uint64(ptr2))
	txF2 := mw2.queue[0]
	mw2.Tick()
	makeRegionResident(t, d2, reg2, 0, 0)
	mw2.Handle(txF2.latencyEvent)
	mw2.Tick()
	reqs = drainRequests(d2)
	// sbin_codex (todo 17): the TBN occupancy (15/16 valid pages) expands to
	// the 2 MB block and prefetches page 15 before the replay.
	if len(reqs) != 1 {
		t.Fatalf("fault service requests = %d, want 1 DMA (TBN prefetch of page 15)",
			len(reqs))
	}
	h2dF2, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("fault service request = %T, want MemCopyH2DReq", reqs[0])
	}
	deliverGeneralRsp(t, d2, h2dF2)
	mw2.Tick()
	reqs = drainRequests(d2)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbF2, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d2, tlbF2)
	mw2.Tick()
	reqs = drainRequests(d2)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replay := reqs[0].(*protocol.UVMFaultReplayReq)

	// A ticketed copy arrives while the fault owns the key: it enqueues once
	// and holds no key.
	q2 := d2.CreateCommandQueue(ctx2)
	cmd2 := &MemCopyH2DCommand{ID: "race-copy-2", Dst: ptr2, Src: payload64K{}}
	q2.Enqueue(cmd2)
	copyMw2.tryProcess(cmd2, q2)
	txC2 := copyMw2.copies[0]
	if txC2.claimed {
		t.Fatal("copy claimed a fault-owned key")
	}
	if len(d2.uvm.copyWaiters) != 1 || d2.uvm.copyWaiters[0] != txC2 {
		t.Fatal("copy not enqueued exactly once by ticket")
	}
	if txF2.Ticket >= txC2.Ticket {
		t.Errorf("ticket order violated: fault %d >= copy %d", txF2.Ticket, txC2.Ticket)
	}
	key2 := copyRegionKey{PID: pid2, GPU: 1, RegionBase: 0}
	if typ, owner := d2.uvm.OwnerOf(key2); typ != OwnershipFault || owner != txF2.Ticket {
		t.Errorf("owner = %v/%d, want FAULT/%d while copy waits", typ, owner, txF2.Ticket)
	}

	// The fault completes after its replay; the copy claims the released key.
	deliverReplayAck(t, d2, replay)
	mw2.Tick()
	if txF2.phase != faultPhaseDone {
		t.Errorf("fault phase = %v, want done", txF2.phase)
	}
	if !txC2.claimed {
		t.Fatal("copy did not claim after the fault released")
	}
	if typ, owner := d2.uvm.OwnerOf(key2); typ != OwnershipCopy || owner != txC2.Ticket {
		t.Errorf("owner = %v/%d, want COPY/%d", typ, owner, txC2.Ticket)
	}
}

// TestUVMFaultDuplicateCompletion proves the transaction completes only after
// the replay ack, and a duplicate replay ack is rejected without double
// completion or double accounting.
func TestUVMFaultDuplicateCompletion(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	intakeFault(t, d, pid, 1, uint64(ptr))
	tx := mw.queue[0]
	mw.Tick()
	makeRegionResident(t, d, reg, 0, 0)
	mw.Handle(tx.latencyEvent)
	mw.Tick()
	reqs := drainRequests(d)
	// sbin_codex (todo 17): the TBN occupancy (15/16 valid pages) expands to
	// the 2 MB block and prefetches page 15 before the replay.
	if len(reqs) != 1 {
		t.Fatalf("service requests = %d, want 1 DMA (TBN prefetch of page 15)",
			len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("service request = %T, want MemCopyH2DReq", reqs[0])
	}
	deliverGeneralRsp(t, d, h2d)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlb, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, tlb)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replay := reqs[0].(*protocol.UVMFaultReplayReq)

	// Complete state only after replay: before the ack the transaction is
	// still active and owns its key.
	if mw.active != tx || tx.phase != faultPhaseReplaying {
		t.Fatalf("active/phase = %v/%v, want active/replaying before ack",
			mw.active == tx, tx.phase)
	}
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipFault || owner != tx.Ticket {
		t.Errorf("owner = %v/%d, want FAULT/%d before ack", typ, owner, tx.Ticket)
	}

	deliverReplayAck(t, d, replay)
	mw.Tick()
	if tx.phase != faultPhaseDone {
		t.Errorf("phase = %v, want done after ack", tx.phase)
	}
	if mw.active != nil || len(mw.queue) != 0 {
		t.Error("transaction not retired after the ack")
	}
	if !d.uvm.IsKeyIdle(key) {
		t.Error("key not released after completion")
	}
	if d.uvm.faultByKey[key] != nil {
		t.Error("coalescing entry not removed after completion")
	}

	// A duplicate replay ack is rejected: no double completion, no new active
	// transaction, no double accounting.
	deliverReplayAck(t, d, replay)
	mw.Tick()
	if mw.active != nil || len(mw.queue) != 0 {
		t.Error("duplicate ack started a new transaction")
	}
	if got := d.uvm.UniqueFaultServiceCount(); got != 1 {
		t.Errorf("unique count = %d, want 1", got)
	}
	if got := d.uvm.RawPageFaultCount(); got != 1 {
		t.Errorf("raw count = %d, want 1", got)
	}
	if mw.latencyChargedCount != 1 {
		t.Errorf("latency charges = %d, want 1", mw.latencyChargedCount)
	}
	if !d.uvm.IsKeyIdle(key) {
		t.Error("key re-acquired by a duplicate ack")
	}
}

// TestUVMFaultIllegalState proves the fault intake rejects protocol
// violations deterministically without mutating any state: faults on
// unmanaged addresses, on GPU_RESIDENT regions, and on MIGRATING_TO_CPU
// regions all fail before any transaction is created.
func TestUVMFaultIllegalState(t *testing.T) {
	d, mw, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	pid := ctx.pid

	// Unmanaged address: no registration.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fault on an unmanaged address did not panic")
			}
		}()
		intakeFault(t, d, pid, 1, 0x1_0000_0000)
	}()

	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	// GPU_RESIDENT region: no fault can start on a resident region.
	makeRegionGPUResident(t, d, reg, 0, 0)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fault on a GPU_RESIDENT region did not panic")
			}
		}()
		intakeFault(t, d, pid, 1, uint64(ptr))
	}()

	// MIGRATING_TO_CPU region: GPU accesses stall; a fault cannot be serviced.
	region := reg.VABlocks[0].SubBlocks[0]
	sm := NewRegionStateMachine(
		RegionContext{PID: pid, GPU: 1, Block: 0, Region: 0}, region)
	now := sim.VTimeInSec(2)
	for _, to := range []RegionState{RegionEvictPending, RegionMigratingToCPU} {
		if err := sm.Transition(to, now); err != nil {
			t.Fatalf("Transition(%s): %v", to, err)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fault on a MIGRATING_TO_CPU region did not panic")
			}
		}()
		intakeFault(t, d, pid, 1, uint64(ptr))
	}()

	// The rejected intakes created no transaction state; only the raw counter
	// observed the requests.
	if got := d.uvm.UniqueFaultServiceCount(); got != 0 {
		t.Errorf("unique count = %d, want 0", got)
	}
	if got := d.uvm.CoalescedPageFaultCount(); got != 0 {
		t.Errorf("coalesced count = %d, want 0", got)
	}
	if got := d.uvm.RawPageFaultCount(); got != 3 {
		t.Errorf("raw count = %d, want 3 (every request counted)", got)
	}
	if len(mw.queue) != 0 || mw.active != nil {
		t.Error("rejected intakes left queue/active state")
	}
}
