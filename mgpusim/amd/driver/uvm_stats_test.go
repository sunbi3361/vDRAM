package driver

// sbin_codex: complete UVM statistics contract tests (plan todo 22 of
// mgpusim-uvm-manager, uvm-manager.md §27, §11.12, §17.1, §28). These plain
// Go tests prove: the StatisticOwnership table covers every metric of the
// immutable UVMStatsSnapshot exactly once; the §17.1 pre-eviction snapshot
// values are exact through the launch -> H2D/D2H -> completion lifecycle;
// a predeclared identical-root trace fixture produces EXACT cross-mode
// snapshot values (normal vs ideal) with zero ideal fault latency; dynamic
// timing-feedback workloads multiset-match mandatory program-origin roots by
// semantic key with provenance-checked unmatched timing-derived roots that
// contribute to the final byte/accounting equations; and the cross-mode
// schema/DAG comparison never relies on sourceLocalSequence, independent-root
// total order, or root count.

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// TestUVMStatsOwnership proves the StatisticOwnership table is the complete,
// unique owner registry of every metric exposed by the immutable
// UVMStatsSnapshot: every metric-tagged snapshot field has exactly one
// documented owner/update point, and no statistic is owned twice.
func TestUVMStatsOwnership(t *testing.T) {
	// Every metric-tagged field of the snapshot must be owned exactly once.
	seen := make(map[string]string)
	for _, o := range StatisticOwnership {
		if o.Statistic == "" || o.Owner == "" {
			t.Errorf("statistic ownership entry has an empty field: %+v", o)
		}
		if prev, dup := seen[o.Statistic]; dup {
			t.Errorf("statistic %q owned by both %q and %q",
				o.Statistic, prev, o.Owner)
		}
		seen[o.Statistic] = o.Owner
	}

	// The snapshot is the exposure contract: every metric-tagged field must
	// be present in the ownership table.
	snapType := reflect.TypeOf(UVMStatsSnapshot{})
	missing := make([]string, 0)
	for i := 0; i < snapType.NumField(); i++ {
		name := snapType.Field(i).Tag.Get("metric")
		if name == "" {
			continue
		}
		if _, ok := seen[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("snapshot metrics without an ownership entry: %v", missing)
	}

	// The snapshot must expose every §27 / §11.12 / §17.1 metric of the
	// spec contract (the reverse direction: no metric is forgotten).
	contract := []string{
		"num_gpu_page_fault_requests", "num_unique_fault_services",
		"num_coalesced_faults", "fault_service_latency_total",
		"fault_service_latency_avg",
		"num_cpu_to_gpu_migrations", "bytes_cpu_to_gpu",
		"num_gpu_to_cpu_migrations", "bytes_gpu_to_cpu",
		"num_prefetch_migrations", "bytes_prefetched",
		"num_demand_migrations", "bytes_demand_migrated",
		"num_access_counter_migrations", "bytes_access_counter_migrated",
		"num_write_triggered_migrations",
		"num_remote_reads", "num_remote_writes_detected",
		"bytes_remote_read", "pcie_remote_read_transactions",
		"num_access_counter_increments", "num_access_counter_notifications",
		"num_access_counter_threshold_hits",
		"num_evictions", "bytes_evicted", "num_dirty_evictions",
		"num_tbn_prefetch_events", "tbn_prefetch_bytes",
		"tbn_useful_prefetch_bytes", "tbn_unused_prefetch_bytes",
		"num_remote_pte_installs", "num_local_pte_installs",
		"num_uvm_tlb_range_invalidations",
		"num_tbn_fault_events", "num_tbn_64kb_selections",
		"num_tbn_128kb_expansions", "num_tbn_256kb_expansions",
		"num_tbn_512kb_expansions", "num_tbn_1mb_expansions",
		"num_tbn_2mb_expansions",
		"tbn_selected_bytes", "tbn_demand_bytes",
		"tbn_prefetch_candidate_bytes", "tbn_actual_prefetch_dma_bytes",
		"tbn_prefetch_suppressed_resident_bytes",
		"tbn_prefetch_suppressed_inflight_bytes",
		"tbn_useful_prefetched_4kb_pages", "tbn_unused_prefetched_4kb_pages",
		"num_pre_evictions", "bytes_pre_evicted",
		"num_concurrent_pre_evictions", "max_concurrent_pre_evictions",
		"num_pre_evictions_overlapped_with_h2d",
		"migration_wait_cycles_for_capacity",
		"optional_headroom_shortfall_count",
		"optional_headroom_shortfall_bytes",
		"peak_resident_bytes", "uvm_capacity_bytes",
	}
	for _, name := range contract {
		if _, ok := seen[name]; !ok {
			t.Errorf("spec metric %q missing from the ownership table", name)
		}
	}
}

// TestUVMPreEvictionStats drives the projected-occupancy pre-eviction
// lifecycle (launch -> concurrent H2D/D2H -> completion) and asserts the
// exact §17.1 snapshot values plus the §27 eviction/migration counters they
// imply.
func TestUVMPreEvictionStats(t *testing.T) {
	d, evmw, faultmw, _ := buildPreEvictionDriver(t, 192*mem.KB)
	ctx := d.Init()
	pid := ctx.pid
	ptr1 := d.AllocateManagedMemory(ctx, 128*mem.KB)
	// The fault on region 1 selects the [0, 128KB) TBN node: regions 0 and 1
	// become GPU-resident (R = 124 KB, 31 pages: 16 demand + 15 prefetch).
	makeRegionGPUResidentViaFault(t, d, pid, uint64(ptr1)+64*mem.KB)

	snap := d.UVMStats()
	if snap.NumGPUPageFaultRequests != 1 || snap.NumUniqueFaultServices != 1 ||
		snap.NumCoalescedFaults != 0 {
		t.Errorf("fault stats = %d/%d/%d, want 1/1/0",
			snap.NumGPUPageFaultRequests, snap.NumUniqueFaultServices,
			snap.NumCoalescedFaults)
	}
	if snap.NumCPUToGPUMigrations != 1 || snap.BytesCPUToGPU != 124*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 1/124KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumDemandMigrations != 1 || snap.BytesDemandMigrated != 64*mem.KB {
		t.Errorf("demand = %d migrations/%d bytes, want 1/64KB",
			snap.NumDemandMigrations, snap.BytesDemandMigrated)
	}
	if snap.NumPrefetchMigrations != 1 || snap.BytesPrefetched != 60*mem.KB {
		t.Errorf("prefetch = %d migrations/%d bytes, want 1/60KB",
			snap.NumPrefetchMigrations, snap.BytesPrefetched)
	}
	if snap.NumUVMTLBRangeInvalidations != 1 || snap.NumLocalPTEInstalls != 31 {
		t.Errorf("TLB/PTE = %d/%d, want 1/31",
			snap.NumUVMTLBRangeInvalidations, snap.NumLocalPTEInstalls)
	}
	if snap.NumTBNFaultEvents != 1 || snap.NumTBN128KBExpansions != 1 ||
		snap.TBNActualPrefetchDMABytes != 60*mem.KB ||
		snap.NumTBNPrefetchEvents != 1 {
		t.Errorf("TBN = %+v, want 1 event, 1 x 128KB expansion, 60KB prefetch",
			snap)
	}
	if snap.PeakResidentBytes != 124*mem.KB || snap.CapacityBytes != 192*mem.KB {
		t.Errorf("oversubscription = %d/%d, want 124KB/192KB",
			snap.PeakResidentBytes, snap.CapacityBytes)
	}

	// The second admission fits (124 + 64 <= 192) but the headroom is short:
	// reserve + H2D immediately, one deterministic LRU victim concurrently.
	ptr2 := d.AllocateManagedMemory(ctx, 64*mem.KB)
	intakeFault(t, d, pid, 1, uint64(ptr2))
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start the transaction")
	}
	faultmw.Handle(faultmw.active.latencyEvent)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not service the admission")
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 H2D", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyH2DReq", reqs[0])
	}
	if evmw.active == nil || !evmw.active.preEviction {
		t.Fatal("no pre-eviction victim queued")
	}

	snap = d.UVMStats()
	if snap.NumPreEvictions != 1 || snap.BytesPreEvicted != 60*mem.KB {
		t.Errorf("pre-evictions = %d/%d bytes, want 1/60KB",
			snap.NumPreEvictions, snap.BytesPreEvicted)
	}
	if snap.NumConcurrentPreEvictions != 1 ||
		snap.MaxConcurrentPreEvictions != 1 {
		t.Errorf("concurrency = %d/%d, want 1/1",
			snap.NumConcurrentPreEvictions,
			snap.MaxConcurrentPreEvictions)
	}
	if snap.NumPreEvictionsOverlappedWithH2D != 1 {
		t.Errorf("overlapped with H2D = %d, want 1",
			snap.NumPreEvictionsOverlappedWithH2D)
	}
	if snap.MigrationWaitCyclesForCapacity != 0 ||
		snap.OptionalHeadroomShortfallCount != 0 ||
		snap.OptionalHeadroomShortfallBytes != 0 {
		t.Errorf("wait/shortfall = %d/%d/%d, want 0/0/0",
			snap.MigrationWaitCyclesForCapacity,
			snap.OptionalHeadroomShortfallCount,
			snap.OptionalHeadroomShortfallBytes)
	}

	// Complete the H2D (fault path: PTE publish -> TLB -> replay).
	deliverGeneralRsp(t, d, h2d)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not complete the migration")
	}
	reqs = drainRequests(d)
	faultTLB, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, faultTLB)
	if !faultmw.Tick() {
		t.Fatal("fault tick did not start the replay")
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

	// Complete the victim: block -> WB+INV -> TLB -> D2H -> replay ->
	// unblock.
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
	victimTLB, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	deliverTLBAck(t, d, victimTLB)
	evmw.Tick()
	reqs = drainRequests(d)
	d2h, ok := reqs[0].(*protocol.MemCopyD2HReq)
	if !ok {
		t.Fatalf("request = %T, want MemCopyD2HReq", reqs[0])
	}
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

	snap = d.UVMStats()
	// The second fault migrates 16 pages (15 demand + 1 TBN prefetch).
	if snap.NumCPUToGPUMigrations != 2 || snap.BytesCPUToGPU != 188*mem.KB {
		t.Errorf("H2D = %d migrations/%d bytes, want 2/188KB",
			snap.NumCPUToGPUMigrations, snap.BytesCPUToGPU)
	}
	if snap.NumDemandMigrations != 2 || snap.BytesDemandMigrated != 124*mem.KB {
		t.Errorf("demand = %d migrations/%d bytes, want 2/124KB",
			snap.NumDemandMigrations, snap.BytesDemandMigrated)
	}
	if snap.NumPrefetchMigrations != 2 || snap.BytesPrefetched != 64*mem.KB {
		t.Errorf("prefetch = %d migrations/%d bytes, want 2/64KB",
			snap.NumPrefetchMigrations, snap.BytesPrefetched)
	}
	if snap.NumGPUToCPUMigrations != 1 || snap.BytesGPUToCPU != 60*mem.KB {
		t.Errorf("D2H = %d migrations/%d bytes, want 1/60KB",
			snap.NumGPUToCPUMigrations, snap.BytesGPUToCPU)
	}
	if snap.NumEvictions != 1 || snap.BytesEvicted != 60*mem.KB ||
		snap.NumDirtyEvictions != 0 {
		t.Errorf("evictions = %d/%d bytes/%d dirty, want 1/60KB/0",
			snap.NumEvictions, snap.BytesEvicted, snap.NumDirtyEvictions)
	}
	if snap.NumUVMTLBRangeInvalidations != 3 || snap.NumLocalPTEInstalls != 47 {
		t.Errorf("TLB/PTE = %d/%d, want 3/47",
			snap.NumUVMTLBRangeInvalidations, snap.NumLocalPTEInstalls)
	}
	if snap.NumConcurrentPreEvictions != 0 ||
		snap.MaxConcurrentPreEvictions != 1 {
		t.Errorf("concurrency = %d/%d, want 0/1 after completion",
			snap.NumConcurrentPreEvictions,
			snap.MaxConcurrentPreEvictions)
	}
	if snap.NumTBNFaultEvents != 2 || snap.NumTBN128KBExpansions != 1 ||
		snap.NumTBN2MBExpansions != 1 ||
		snap.TBNActualPrefetchDMABytes != 64*mem.KB ||
		snap.NumTBNPrefetchEvents != 2 {
		t.Errorf("TBN = %+v, want 2 events, 128KB + 2MB expansions, 64KB DMA",
			snap)
	}
	if snap.PeakResidentBytes != 188*mem.KB {
		t.Errorf("peak resident = %d, want 188KB", snap.PeakResidentBytes)
	}
	// The fault-service latency is charged once per unique service (20 us).
	if snap.FaultServiceLatencyTotal != sim.VTimeInSec(2*uvmLatencyMicros) {
		t.Errorf("latency total = %v, want 40us", snap.FaultServiceLatencyTotal)
	}
	if snap.FaultServiceLatencyAvg != sim.VTimeInSec(uvmLatencyMicros) {
		t.Errorf("latency avg = %v, want 20us", snap.FaultServiceLatencyAvg)
	}
	if snap.IdealUVM {
		t.Error("normal mode reported ideal")
	}
	checkUVMInvariant(t, d)
}

// uvmLatencyMicros is the default -uvm-fault-handling-latency in seconds
// (20 us), used to assert the latency rows without importing the config
// defaults into the fixture.
const uvmLatencyMicros = 20e-6

// TestUVMPredeclaredCrossModeValues drives the IDENTICAL predeclared trace
// (one 64 KB demand fault serviced to completion) in both modes and proves
// the snapshot values are EXACTLY equal on every functional metric, while
// the ideal mode's UVM latency rows are zero.
func TestUVMPredeclaredCrossModeValues(t *testing.T) {
	run := func(ideal bool) (*Driver, UVMStatsSnapshot) {
		d, mw, _ := buildFaultDriver(t, ideal)
		ctx := d.Init()
		pid := ctx.pid
		ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
		intakeFault(t, d, pid, 1, uint64(ptr))
		tx := mw.queue[0]
		if !mw.Tick() {
			t.Fatal("tick did not start the FIFO head")
		}
		mw.Handle(tx.latencyEvent)
		if !mw.Tick() {
			t.Fatal("tick did not service the transaction")
		}
		reqs := drainRequests(d)
		if len(reqs) != 1 {
			t.Fatalf("service requests = %d, want 1 H2D", len(reqs))
		}
		h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
		if !ok {
			t.Fatalf("service request = %T, want MemCopyH2DReq", reqs[0])
		}
		deliverGeneralRsp(t, d, h2d)
		if !mw.Tick() {
			t.Fatal("tick did not complete the migration")
		}
		reqs = drainRequests(d)
		tlb, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
		if !ok {
			t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
		}
		deliverTLBAck(t, d, tlb)
		if !mw.Tick() {
			t.Fatal("tick did not start the replay")
		}
		reqs = drainRequests(d)
		replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
		if !ok {
			t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
		}
		deliverReplayAck(t, d, replay)
		if !mw.Tick() {
			t.Fatal("tick did not retire the transaction")
		}
		return d, d.UVMStats()
	}

	normal, ns := run(false)
	ideal, is := run(true)

	// Exact cross-mode equality on every functional metric (the latency
	// rows and the mode flag are the only intentional differences).
	nt := reflect.TypeOf(ns)
	nv := reflect.ValueOf(ns)
	iv := reflect.ValueOf(is)
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
	if ns.IdealUVM || !is.IdealUVM {
		t.Errorf("mode flags = %v/%v, want false/true",
			ns.IdealUVM, is.IdealUVM)
	}

	// The predeclared trace's exact values.
	if ns.NumGPUPageFaultRequests != 1 || ns.NumUniqueFaultServices != 1 {
		t.Errorf("faults = %d/%d, want 1/1",
			ns.NumGPUPageFaultRequests, ns.NumUniqueFaultServices)
	}
	if ns.BytesCPUToGPU != 64*mem.KB || ns.BytesDemandMigrated != 60*mem.KB ||
		ns.BytesPrefetched != 4*mem.KB {
		t.Errorf("migration bytes = %d/%d/%d, want 64KB/60KB/4KB",
			ns.BytesCPUToGPU, ns.BytesDemandMigrated, ns.BytesPrefetched)
	}
	if ns.NumUVMTLBRangeInvalidations != 1 || ns.NumLocalPTEInstalls != 16 {
		t.Errorf("TLB/PTE = %d/%d, want 1/16",
			ns.NumUVMTLBRangeInvalidations, ns.NumLocalPTEInstalls)
	}
	if ns.TBNActualPrefetchDMABytes != 4*mem.KB ||
		ns.NumTBNPrefetchEvents != 1 {
		t.Errorf("TBN prefetch = %d bytes/%d events, want 4KB/1",
			ns.TBNActualPrefetchDMABytes, ns.NumTBNPrefetchEvents)
	}

	// Ideal UVM latency rows are zero; normal charges the modeled latency.
	if ns.FaultServiceLatencyTotal <= 0 || ns.FaultServiceLatencyAvg <= 0 {
		t.Errorf("normal latency = %v/%v, want > 0",
			ns.FaultServiceLatencyTotal, ns.FaultServiceLatencyAvg)
	}
	if is.FaultServiceLatencyTotal != 0 || is.FaultServiceLatencyAvg != 0 {
		t.Errorf("ideal latency = %v/%v, want 0/0",
			is.FaultServiceLatencyTotal, is.FaultServiceLatencyAvg)
	}
	if normal.uvmCoordinator.TotalLatency() > 0 &&
		ideal.uvmCoordinator.TotalLatency() != 0 {
		t.Errorf("coordinator latency = %v (normal) / %v (ideal)",
			normal.uvmCoordinator.TotalLatency(),
			ideal.uvmCoordinator.TotalLatency())
	}
}

// ---------------------------------------------------------------------------
// Cross-mode coordinator fixtures (todo 22): the timing-neutral handlers run
// identically in both modes behind the coordinator; only the coordinator
// timing differs. The fixture records functional counters and logical byte
// accounting so the final data/accounting equations can be compared.

const (
	statsFixtureCPUResident = "CPU_RESIDENT"
	statsFixtureMigrating   = "MIGRATING"
	statsFixtureGPUResident = "GPU_RESIDENT"
	statsFixtureEvicting    = "EVICTING"
)

// statsFixtureRegion tracks one 64 KB region's residency state.
type statsFixtureRegion struct {
	state  string
	pinned bool
}

// statsFixture is the compact timing-neutral state machine behind the
// coordinator for the todo-22 cross-mode tests.
type statsFixture struct {
	pid    vm.PID
	gpu    int
	launch uint64
	source string

	regions  map[string]*statsFixtureRegion
	counters map[string]uint64
	bytesH2D uint64
	bytesD2H uint64
	capacity uint64
	refaults map[string]bool
	feedback map[string]bool
	done     map[string]bool
	recency  map[string]uint64
	recencyN uint64
	observed []string
}

func newStatsFixture(pid vm.PID, gpu int, launch uint64, source string) *statsFixture {
	return &statsFixture{
		pid:       pid,
		gpu:       gpu,
		launch:    launch,
		source:    source,
		regions:   make(map[string]*statsFixtureRegion),
		counters:  make(map[string]uint64),
		refaults:  make(map[string]bool),
		feedback:  make(map[string]bool),
		done:      make(map[string]bool),
		recency:   make(map[string]uint64),
	}
}

func (f *statsFixture) addRegion(regionBase uint64, state string) {
	f.regions[fmt.Sprintf("%#x", regionBase)] = &statsFixtureRegion{state: state}
}

func (f *statsFixture) pin(regionBase uint64) {
	f.regions[fmt.Sprintf("%#x", regionBase)].pinned = true
}

func (f *statsFixture) refaultOn(regionBase uint64) {
	f.refaults[fmt.Sprintf("%#x", regionBase)] = true
}

func (f *statsFixture) feedbackOn(regionBase uint64) {
	f.feedback[fmt.Sprintf("%#x", regionBase)] = true
}

func (f *statsFixture) key(
	kind uvm.OriginKind, regionBase uint64, access vm.AccessKind, ordinal uint64,
) uvm.SemanticRootKey {
	return uvm.SemanticRootKey{
		KernelLaunchOrdinal:     f.launch,
		SourceComponentStableID: f.source,
		OriginKind:              kind,
		PID:                     f.pid,
		GPU:                     f.gpu,
		RegionBase:              regionBase,
		AccessKind:              access,
		ProgramCommandOrdinal:   ordinal,
	}
}

func (f *statsFixture) request(
	regionBase uint64, ordinal, seq uint64, at sim.VTimeInSec,
) *uvm.Root {
	return &uvm.Root{
		SemanticKey: f.key(uvm.OriginFaultRequest, regionBase,
			vm.AccessKindRead, ordinal),
		Stamp: uvm.SameModeStamp{
			KernelLaunchOrdinal: f.launch,
			SourceBuildOrdinal:  0,
			SourceLocalSequence: seq,
		},
		Operation:        "fault-request",
		CurrentVTime:     at,
		OperationOrdinal: 1,
	}
}

func (f *statsFixture) region(root *uvm.Root) *statsFixtureRegion {
	return f.regions[fmt.Sprintf("%#x", root.SemanticKey.RegionBase)]
}

func (f *statsFixture) countState(state string) uint64 {
	var n uint64
	for _, reg := range f.regions {
		if reg.state == state {
			n++
		}
	}
	return n
}

func (f *statsFixture) lruVictim() string {
	var best string
	var bestRec uint64
	for base, reg := range f.regions {
		if reg.state != statsFixtureGPUResident || reg.pinned {
			continue
		}
		if best == "" || f.recency[base] < bestRec {
			best = base
			bestRec = f.recency[base]
		}
	}
	return best
}

// admissionGate runs the projected-occupancy gate: free = C-(R+I+N+bytes);
// NeedToEvict = max(0, H-(free+E)) with H = one 64 KB region; a required
// victim is launched only when an unpinned resident victim exists.
func (f *statsFixture) admissionGate(root *uvm.Root, newBytes uint64) []*uvm.Root {
	if f.capacity == 0 {
		return nil
	}
	r := f.countState(statsFixtureGPUResident)
	i := f.countState(statsFixtureMigrating)
	e := f.countState(statsFixtureEvicting)
	free := uint64(0)
	if f.capacity > r+i+newBytes {
		free = f.capacity - (r + i + newBytes)
	}
	need := uint64(0)
	if free+e < 1 {
		need = 1 - (free + e)
	}
	var children []*uvm.Root
	for v := uint64(0); v < need; v++ {
		victim := f.lruVictim()
		if victim == "" {
			break
		}
		regionBase, err := strconv.ParseUint(victim, 0, 64)
		if err != nil {
			panic(err)
		}
		pre := &uvm.Root{
			SemanticKey: f.key(uvm.OriginPreEviction, regionBase,
				vm.AccessKindWrite, root.SemanticKey.ProgramCommandOrdinal),
			Operation:  "pre-eviction",
			EdgeLabel:  "pre-evict",
			Provenance: root.Key(),
		}
		f.regions[victim].state = statsFixtureEvicting
		children = append(children, pre)
	}
	return children
}

func (f *statsFixture) faultRequest(root *uvm.Root) ([]*uvm.Root, string, string, bool) {
	f.counters["fault-request"]++
	reg := f.region(root)
	if reg.state == statsFixtureMigrating {
		return nil, "", "delivered", true
	}
	return []*uvm.Root{{
		SemanticKey: f.key(uvm.OriginFaultService, root.SemanticKey.RegionBase,
			vm.AccessKindRead, root.SemanticKey.ProgramCommandOrdinal),
		Operation:  "fault-service",
		EdgeLabel:  "service",
		Provenance: root.Key(),
	}}, "", "delivered", true
}

func (f *statsFixture) faultService(root *uvm.Root) ([]*uvm.Root, string, string, bool) {
	f.counters["fault-service"]++
	reg := f.region(root)
	switch reg.state {
	case statsFixtureGPUResident:
		return []*uvm.Root{{Operation: "replay", EdgeLabel: "replay"}},
			reg.state, "replayed", true
	case statsFixtureMigrating:
		return nil, reg.state, "coalesced", true
	default:
		children := f.admissionGate(root, 1)
		reg.state = statsFixtureMigrating
		f.bytesH2D += 64 * 1024
		f.recency[fmt.Sprintf("%#x", root.SemanticKey.RegionBase)] = f.recencyN
		f.recencyN++
		children = append(children,
			&uvm.Root{Operation: "dma-h2d", EdgeLabel: "migrate"})
		return children, reg.state, "migrating", true
	}
}

func (f *statsFixture) dmaH2D(root *uvm.Root) ([]*uvm.Root, string, string, bool) {
	f.counters["dma-h2d"]++
	reg := f.region(root)
	reg.state = statsFixtureGPUResident
	root.Bytes = 64 * 1024
	return []*uvm.Root{{Operation: "replay", EdgeLabel: "replay"}},
		reg.state, "h2d-done", true
}

func (f *statsFixture) replay(root *uvm.Root) ([]*uvm.Root, string, string, bool) {
	f.counters["replay"]++
	reg := f.region(root)
	base := fmt.Sprintf("%#x", root.SemanticKey.RegionBase)
	if f.feedback[base] && !f.done[base] {
		f.done[base] = true
		sk := root.SemanticKey
		sk.OriginKind = uvm.OriginFaultRequest
		sk.AccessKind = vm.AccessKindRead
		sk.ProgramCommandOrdinal++
		return []*uvm.Root{{
			SemanticKey: sk,
			Operation:   "fault-request",
			EdgeLabel:   "feedback",
		}}, reg.state, "replayed", true
	}
	return nil, reg.state, "replayed", true
}

func (f *statsFixture) preEviction(root *uvm.Root) ([]*uvm.Root, string, string, bool) {
	f.counters["pre-eviction"]++
	reg := f.region(root)
	reg.state = statsFixtureCPUResident
	root.Bytes = 64 * 1024
	f.bytesD2H += 64 * 1024
	base := fmt.Sprintf("%#x", root.SemanticKey.RegionBase)
	if f.refaults[base] {
		sk := root.SemanticKey
		sk.OriginKind = uvm.OriginFaultRequest
		sk.AccessKind = vm.AccessKindRead
		sk.ProgramCommandOrdinal++
		return []*uvm.Root{{
			SemanticKey:   sk,
			Operation:     "fault-request",
			EdgeLabel:     "refault",
			TimingDerived: true,
			Provenance: provenanceOf(f.pid, f.gpu,
				root.SemanticKey.RegionBase, vm.AccessKindRead),
		}}, reg.state, "pre-evicted", true
	}
	return nil, reg.state, "pre-evicted", true
}

func (f *statsFixture) registerHandlers(c *uvm.Coordinator) {
	c.RegisterHandler("fault-request", uvm.HandlerFunc(f.faultRequest))
	c.RegisterHandler("fault-service", uvm.HandlerFunc(f.faultService))
	c.RegisterHandler("dma-h2d", uvm.HandlerFunc(f.dmaH2D))
	c.RegisterHandler("replay", uvm.HandlerFunc(f.replay))
	c.RegisterHandler("pre-eviction", uvm.HandlerFunc(f.preEviction))
}

func (f *statsFixture) registerProvenance(c *uvm.Coordinator) {
	for _, base := range f.observed {
		c.RegisterProvenance(base)
	}
}

// provenanceOf builds the canonical observed-access provenance string of a
// region (mirrors the coordinator's own helper for the fixture roots).
func provenanceOf(pid vm.PID, gpu int, regionBase uint64, kind vm.AccessKind) string {
	return fmt.Sprintf("observed-access:pid=%d,gpu=%d,region=%#x,kind=%d",
		pid, gpu, regionBase, kind)
}

// statsTransport is the modeled transport of the fixture operations.
func statsTransport(root *uvm.Root) sim.VTimeInSec {
	switch root.Operation {
	case "dma-h2d", "pre-eviction":
		return 10
	case "replay":
		return 4
	default:
		return 3
	}
}

func buildStatsCoordinator(mode uvm.Mode, f *statsFixture) *uvm.Coordinator {
	c := uvm.NewCoordinator(mode)
	c.SetTransport(statsTransport)
	f.registerHandlers(c)
	f.registerProvenance(c)
	return c
}

func drainStatsAll(c *uvm.Coordinator) int {
	total := 0
	for {
		next, ok := c.NextReadyTime()
		if !ok {
			return total
		}
		total += c.Drain(next)
	}
}

// TestUVMUnmatchedServiceRootAccounting proves a timing-derived service root
// in only one mode is accepted only with valid provenance and contributes to
// the final byte/accounting equations: normal runs no pre-eviction (B's
// admission fits, A's admission has no resident victim), ideal pre-evicts A
// whose refault re-migrates it; the final states and the
// migrated - evicted == resident equation match in both modes.
func TestUVMUnmatchedServiceRootAccounting(t *testing.T) {
	run := func(mode uvm.Mode) (*uvm.Coordinator, *statsFixture) {
		f := newStatsFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, statsFixtureCPUResident)
		f.addRegion(0x20000, statsFixtureCPUResident)
		f.pin(0x20000)     // B is pinned: never an eviction victim
		f.refaultOn(0x10000) // A's eviction re-faults (timing-derived)
		f.capacity = 2
		f.observed = append(f.observed,
			provenanceOf(1, 0, 0x10000, vm.AccessKindRead),
			provenanceOf(1, 0, 0x20000, vm.AccessKindRead))
		c := buildStatsCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, 0, 0)) // A
		c.Enqueue(f.request(0x20000, 2, 1, 0)) // B
		drainStatsAll(c)
		return c, f
	}

	normal, fn := run(uvm.ModeNormal)
	ideal, fi := run(uvm.ModeIdeal)

	// Normal: B's admission fits (free = 1 = H); A's admission has no
	// resident victim (B in-flight) -> the optional target is infeasible.
	// Ideal: B's admission sees A resident at capacity -> the pre-eviction
	// of A, whose refault re-migrates A.
	if fn.counters["pre-eviction"] != 0 {
		t.Fatalf("normal pre-eviction count = %d, want 0",
			fn.counters["pre-eviction"])
	}
	if fi.counters["pre-eviction"] != 1 {
		t.Fatalf("ideal pre-eviction count = %d, want 1",
			fi.counters["pre-eviction"])
	}

	m := normal.Match(ideal)
	if len(m.Failures) != 0 {
		t.Fatalf("unmatched roots must be accepted with justification: %v",
			m.Failures)
	}
	if len(m.Unmatched) == 0 {
		t.Fatal("the ideal mode must report its unmatched roots")
	}
	for _, u := range m.Unmatched {
		if u.Mode != uvm.ModeIdeal {
			t.Fatalf("unmatched root in mode %s, want ideal only", u.Mode)
		}
		if u.Root.Provenance == "" && u.Root.ChildKey == nil {
			t.Fatalf("unmatched root without provenance: %s", u.Root.Key())
		}
	}

	// Final-state equality: A and B are GPU-resident in both modes.
	for _, base := range []string{"0x10000", "0x20000"} {
		if fn.regions[base].state != statsFixtureGPUResident {
			t.Fatalf("normal final state of %s = %s, want GPU_RESIDENT",
				base, fn.regions[base].state)
		}
		if fi.regions[base].state != statsFixtureGPUResident {
			t.Fatalf("ideal final state of %s = %s, want GPU_RESIDENT",
				base, fi.regions[base].state)
		}
	}
	// Accounting equations: migrated - evicted == resident in both modes
	// (the unmatched roots contribute to the equations).
	if fn.bytesH2D-fn.bytesD2H != fi.bytesH2D-fi.bytesD2H {
		t.Fatalf("accounting equation = %d (normal) vs %d (ideal)",
			fn.bytesH2D-fn.bytesD2H, fi.bytesH2D-fi.bytesD2H)
	}
	if fi.bytesD2H != 64*1024 {
		t.Fatalf("ideal evicted bytes = %d, want 64KB", fi.bytesD2H)
	}
	if fn.bytesH2D != 2*64*1024 || fi.bytesH2D != 3*64*1024 {
		t.Fatalf("H2D bytes = %d (normal) / %d (ideal), want 128KB/192KB",
			fn.bytesH2D, fi.bytesH2D)
	}

	// Deleting the provenance of the unmatched pre-eviction fails the match.
	f2 := newStatsFixture(1, 0, 3, "gmmu0")
	f2.addRegion(0x10000, statsFixtureCPUResident)
	f2.addRegion(0x20000, statsFixtureCPUResident)
	f2.pin(0x20000)
	f2.refaultOn(0x10000)
	f2.capacity = 2
	f2.observed = append(f2.observed,
		provenanceOf(1, 0, 0x10000, vm.AccessKindRead),
		provenanceOf(1, 0, 0x20000, vm.AccessKindRead))
	c2 := buildStatsCoordinator(uvm.ModeIdeal, f2)
	c2.Enqueue(f2.request(0x10000, 1, 0, 0))
	c2.Enqueue(f2.request(0x20000, 2, 1, 0))
	drainStatsAll(c2)
	for _, r := range c2.ExecutedRoots() {
		if r.SemanticKey.OriginKind == uvm.OriginPreEviction {
			r.Provenance = ""
		}
	}
	m2 := normal.Match(c2)
	found := false
	for _, msg := range m2.Failures {
		if contains(msg, "without valid provenance") {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleting the provenance must fail the match: %v",
			m2.Failures)
	}
}

// TestUVMFeedbackCrossModeSchemaAndDAG proves the dynamic feedback workload
// (A fault -> A replay -> A2 fault with independent B) multiset-matches
// mandatory program-origin roots by semantic key and compares the
// matched-root DAGs — never the sourceLocalSequence, the independent-root
// total order, or the root count.
func TestUVMFeedbackCrossModeSchemaAndDAG(t *testing.T) {
run := func(mode uvm.Mode, seqBase uint64) (*uvm.Coordinator, *statsFixture, []string) {
		f := newStatsFixture(1, 0, 3, "gmmu0")
		f.addRegion(0x10000, statsFixtureCPUResident)
		f.addRegion(0x20000, statsFixtureCPUResident)
		f.feedbackOn(0x10000) // A's replay re-faults exactly once
		f.observed = append(f.observed,
			provenanceOf(1, 0, 0x10000, vm.AccessKindRead),
			provenanceOf(1, 0, 0x20000, vm.AccessKindRead))
		c := buildStatsCoordinator(mode, f)
		c.Enqueue(f.request(0x10000, 1, seqBase, 0))   // A
		c.Enqueue(f.request(0x20000, 2, seqBase+1, 0)) // B
		drainStatsAll(c)
		var order []string
		for _, n := range c.Trace().Nodes() {
			if n.Operation == "fault-request" {
				order = append(order, n.Key)
			}
		}
		return c, f, order
	}

	normal, fn, normalOrder := run(uvm.ModeNormal, 0)
	ideal, fi, idealOrder := run(uvm.ModeIdeal, 9)

	// Normal may total-order A, B, A2; ideal runs the zero-time successors
	// to quiescence: A, A2, then B. The independent-root total order differs
	// and is NOT compared.
	if len(normalOrder) != 3 || len(idealOrder) != 3 {
		t.Fatalf("request orders = %v / %v, want 3 each",
			normalOrder, idealOrder)
	}
	if normalOrder[0] != statsKey(fn, uvm.OriginFaultRequest, 0x10000, 1) ||
		normalOrder[1] != statsKey(fn, uvm.OriginFaultRequest, 0x20000, 2) {
		t.Fatalf("normal order = %v, want A then B", normalOrder)
	}
	if idealOrder[0] != statsKey(fi, uvm.OriginFaultRequest, 0x10000, 1) ||
		!contains(idealOrder[1], "feedback") ||
		idealOrder[2] != statsKey(fi, uvm.OriginFaultRequest, 0x20000, 2) {
		t.Fatalf("ideal order = %v, want A, A2, B", idealOrder)
	}

	// The canonical DAG: A -> A2 (via the replay chain), B unordered. The
	// matched-root DAGs are identical despite the total-order difference.
	if !normal.Trace().Equal(ideal.Trace()) {
		t.Fatal("the matched-root DAGs must be identical across modes")
	}

	// Multiset-match by semantic key: no failures, every mandatory
	// program-origin root paired; the sourceLocalSequence tie-break differs
	// and is excluded from the identity.
	m := normal.Match(ideal)
	if len(m.Failures) != 0 {
		t.Fatalf("cross-mode match failures: %v", m.Failures)
	}
	if len(m.Pairs) == 0 {
		t.Fatal("no matched root pairs")
	}
	for _, p := range m.Pairs {
		// Children inherit the parent's stamp; only the delivered
		// program-origin roots carry their own local sequence.
		if p.A.ChildKey == nil && p.A.Stamp == p.B.Stamp {
			t.Errorf("matched root pair %s has identical stamps (the local "+
				"sequence must differ across modes)", p.A.Key())
		}
	}

	// The functional counters and byte accounting are identical (the schema
	// is shared; the root count is not a comparison criterion).
	for _, key := range []string{"fault-request", "fault-service",
		"dma-h2d", "replay"} {
		if fn.counters[key] != fi.counters[key] {
			t.Fatalf("counter %s = %d (normal) vs %d (ideal)",
				key, fn.counters[key], fi.counters[key])
		}
	}
	if fn.bytesH2D != fi.bytesH2D || fn.bytesD2H != fi.bytesD2H {
		t.Fatalf("bytes = %d/%d (normal) vs %d/%d (ideal)",
			fn.bytesH2D, fn.bytesD2H, fi.bytesH2D, fi.bytesD2H)
	}
	if normal.TotalLatency() <= 0 || ideal.TotalLatency() != 0 {
		t.Fatalf("latency = %v (normal) / %v (ideal), want > 0 / 0",
			normal.TotalLatency(), ideal.TotalLatency())
	}
}

// statsKey builds the canonical key string of a fixture root.
func statsKey(f *statsFixture, kind uvm.OriginKind, regionBase uint64, ordinal uint64) string {
	return f.key(kind, regionBase, vm.AccessKindRead, ordinal).String()
}

// contains reports whether s contains the substring.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}