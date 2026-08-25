package driver

import (
	"fmt"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: projected-occupancy pre-eviction contract tests (todo 20 of
// plan mgpusim-uvm-manager, uvm-manager.md §17.1, §23.1.2, decisions at
// 3002-3017/3071-3074). The QA regex
// 'TestUVM(PreEviction|ProjectedOccupancy|AllocatorFrames|
// FeasibleHeadroomConcurrentAdmission|OptionalHeadroomShortfall)' runs the
// fixtures in this file: the pre-admission R+I+N computation, queueing only
// for hard capacity/frame shortage, immediate reserve+H2D when the incoming
// fits, deterministic 64 KB LRU victims launched CONCURRENTLY with the H2D
// (H2D never waits for D2H; both use normal DMA backpressure), R+I+N <= C
// throughout, headroom required only after victim completion, the
// pinned-only optional-target shortfall diagnostic, and reservations/victim
// ordinals with no fixed depth.

// buildPreEvictionDriver builds a real driver (real allocator, real CPU + GPU
// page tables, real UVM manager, host storage) with a configurable UVM
// capacity and the fault + eviction middlewares wired, plus a registered GPU
// port.
func buildPreEvictionDriver(t *testing.T, capacity uint64) (
	*Driver, *evictionMiddleware, *faultServiceMiddleware, []vm.PageTable,
) {
	t.Helper()

	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	cfg.GPUMemoryCapacity = capacity
	cfg.CapacitySet = true

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

	return d, d.uvmEviction, d.uvmFault, gpuTables
}

// checkUVMInvariant asserts the R+I+N <= C capacity invariant. // sbin_codex
func checkUVMInvariant(t *testing.T, d *Driver) {
	t.Helper()
	r := d.uvm.Reservation().ResidentBytes()
	i := d.uvm.Reservation().InFlightBytes()
	n := d.uvm.Reservation().ReservedBytes()
	c := d.uvm.Reservation().CapacityBytes()
	if r+i+n > c {
		t.Errorf("R+I+N = %d+%d+%d = %d > C = %d", r, i, n, r+i+n, c)
	}
}

// drivePreEvictionToEnd promotes the next queued pre-eviction transaction
// (when the middleware is idle) and drives it through every stage until done.
// sbin_codex
func drivePreEvictionToEnd(t *testing.T, d *Driver, mw *evictionMiddleware) {
	t.Helper()
	if mw.active == nil {
		if len(mw.pending) == 0 {
			t.Fatal("no queued eviction transaction")
		}
		if !mw.Tick() {
			t.Fatal("eviction tick did not promote the transaction")
		}
		reqs := drainRequests(d)
		if len(reqs) != 1 {
			t.Fatalf("promote requests = %d, want 1 block", len(reqs))
		}
		block, ok := reqs[0].(*vm.BlockRange)
		if !ok {
			t.Fatalf("promote request = %T, want BlockRange", reqs[0])
		}
		deliverGeneralRsp(t, d, block)
	}
	driveEvictionToEnd(t, d, mw, 0)
}

// TestUVMProjectedOccupancyDecision proves the pre-admission R+I+N
// computation (uvm-manager.md §17.1): the admission fits only when
// R+I+N+bytes <= C, the headroom trigger
// NeedToEvict = max(0, H - (C-(R+I+N)+E)) counts incoming reservations and
// in-flight pre-eviction bytes, and NumVictims = ceil(NeedToEvict/64KB).
func TestUVMProjectedOccupancyDecision(t *testing.T) {
	cases := []struct {
		name        string
		r, i, n     uint64 // resident, in-flight evicting, reserved incoming
		e           uint64 // pre-eviction bytes in flight (synthetic entry)
		bytes       uint64 // new H2D admission bytes
		wantHard    bool
		wantFits    bool
		wantNeed    uint64
		wantVictims int
	}{
		{"empty", 0, 0, 0, 0, 64 * mem.KB, false, true, 0, 0},
		{"headroom-satisfied", 128 * mem.KB, 0, 0, 0, 64 * mem.KB,
			false, true, 0, 0},
		{"headroom-short", 132 * mem.KB, 0, 0, 0, 64 * mem.KB,
			false, true, 4 * mem.KB, 1},
		{"incoming-reservations-count", 64 * mem.KB, 0, 68 * mem.KB, 0,
			64 * mem.KB, false, true, 4 * mem.KB, 1},
		{"evicting-e-relaxes-headroom", 132 * mem.KB, 0, 0, 60 * mem.KB,
			64 * mem.KB, false, true, 0, 0},
		{"hard-shortage", 200 * mem.KB, 0, 0, 0, 64 * mem.KB,
			true, false, 64 * mem.KB, 1},
		{"hard-shortage-with-e", 200 * mem.KB, 0, 0, 60 * mem.KB,
			64 * mem.KB, true, false, 4 * mem.KB, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, _ := buildPreEvictionDriver(t, 256*mem.KB)
			m := d.uvm
			m.Lock()
			defer m.Unlock()
			if tc.r > 0 {
				if err := m.reservation.ReserveAdmission(tc.r); err != nil {
					t.Fatalf("reserve R: %v", err)
				}
				m.reservation.CommitAdmission(tc.r)
			}
			if tc.i > 0 {
				if err := m.reservation.ReserveAdmission(tc.i); err != nil {
					t.Fatalf("reserve I: %v", err)
				}
				m.reservation.CommitAdmission(tc.i)
				m.reservation.StartMigration(tc.i)
			}
			if tc.n > 0 {
				if err := m.reservation.ReserveAdmission(tc.n); err != nil {
					t.Fatalf("reserve N: %v", err)
				}
			}
			if tc.e > 0 {
				m.evictByKey[copyRegionKey{PID: 1, GPU: 1, RegionBase: 0}] =
					&evictionTransaction{preEviction: true, bytes: tc.e}
			}
			dec := m.computeAdmissionDecisionLocked(tc.bytes)
			if dec.HardShortage != tc.wantHard {
				t.Errorf("hard shortage = %v, want %v",
					dec.HardShortage, tc.wantHard)
			}
			if dec.Fits != tc.wantFits {
				t.Errorf("fits = %v, want %v", dec.Fits, tc.wantFits)
			}
			if dec.NeedToEvict != tc.wantNeed {
				t.Errorf("need = %d, want %d", dec.NeedToEvict, tc.wantNeed)
			}
			if dec.NumVictims != tc.wantVictims {
				t.Errorf("victims = %d, want %d",
					dec.NumVictims, tc.wantVictims)
			}
		})
	}
}

// TestUVMProjectedOccupancyCapacityBounds proves the runtime capacity
// enforcement: the UVM capacity must be page-aligned, >= 64 KB, and bounded
// by the available GPU DRAM/frames (config-time validation is todo 1; the
// manager enforces it defensively at construction).
func TestUVMProjectedOccupancyCapacityBounds(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: NewUVMManager did not panic", name)
			}
		}()
		fn()
	}
	// Capacity below the 64 KB minimum.
	mustPanic("below-64KB", func() {
		NewUVMManager(DefaultUVMConfig(), 32*mem.KB)
	})
	// Capacity not 4 KB-aligned.
	mustPanic("unaligned", func() {
		NewUVMManager(DefaultUVMConfig(), 66*mem.KB)
	})
	// Capacity beyond the available GPU DRAM/frames.
	cfg := DefaultUVMConfig()
	cfg.CapacitySet = true
	cfg.GPUMemoryCapacity = 8 * mem.GB
	mustPanic("above-dram", func() {
		NewUVMManager(cfg, 4*mem.GB)
	})
	// A page-aligned >= 64 KB capacity within DRAM is accepted.
	NewUVMManager(DefaultUVMConfig(), 64*mem.KB)
}

// TestUVMProjectedOccupancyInvariant proves the gate never over-reserves:
// a fitting admission reserves and keeps R+I+N <= C, a hard-shortage
// admission reserves nothing, and the reservation rejects over-capacity.
func TestUVMProjectedOccupancyInvariant(t *testing.T) {
	d, _, _, _ := buildPreEvictionDriver(t, 192*mem.KB)
	m := d.uvm
	// The reservation rejects an over-capacity admission.
	if err := m.reservation.ReserveAdmission(200 * mem.KB); err == nil {
		t.Error("reservation over capacity accepted")
	}
	m.Lock()
	defer m.Unlock()
	// R = 124 KB.
	if err := m.reservation.ReserveAdmission(124 * mem.KB); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	m.reservation.CommitAdmission(124 * mem.KB)
	// A fitting admission reserves and keeps R+I+N <= C.
	dec := m.computeAdmissionDecisionLocked(64 * mem.KB)
	if dec.HardShortage {
		t.Fatal("unexpected hard shortage")
	}
	if _, err := m.admitWithPreEvictionLocked(1, 1, 64*mem.KB); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if r, i, n, _ := m.projectedOccupancyLocked(); r+i+n >
		m.reservation.CapacityBytes() {
		t.Errorf("R+I+N = %d > C = %d after admission", r+i+n,
			m.reservation.CapacityBytes())
	}
	// A hard-shortage admission reserves nothing.
	if _, err := m.admitWithPreEvictionLocked(1, 1, 128*mem.KB); err == nil {
		t.Error("hard-shortage admission accepted")
	}
	if r, i, n, _ := m.projectedOccupancyLocked(); r+i+n >
		m.reservation.CapacityBytes() {
		t.Errorf("R+I+N = %d > C = %d after rejected admission", r+i+n,
			m.reservation.CapacityBytes())
	}
}

// failingFrameAllocator is a migration frame allocator that always fails:
// it proves the frame-shortage queue. // sbin_codex
type failingFrameAllocator struct{}

func (f *failingFrameAllocator) allocateMigrationFrames(
	gpu int, count int,
) ([]uint64, error) {
	return nil, fmt.Errorf("uvm: no free frames on GPU %d", gpu)
}

func (f *failingFrameAllocator) freeMigrationFrames(frames []uint64) {}

// TestUVMAllocatorFramesShortage proves a hard FRAME shortage queues the
// admission: no H2D is emitted, the reservation is released, and the retry
// admits immediately once frames are available again.
func TestUVMAllocatorFramesShortage(t *testing.T) {
	d, _, faultmw, _ := buildPreEvictionDriver(t, 256*mem.KB)
	ctx := d.Init()
	pid := ctx.pid
	ptr1 := d.AllocateManagedMemory(ctx, 60*mem.KB)
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr1))
	ptr2 := d.AllocateManagedMemory(ctx, 64*mem.KB)

	// Frame shortage: the allocator cannot provide destination frames.
	d.uvm.SetFrameAllocator(&failingFrameAllocator{})
	intakeFault(t, d, pid, 1, uint64(ptr2))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start the transaction")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service the admission")
	}
	tx := faultmw.active
	if tx.phase != faultPhaseWaitingCapacity {
		t.Fatalf("phase = %v, want waiting-capacity", tx.phase)
	}
	if len(d.requestsToSend) != 0 {
		t.Error("H2D emitted without destination frames")
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("N = %d, want 0 after the frame shortage", got)
	}
	if got := d.uvm.MigrationWaitCyclesForCapacity(); got < 1 {
		t.Errorf("wait cycles = %d, want >= 1", got)
	}
	checkUVMInvariant(t, d)

	// Frames available again: the retry admits and emits the H2D.
	d.uvm.SetFrameAllocator(d)
	if !faultmw.Tick() {
		t.Fatal("retry tick made no progress")
	}
	if tx.phase != faultPhaseMigrating {
		t.Fatalf("phase = %v, want migrating", tx.phase)
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 H2D", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	deliverGeneralRsp(t, d, h2d)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete the migration")
	}
	reqs = drainRequests(d)
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not retire the transaction")
	}
	if faultmw.active != nil {
		t.Error("fault transaction not retired")
	}
}

// TestUVMFeasibleHeadroomConcurrentAdmission proves the feasible-headroom
// fixture: the H2D is launched IMMEDIATELY (no admission wait), the 64 KB LRU
// victim is reserved and its D2H runs CONCURRENTLY with the H2D (both
// outstanding before either completes), R+I+N <= C throughout, and the 64 KB
// headroom appears only after the victim completes.
func TestUVMFeasibleHeadroomConcurrentAdmission(t *testing.T) {
	d, evmw, faultmw, _ := buildPreEvictionDriver(t, 192*mem.KB)
	ctx := d.Init()
	pid := ctx.pid
	ptr1 := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg1 := d.uvm.registrations[0]
	// The fault on region 1 selects the [0, 128KB) TBN node: regions 0 and 1
	// become GPU-resident (R = 124 KB).
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr1)+64*mem.KB)
	if got := d.uvm.Reservation().ResidentBytes(); got != 124*mem.KB {
		t.Fatalf("R = %d, want 124KB", got)
	}
	ptr2 := d.AllocateManagedMemory(ctx, 64*mem.KB)

	// The admission fits (124 + 64 <= 192) but the headroom is short
	// (free = 4 KB < 64 KB): reserve + H2D immediately, victim concurrently.
	intakeFault(t, d, pid, 1, uint64(ptr2))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start the transaction")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service the admission")
	}
	tx := faultmw.active
	if tx.phase != faultPhaseMigrating {
		t.Fatalf("phase = %v, want migrating (no admission wait)", tx.phase)
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 H2D", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	// The gate reserved exactly one deterministic LRU victim (region 0).
	if evmw.active == nil {
		t.Fatal("no pre-eviction victim queued")
	}
	victim := evmw.active
	if !victim.preEviction {
		t.Error("victim not marked as a pre-eviction")
	}
	if victim.RegionBase != 0 {
		t.Errorf("victim = %#x, want region 0 (LRU)", victim.RegionBase)
	}
	if reg1.VABlocks[0].SubBlocks[0].State != RegionEvictPending {
		t.Error("victim region not EVICTING")
	}
	checkUVMInvariant(t, d)
	if got := d.uvm.NumPreEvictions(); got != 1 {
		t.Errorf("num pre-evictions = %d, want 1", got)
	}
	if got := d.uvm.BytesPreEvicted(); got != 60*mem.KB {
		t.Errorf("bytes pre-evicted = %d, want 60KB", got)
	}
	if got := d.uvm.NumPreEvictionsOverlappedWithH2D(); got != 1 {
		t.Errorf("overlapped with H2D = %d, want 1", got)
	}

	// Drive the victim to its D2H: block -> WB+INV -> TLB -> D2H.
	if !evmw.Tick() {
		t.Fatal("eviction tick did not start the victim")
	}
	reqs = drainRequests(d)
	block, ok := reqs[0].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request = %T, want BlockRange", reqs[0])
	}
	deliverGeneralRsp(t, d, block)
	evmw.Tick()
	reqs = drainRequests(d)
	flush, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("request = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	deliverFlushRsp(t, d, flush)
	evmw.Tick()
	reqs = drainRequests(d)
	tlb, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, tlb)
	evmw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 D2H", len(reqs))
	}
	d2h, ok := reqs[0].(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyD2HReq", reqs[0])
	}

	// H2D and D2H are BOTH outstanding: the H2D was emitted before the
	// victim's D2H and has not completed — no admission wait for the D2H.
	if tx.phase != faultPhaseMigrating {
		t.Errorf("fault phase = %v, want migrating (H2D in flight)",
			tx.phase)
	}
	if victim.phase != evictionStageD2H {
		t.Errorf("victim phase = %v, want d2h", victim.phase)
	}
	checkUVMInvariant(t, d)

	// Complete the H2D first (fault path: PTE publish -> TLB -> replay).
	deliverGeneralRsp(t, d, h2d)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete the migration")
	}
	reqs = drainRequests(d)
	faultReplay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, faultReplay)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not retire the transaction")
	}
	if faultmw.active != nil {
		t.Error("fault transaction not retired")
	}

	// Complete the D2H (eviction path: final PTE -> free -> replay ->
	// unblock). The headroom is required only after the victim completes.
	deliverGeneralRsp(t, d, d2h)
	evmw.Tick()
	reqs = drainRequests(d)
	victimReplay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-D2H request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, victimReplay)
	evmw.Tick()
	reqs = drainRequests(d)
	unblock, ok := reqs[0].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request = %T, want UnblockRange", reqs[0])
	}
	deliverGeneralRsp(t, d, unblock)
	evmw.Tick()
	if victim.phase != evictionStageDone {
		t.Errorf("victim phase = %v, want done", victim.phase)
	}

	// Converged: R = 128 KB (alloc 1 region 1 + alloc 2), free = 64 KB
	// headroom, R+I+N <= C.
	if got := d.uvm.Reservation().ResidentBytes(); got != 128*mem.KB {
		t.Errorf("R = %d, want 128KB", got)
	}
	if free := d.uvm.Reservation().CapacityBytes() -
		d.uvm.Reservation().ResidentBytes(); free != 64*mem.KB {
		t.Errorf("free = %d, want 64KB headroom after the victim", free)
	}
	checkUVMInvariant(t, d)
}

// TestUVMOptionalHeadroomShortfall proves the pinned-only optional target:
// every eligible region is pinned, so no victim can be reserved; the
// admission still admits and records the shortfall diagnostic.
func TestUVMOptionalHeadroomShortfall(t *testing.T) {
	d, evmw, faultmw, _ := buildPreEvictionDriver(t, 192*mem.KB)
	ctx := d.Init()
	pid := ctx.pid
	ptr1 := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg1 := d.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr1)+64*mem.KB)
	key0 := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}
	key1 := copyRegionKey{PID: pid, GPU: 1, RegionBase: 64 * mem.KB}
	d.uvm.PinRegion(key0)
	d.uvm.PinRegion(key1)
	if reg1.VABlocks[0].SubBlocks[0].State != RegionGPUResident {
		t.Fatal("region 0 not GPU-resident")
	}
	ptr2 := d.AllocateManagedMemory(ctx, 64*mem.KB)

	// The admission fits (124 + 64 <= 192) but the headroom is short; every
	// region is pinned -> no victim; the optional target admits anyway.
	intakeFault(t, d, pid, 1, uint64(ptr2))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start the transaction")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service the admission")
	}
	tx := faultmw.active
	if tx.phase != faultPhaseMigrating {
		t.Fatalf("phase = %v, want migrating (admitted)", tx.phase)
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 H2D", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	// No victim was reserved; the shortfall diagnostic was recorded.
	if len(d.uvm.evictByKey) != 0 {
		t.Error("victim reserved despite pinned-only regions")
	}
	if evmw.active != nil {
		t.Error("eviction transaction queued despite pinned-only regions")
	}
	if got := d.uvm.OptionalHeadroomShortfallCount(); got != 1 {
		t.Errorf("shortfall count = %d, want 1", got)
	}
	if got := d.uvm.OptionalHeadroomShortfallBytes(); got != 60*mem.KB {
		t.Errorf("shortfall bytes = %d, want 60KB", got)
	}
	checkUVMInvariant(t, d)

	// The admission completes normally.
	deliverGeneralRsp(t, d, h2d)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete the migration")
	}
	reqs = drainRequests(d)
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not retire the transaction")
	}
	if faultmw.active != nil {
		t.Error("fault transaction not retired")
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 188*mem.KB {
		t.Errorf("R = %d, want 188KB", got)
	}
}

// TestUVMPreEvictionHardShortageQueues proves the hard capacity shortage:
// the admission queues (no H2D, no reservation), the gate launches a victim
// to free capacity, and after the victim completes the retry admits and
// converges with the 64 KB headroom.
func TestUVMPreEvictionHardShortageQueues(t *testing.T) {
	d, evmw, faultmw, _ := buildPreEvictionDriver(t, 128*mem.KB)
	ctx := d.Init()
	pid := ctx.pid
	ptr1 := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg1 := d.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr1)+64*mem.KB)
	if got := d.uvm.Reservation().ResidentBytes(); got != 124*mem.KB {
		t.Fatalf("R = %d, want 124KB", got)
	}
	ptr2 := d.AllocateManagedMemory(ctx, 64*mem.KB)

	// 124 + 64 = 188 > 128: hard shortage. The admission queues.
	intakeFault(t, d, pid, 1, uint64(ptr2))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start the transaction")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service the admission")
	}
	tx := faultmw.active
	if tx.phase != faultPhaseWaitingCapacity {
		t.Fatalf("phase = %v, want waiting-capacity", tx.phase)
	}
	if len(d.requestsToSend) != 0 {
		t.Error("requests emitted while waiting for capacity")
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("N = %d, want 0 (no reservation while waiting)", got)
	}
	// The gate launched one deterministic victim to free capacity.
	if evmw.active == nil {
		t.Fatal("no pre-eviction victim launched on hard shortage")
	}
	victim := evmw.active
	if victim.RegionBase != 0 {
		t.Errorf("victim = %#x, want region 0 (LRU)", victim.RegionBase)
	}
	if reg1.VABlocks[0].SubBlocks[0].State != RegionEvictPending {
		t.Error("victim region not EVICTING")
	}
	if got := d.uvm.MigrationWaitCyclesForCapacity(); got != 1 {
		t.Errorf("wait cycles = %d, want 1 at entry", got)
	}
	checkUVMInvariant(t, d)

	// The victim completes and frees capacity.
	driveEvictionToEnd(t, d, evmw, pid)
	if victim.phase != evictionStageDone {
		t.Fatalf("victim phase = %v, want done", victim.phase)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 64*mem.KB {
		t.Errorf("R = %d, want 64KB after the victim", got)
	}

	// The retry now fits: reserve + H2D immediately; the headroom policy
	// launches the next victim (region 1) concurrently.
	if !faultmw.Tick() {
		t.Fatal("retry tick made no progress")
	}
	if tx.phase != faultPhaseMigrating {
		t.Fatalf("phase = %v, want migrating", tx.phase)
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 H2D", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	if evmw.active == nil {
		t.Fatal("no second victim launched")
	}
	victim2 := evmw.active
	if victim2.RegionBase != 64*mem.KB {
		t.Errorf("second victim = %#x, want region 1", victim2.RegionBase)
	}
	if victim2.Ticket <= victim.Ticket {
		t.Errorf("victim ordinals not monotonic: %d then %d",
			victim.Ticket, victim2.Ticket)
	}
	checkUVMInvariant(t, d)

	// Converge: complete the H2D and the second victim.
	deliverGeneralRsp(t, d, h2d)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete the migration")
	}
	reqs = drainRequests(d)
	faultReplay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, faultReplay)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not retire the transaction")
	}
	if faultmw.active != nil {
		t.Error("fault transaction not retired")
	}

	drivePreEvictionToEnd(t, d, evmw)
	if victim2.phase != evictionStageDone {
		t.Fatalf("second victim phase = %v, want done", victim2.phase)
	}
	// Converged: R = 64 KB (alloc 2), free = 64 KB headroom.
	if got := d.uvm.Reservation().ResidentBytes(); got != 64*mem.KB {
		t.Errorf("R = %d, want 64KB", got)
	}
	if free := d.uvm.Reservation().CapacityBytes() -
		d.uvm.Reservation().ResidentBytes(); free != 64*mem.KB {
		t.Errorf("free = %d, want 64KB headroom", free)
	}
	checkUVMInvariant(t, d)
}

// TestUVMPreEvictionConcurrentVictims proves multiple pre-eviction victims
// can be in flight concurrently (no fixed depth): two admissions launch two
// victims with distinct ordinals, the concurrency statistics track the
// maximum, and everything converges to the 64 KB headroom.
func TestUVMPreEvictionConcurrentVictims(t *testing.T) {
	d, evmw, faultmw, _ := buildPreEvictionDriver(t, 192*mem.KB)
	ctx := d.Init()
	pid := ctx.pid
	ptr1 := d.AllocateManagedMemory(ctx, 128*mem.KB)
	reg1 := d.uvm.registrations[0]
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr1)+64*mem.KB)
	ptr2 := d.AllocateManagedMemory(ctx, 64*mem.KB)
	ptr3 := d.AllocateManagedMemory(ctx, 64*mem.KB)

	// Fault 1 on alloc 2: fits, headroom short -> victim 1 (region 0) + H2D.
	intakeFault(t, d, pid, 1, uint64(ptr2))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start transaction 1")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service admission 1")
	}
	reqs := drainRequests(d)
	h2d1, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	victim1 := evmw.active
	if victim1 == nil || victim1.RegionBase != 0 {
		t.Fatalf("victim 1 = %+v, want region 0", victim1)
	}
	// Complete fault 1 fully: R = 188 KB.
	deliverGeneralRsp(t, d, h2d1)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete migration 1")
	}
	reqs = drainRequests(d)
	replay1, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay1)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not retire transaction 1")
	}

	// Fault 2 on alloc 3: 188 + 64 > 192 -> hard shortage -> victim 2
	// (region 1) launched to free capacity; the admission queues.
	intakeFault(t, d, pid, 1, uint64(ptr3))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start transaction 2")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service admission 2")
	}
	tx2 := faultmw.active
	if tx2.phase != faultPhaseWaitingCapacity {
		t.Fatalf("phase = %v, want waiting-capacity", tx2.phase)
	}
	if len(d.requestsToSend) != 0 {
		t.Error("H2D emitted on hard shortage")
	}
	if len(evmw.pending) != 1 {
		t.Fatalf("pending victims = %d, want 1", len(evmw.pending))
	}
	victim2 := evmw.pending[0]
	if victim2.RegionBase != 64*mem.KB {
		t.Errorf("victim 2 = %#x, want region 1", victim2.RegionBase)
	}
	if reg1.VABlocks[0].SubBlocks[1].State != RegionEvictPending {
		t.Error("victim 2 region not EVICTING")
	}
	// Two pre-evictions in flight concurrently, distinct ordinals.
	if got := d.uvm.NumPreEvictions(); got != 2 {
		t.Errorf("num pre-evictions = %d, want 2", got)
	}
	if got := d.uvm.NumConcurrentPreEvictions(); got != 2 {
		t.Errorf("concurrent pre-evictions = %d, want 2", got)
	}
	if got := d.uvm.MaxConcurrentPreEvictions(); got != 2 {
		t.Errorf("max concurrent pre-evictions = %d, want 2", got)
	}
	if victim1.Ticket == victim2.Ticket {
		t.Error("victim ordinals not distinct")
	}
	if victim1.Ticket > victim2.Ticket {
		t.Error("victim ordinals not monotonic")
	}
	if got := d.uvm.NumPreEvictionsOverlappedWithH2D(); got != 1 {
		t.Errorf("overlapped with H2D = %d, want 1", got)
	}
	checkUVMInvariant(t, d)

	// Complete victim 1: the eviction middleware immediately promotes
	// victim 2 and starts its block (concurrent with the fault retry).
	driveEvictionToEnd(t, d, evmw, pid)
	if victim1.phase != evictionStageDone {
		t.Fatalf("victim 1 phase = %v, want done", victim1.phase)
	}
	if evmw.active != victim2 {
		t.Fatalf("active = %+v, want victim 2 promoted", evmw.active)
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 block", len(reqs))
	}
	block2, ok := reqs[0].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request = %T, want BlockRange", reqs[0])
	}
	deliverGeneralRsp(t, d, block2)
	evmw.Tick()
	reqs = drainRequests(d)
	flush2, ok := reqs[0].(*protocol.UVMCacheRangeFlushReq)
	if !ok {
		t.Fatalf("request = %T, want UVMCacheRangeFlushReq", reqs[0])
	}
	deliverFlushRsp(t, d, flush2)
	evmw.Tick()
	reqs = drainRequests(d)
	tlb2, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, tlb2)
	evmw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 D2H", len(reqs))
	}
	d2h2, ok := reqs[0].(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyD2HReq", reqs[0])
	}

	// The retry now fits (128 + 64 = 192); victim 2's in-flight bytes cover
	// the headroom, so no new victim is launched. H2D 2 is emitted while
	// victim 2's D2H is already outstanding — concurrent, no admission wait.
	if !faultmw.Tick() {
		t.Fatal("retry tick made no progress")
	}
	if tx2.phase != faultPhaseMigrating {
		t.Fatalf("phase = %v, want migrating", tx2.phase)
	}
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 H2D", len(reqs))
	}
	h2d2, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	if victim2.phase != evictionStageD2H {
		t.Errorf("victim 2 phase = %v, want d2h (D2H concurrent with H2D)",
			victim2.phase)
	}
	if got := d.uvm.NumPreEvictions(); got != 2 {
		t.Errorf("num pre-evictions = %d, want 2 (no extra victim)", got)
	}
	checkUVMInvariant(t, d)

	// Converge: complete H2D 2 (fault path: PTE publish -> TLB -> replay).
	deliverGeneralRsp(t, d, h2d2)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete migration 2")
	}
	reqs = drainRequests(d)
	replay2, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, replay2)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not retire transaction 2")
	}
	if faultmw.active != nil {
		t.Error("fault transaction 2 not retired")
	}

	// Complete victim 2's D2H (eviction path: final PTE -> free -> replay ->
	// unblock).
	deliverGeneralRsp(t, d, d2h2)
	evmw.Tick()
	reqs = drainRequests(d)
	victimReplay2, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-D2H request = %T, want UVMFaultReplayReq", reqs[0])
	}
	deliverReplayAck(t, d, victimReplay2)
	evmw.Tick()
	reqs = drainRequests(d)
	unblock2, ok := reqs[0].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request = %T, want UnblockRange", reqs[0])
	}
	deliverGeneralRsp(t, d, unblock2)
	evmw.Tick()
	if victim2.phase != evictionStageDone {
		t.Fatalf("victim 2 phase = %v, want done", victim2.phase)
	}
	// Converged: R = 128 KB (alloc 2 + alloc 3), free = 64 KB headroom.
	if got := d.uvm.Reservation().ResidentBytes(); got != 128*mem.KB {
		t.Errorf("R = %d, want 128KB", got)
	}
	if free := d.uvm.Reservation().CapacityBytes() -
		d.uvm.Reservation().ResidentBytes(); free != 64*mem.KB {
		t.Errorf("free = %d, want 64KB headroom", free)
	}
	checkUVMInvariant(t, d)
}
