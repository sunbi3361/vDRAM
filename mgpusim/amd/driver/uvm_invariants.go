package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_codex: §28 invariant assertions and statistics-ownership documentation.
// Each check returns a descriptive error naming the violated invariant and the
// PID/GPU/block/region context; nil means the invariant holds. The
// access-counter and write invariants are enforced at the API level
// (SubBlockState.RecordRemoteAccess, RegionStateMachine.GPUWrite).

// InvariantContext binds the authoritative masks, a VA block, one of its
// regions, and the capacity reservation for a set of invariant checks.
type InvariantContext struct {
	PID         vm.PID
	GPU         int
	Block       *VABlock
	BlockIdx    uint64
	Region      *SubBlockState
	RegionIdx   uint64
	Reg         *ManagedAllocationRegistration
	Reservation *AdmissionReservation
}

// regionPageRange returns the first allocation page index and the count of
// valid (allocated) pages in the region, computed from VA ranges so a
// misaligned allocation (base not 64 KB-aligned) maps correctly.
func (c *InvariantContext) regionPageRange() (allocStart, valid uint64) {
	regionVA := c.Block.StartVA + c.RegionIdx*subblockSizeBytes
	allocEndVA := c.Reg.Base + c.Reg.PageCount*basePageSize
	lo := regionVA
	if lo < c.Reg.Base {
		lo = c.Reg.Base
	}
	hi := regionVA + subblockSizeBytes
	if hi > allocEndVA {
		hi = allocEndVA
	}
	if lo >= hi {
		return 0, 0
	}
	return (lo - c.Reg.Base) / basePageSize, (hi - lo) / basePageSize
}

// blockLocalPage returns the block-local page index (0..511) for an allocation
// page, derived from VA so misaligned allocations map correctly.
func (c *InvariantContext) blockLocalPage(allocPage uint64) uint64 {
	return (c.Reg.Base + allocPage*basePageSize - c.Block.StartVA) / basePageSize
}

// maskBit reports whether bit `page` of mask is set.
func maskBit(mask []uint64, page uint64) bool {
	return mask[page/64]&(uint64(1)<<(page%64)) != 0
}

// CheckResidencyAuthority verifies §28 "Residency": the region's transaction
// state must agree with its pages' authoritative GPU residency (the mask) — a
// region must not simultaneously hold two authoritative residences. Migrating
// states are explicitly modeled and exempt.
func (c *InvariantContext) CheckResidencyAuthority() error {
	allocStart, valid := c.regionPageRange()
	resident := uint64(0)
	for i := uint64(0); i < valid; i++ {
		if maskBit(c.Reg.ResidentMask, allocStart+i) {
			resident++
		}
	}
	switch c.Region.State {
	case RegionGPUResident:
		if resident != valid {
			return fmt.Errorf(
				"uvm: invariant residency: region pid=%d gpu=%d block=%d region=%d is GPU_RESIDENT with %d/%d resident pages",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, resident, valid)
		}
	case RegionCPUResident, RegionIDLE:
		if resident != 0 {
			return fmt.Errorf(
				"uvm: invariant residency: region pid=%d gpu=%d block=%d region=%d is %s with %d resident pages",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, c.Region.State, resident)
		}
	}
	return nil
}

// CheckGPUPhysicalAllocation verifies §28 "GPU Physical Allocation":
// GPU_RESIDENT => valid GPU physical page exists.
func (c *InvariantContext) CheckGPUPhysicalAllocation() error {
	allocStart, valid := c.regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		if maskBit(c.Reg.ResidentMask, page) &&
			c.Block.Pages[c.blockLocalPage(page)].GPUPhysicalPage == 0 {
			return fmt.Errorf(
				"uvm: invariant gpu-pa: region pid=%d gpu=%d block=%d region=%d page %d resident with no GPU physical page",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, page)
		}
	}
	return nil
}

// CheckRemoteMapping verifies §28 "Remote Mapping": REMOTE mapping => CPU
// backing page exists.
func (c *InvariantContext) CheckRemoteMapping() error {
	allocStart, valid := c.regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := &c.Block.Pages[c.blockLocalPage(page)]
		if p.RemoteMapped && p.CPUPhysicalPage == 0 {
			return fmt.Errorf(
				"uvm: invariant remote: region pid=%d gpu=%d block=%d region=%d page %d remote-mapped with no CPU backing",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, page)
		}
	}
	return nil
}

// CheckRemoteCacheability verifies §28 "Remote Cacheability": CPU_REMOTE data
// must never be inserted into GPU data caches. A page is CPU_REMOTE when it is
// remote-mapped and not GPU-resident; such pages must not be CachedOnGPU.
func (c *InvariantContext) CheckRemoteCacheability() error {
	allocStart, valid := c.regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := &c.Block.Pages[c.blockLocalPage(page)]
		remote := p.RemoteMapped && !maskBit(c.Reg.ResidentMask, page)
		if remote && p.CachedOnGPU {
			return fmt.Errorf(
				"uvm: invariant cacheability: region pid=%d gpu=%d block=%d region=%d page %d CPU_REMOTE cached on GPU",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, page)
		}
	}
	return nil
}

// CheckOversubscription verifies §28 "Oversubscription": R+I+N <= C. In-flight
// bytes are the explicitly modeled transient of an atomic migration/eviction
// transaction and remain bounded by the reservation.
func (c *InvariantContext) CheckOversubscription() error {
	r, i, n := c.Reservation.ResidentBytes(),
		c.Reservation.InFlightBytes(), c.Reservation.ReservedBytes()
	if r+i+n > c.Reservation.CapacityBytes() {
		return fmt.Errorf(
			"uvm: invariant oversubscription: pid=%d gpu=%d R+I+N=%d exceeds capacity %d",
			c.PID, c.GPU, r+i+n, c.Reservation.CapacityBytes())
	}
	return nil
}

// CheckAll runs every §28 invariant check and returns the first violation.
func (c *InvariantContext) CheckAll() error {
	for _, check := range []func() error{
		c.CheckResidencyAuthority,
		c.CheckGPUPhysicalAllocation,
		c.CheckRemoteMapping,
		c.CheckRemoteCacheability,
		c.CheckOversubscription,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// StatisticOwner documents the single update point for one UVM statistic.
// sbin_codex (todo 22): the table now covers every §27 metric, every §11.12
// detailed TBN metric, and every §17.1 pre-eviction metric exposed by the
// immutable UVMStatsSnapshot (Driver.UVMStats / extendedReporter SQLite
// rows). Each statistic is updated by exactly one function; the invariant
// test asserts uniqueness and completeness.
type StatisticOwner struct {
	Statistic string
	Owner     string
}

// StatisticOwnership is the authoritative owner table. Each statistic is
// updated by exactly one function; the invariant test asserts uniqueness.
// sbin_codex (todo 22): full §27 + §11.12 + §17.1 coverage.
var StatisticOwnership = []StatisticOwner{
	// Internal capacity/residency statistics (plan todos 4/20).
	{Statistic: "resident_bytes_gpu", Owner: "AdmissionReservation.CommitAdmission / CompleteMigrationToGPU"},
	{Statistic: "in_flight_bytes", Owner: "AdmissionReservation.StartMigration"},
	{Statistic: "reserved_bytes", Owner: "AdmissionReservation.ReserveAdmission"},
	{Statistic: "access_counter", Owner: "SubBlockState.RecordRemoteAccess"},
	{Statistic: "migration_recency", Owner: "SubBlockState.RecordMigration"},
	{Statistic: "suppressed_migrations", Owner: "UVMManager.intakeMigration"},

	// §27 Faults.
	{Statistic: "num_gpu_page_fault_requests", Owner: "UVMManager.intakePageFault (uvmStats.recordRawFault)"},
	{Statistic: "num_unique_fault_services", Owner: "UVMManager.intakePageFault (uvmStats.recordUniqueFaultService)"},
	{Statistic: "num_coalesced_faults", Owner: "UVMManager.intakePageFault (uvmStats.recordCoalescedFault)"},
	{Statistic: "fault_service_latency_total", Owner: "faultServiceMiddleware.chargeLatency (uvmStats.recordFaultServiceLatency)"},
	{Statistic: "fault_service_latency_avg", Owner: "derived from fault_service_latency_total / count at snapshot time (safe average)"},

	// §27 Migration.
	{Statistic: "num_cpu_to_gpu_migrations", Owner: "faultServiceMiddleware.completeMigration / migrationMiddleware.publish (uvmStats.recordCPUToGPUMigration)"},
	{Statistic: "bytes_cpu_to_gpu", Owner: "faultServiceMiddleware.completeMigration / migrationMiddleware.publish (uvmStats.recordCPUToGPUMigration)"},
	{Statistic: "num_gpu_to_cpu_migrations", Owner: "evictionD2HTransfer.writeback (uvmStats.recordGPUToCPUMigration)"},
	{Statistic: "bytes_gpu_to_cpu", Owner: "evictionD2HTransfer.writeback (uvmStats.recordGPUToCPUMigration)"},
	{Statistic: "num_prefetch_migrations", Owner: "faultServiceMiddleware.completeMigration (uvmStats.recordPrefetchMigration)"},
	{Statistic: "bytes_prefetched", Owner: "faultServiceMiddleware.completeMigration (uvmStats.recordPrefetchMigration)"},
	{Statistic: "num_demand_migrations", Owner: "faultServiceMiddleware.completeMigration (uvmStats.recordDemandMigration)"},
	{Statistic: "bytes_demand_migrated", Owner: "faultServiceMiddleware.completeMigration (uvmStats.recordDemandMigration)"},
	{Statistic: "num_access_counter_migrations", Owner: "UVMManager.intakeMigration (uvmStats.recordAccessCounterMigration)"},
	{Statistic: "bytes_access_counter_migrated", Owner: "migrationMiddleware.publish (uvmStats.recordAccessCounterMigratedBytes)"},
	{Statistic: "num_write_triggered_migrations", Owner: "UVMManager.intakeMigration (uvmStats.recordWriteTriggeredMigration)"},

	// §27 Remote access.
	{Statistic: "num_remote_reads", Owner: "migrationMiddleware.intakeNotification (uvmStats.recordRemoteReads)"},
	{Statistic: "bytes_remote_read", Owner: "migrationMiddleware.intakeNotification (uvmStats.recordRemoteReads)"},
	{Statistic: "pcie_remote_read_transactions", Owner: "migrationMiddleware.intakeNotification (uvmStats.recordRemoteReads)"},
	{Statistic: "num_remote_writes_detected", Owner: "migrationMiddleware.intakeRemoteWrite (uvmStats.recordRemoteWriteDetected)"},

	// §27 Access counter.
	{Statistic: "num_access_counter_increments", Owner: "migrationMiddleware.intakeNotification (uvmStats.recordAccessCounterIncrements)"},
	{Statistic: "num_access_counter_notifications", Owner: "migrationMiddleware.intakeNotification (uvmStats.recordAccessCounterNotification)"},
	{Statistic: "num_access_counter_threshold_hits", Owner: "migrationMiddleware.intakeNotification (uvmStats.recordAccessCounterNotification)"},

	// §27 Eviction.
	{Statistic: "num_evictions", Owner: "UVMManager.freeEvictionFrames (uvmStats.recordEviction)"},
	{Statistic: "bytes_evicted", Owner: "UVMManager.freeEvictionFrames (uvmStats.recordEviction)"},
	{Statistic: "num_dirty_evictions", Owner: "UVMManager.freeEvictionFrames (uvmStats.recordEviction, DirtyMask check)"},

	// §27 TBN summary (derived from the §11.12 detailed counters).
	{Statistic: "num_tbn_prefetch_events", Owner: "UVMManager.recomputeTBN (tbnStatistics.PrefetchEvents)"},
	{Statistic: "tbn_prefetch_bytes", Owner: "derived from tbn_actual_prefetch_dma_bytes at snapshot time"},
	{Statistic: "tbn_useful_prefetch_bytes", Owner: "derived from tbn_useful_prefetched_4kb_pages at snapshot time"},
	{Statistic: "tbn_unused_prefetch_bytes", Owner: "derived from tbn_unused_prefetched_4kb_pages at snapshot time"},

	// §27 TLB / mapping.
	{Statistic: "num_remote_pte_installs", Owner: "evictionMiddleware.finalPTE (uvmStats.recordRemotePTEInstalls)"},
	{Statistic: "num_local_pte_installs", Owner: "faultServiceMiddleware.completeMigration / migrationMiddleware.publish (uvmStats.recordLocalPTEInstalls)"},
	{Statistic: "num_uvm_tlb_range_invalidations", Owner: "fault/migration/eviction startTLBI (uvmStats.recordUVMTLBRangeInvalidation)"},

	// §11.12 detailed TBN.
	{Statistic: "num_tbn_fault_events", Owner: "UVMManager.recomputeTBN (tbnStatistics.FaultEvents)"},
	{Statistic: "num_tbn_64kb_selections", Owner: "UVMManager.recomputeTBN (tbnStatistics.Selections64KB)"},
	{Statistic: "num_tbn_128kb_expansions", Owner: "UVMManager.recomputeTBN (tbnStatistics.Expansions128KB)"},
	{Statistic: "num_tbn_256kb_expansions", Owner: "UVMManager.recomputeTBN (tbnStatistics.Expansions256KB)"},
	{Statistic: "num_tbn_512kb_expansions", Owner: "UVMManager.recomputeTBN (tbnStatistics.Expansions512KB)"},
	{Statistic: "num_tbn_1mb_expansions", Owner: "UVMManager.recomputeTBN (tbnStatistics.Expansions1MB)"},
	{Statistic: "num_tbn_2mb_expansions", Owner: "UVMManager.recomputeTBN (tbnStatistics.Expansions2MB)"},
	{Statistic: "tbn_selected_bytes", Owner: "UVMManager.recomputeTBN (tbnStatistics.SelectedBytes)"},
	{Statistic: "tbn_demand_bytes", Owner: "UVMManager.recomputeTBN (tbnStatistics.DemandBytes)"},
	{Statistic: "tbn_prefetch_candidate_bytes", Owner: "UVMManager.recomputeTBN (tbnStatistics.PrefetchCandidateBytes)"},
	{Statistic: "tbn_actual_prefetch_dma_bytes", Owner: "UVMManager.recomputeTBN (tbnStatistics.ActualPrefetchDMABytes)"},
	{Statistic: "tbn_prefetch_suppressed_resident_bytes", Owner: "UVMManager.recomputeTBN (tbnStatistics.SuppressedResidentBytes)"},
	{Statistic: "tbn_prefetch_suppressed_inflight_bytes", Owner: "UVMManager.recomputeTBN (tbnStatistics.SuppressedInflightBytes)"},
	{Statistic: "tbn_useful_prefetched_4kb_pages", Owner: "UVMManager.recomputeTBN (tbnStatistics.UsefulPrefetchedPages)"},
	{Statistic: "tbn_unused_prefetched_4kb_pages", Owner: "UVMManager.recomputeTBN (tbnStatistics.UnusedPrefetchedPages)"},

	// §17.1 pre-eviction.
	{Statistic: "num_pre_evictions", Owner: "UVMManager.launchPreEvictionVictimsLocked (preEvictionStats.numPreEvictions)"},
	{Statistic: "bytes_pre_evicted", Owner: "UVMManager.launchPreEvictionVictimsLocked (preEvictionStats.bytesPreEvicted)"},
	{Statistic: "num_concurrent_pre_evictions", Owner: "preEvictionStats lifecycle: launchPreEvictionVictimsLocked / completeEviction / abortEviction"},
	{Statistic: "max_concurrent_pre_evictions", Owner: "UVMManager.launchPreEvictionVictimsLocked (preEvictionStats.maxConcurrentPreEvictions)"},
	{Statistic: "num_pre_evictions_overlapped_with_h2d", Owner: "UVMManager.launchPreEvictionVictimsLocked (preEvictionStats.numPreEvictionsOverlappedWithH2D)"},
	{Statistic: "migration_wait_cycles_for_capacity", Owner: "UVMManager.recordCapacityWait (preEvictionStats.migrationWaitCyclesForCapacity)"},
	{Statistic: "optional_headroom_shortfall_count", Owner: "UVMManager.launchPreEvictionVictimsLocked (preEvictionStats.optionalHeadroomShortfallCount)"},
	{Statistic: "optional_headroom_shortfall_bytes", Owner: "UVMManager.launchPreEvictionVictimsLocked (preEvictionStats.optionalHeadroomShortfallBytes)"},

	// §28 oversubscription diagnostics.
	{Statistic: "peak_resident_bytes", Owner: "UVMManager.completeMigrationAdmission (uvmStats.recordResidentBytes)"},
	{Statistic: "uvm_capacity_bytes", Owner: "UVMManager.NewUVMManager (fixed at construction)"},

	// Mode flag (not a statistic).
	{Statistic: "uvm_mode", Owner: "UVMConfig.Ideal (immutable mode flag)"},
}
