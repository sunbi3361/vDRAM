package driver

// sbin_codex: complete UVM statistics (plan todo 22 of mgpusim-uvm-manager,
// uvm-manager.md §27, §11.12, §17.1, §28). uvmStats is the single owner of
// every UVM counter; each metric has exactly one documented update point
// (the StatisticOwnership table in uvm_invariants.go). UVMStatsSnapshot is
// the immutable point-in-time exposure: Driver.UVMStats() returns a value
// copy taken under the manager lock, and the samples runner's
// extendedReporter emits one SQLite row per metric-tagged snapshot field.
// Ideal mode (-uvm-ideal) shares the same counters and update points — no
// duplicate "ideal_" counters exist (uvm-manager.md §27: "The implementation
// does not need duplicate ideal_ counters if the same standard counters are
// used unchanged in both modes").

import (
	"github.com/sarchlab/akita/v4/sim"
)

// tbnStatistics records the TBN selection counters (uvm-manager.md §11.12).
// Each fault event records exactly one selection; the level counters
// partition the selections by their final level (64 KB selections vs
// 128 KB .. 2 MB expansions). The byte counters accumulate the per-selection
// accounting of tbnSelection. The single update point is
// UVMManager.recomputeTBN (uvm_tbn.go). sbin_codex
//
// Moved from uvm_tbn.go by todo 22 so uvm_stats.go owns every UVM counter.
type tbnStatistics struct {
	FaultEvents             uint64 // num_tbn_fault_events
	Selections64KB          uint64 // num_tbn_64kb_selections
	Expansions128KB         uint64 // num_tbn_128kb_expansions
	Expansions256KB         uint64 // num_tbn_256kb_expansions
	Expansions512KB         uint64 // num_tbn_512kb_expansions
	Expansions1MB           uint64 // num_tbn_1mb_expansions
	Expansions2MB           uint64 // num_tbn_2mb_expansions
	SelectedBytes           uint64 // tbn_selected_bytes
	DemandBytes             uint64 // tbn_demand_bytes
	PrefetchCandidateBytes  uint64 // tbn_prefetch_candidate_bytes
	ActualPrefetchDMABytes  uint64 // tbn_actual_prefetch_dma_bytes
	SuppressedResidentBytes uint64 // tbn_prefetch_suppressed_resident_bytes
	SuppressedInflightBytes uint64 // tbn_prefetch_suppressed_inflight_bytes
	UsefulPrefetchedPages   uint64 // tbn_useful_prefetched_4kb_pages
	UnusedPrefetchedPages   uint64 // tbn_unused_prefetched_4kb_pages
	// PrefetchEvents is the §27 num_tbn_prefetch_events counter: selections
	// whose actual prefetch DMA bytes are nonzero. sbin_codex (todo 22)
	PrefetchEvents uint64
}

// preEvictionStats tracks the §17.1 pre-eviction statistics. The update
// points are UVMManager.launchPreEvictionVictimsLocked (launch: counts,
// bytes, concurrency, overlap, shortfall), UVMManager.recordCapacityWait
// (wait cycles), and UVMManager.completeEviction / abortEviction (the live
// concurrent count). sbin_codex
//
// Moved from uvm_preevict.go by todo 22 so uvm_stats.go owns every UVM
// counter.
type preEvictionStats struct {
	numPreEvictions                  uint64
	bytesPreEvicted                  uint64
	numConcurrentPreEvictions        uint64
	maxConcurrentPreEvictions        uint64
	numPreEvictionsOverlappedWithH2D uint64
	migrationWaitCyclesForCapacity   uint64
	optionalHeadroomShortfallCount   uint64
	optionalHeadroomShortfallBytes   uint64
}

// uvmStats is the single owner of every UVM statistic. It is embedded in
// UVMManager as `stats`; every counter is updated by exactly one documented
// function (see StatisticOwnership). All fields are guarded by the manager
// lock. sbin_codex
type uvmStats struct {
	// §11.12 detailed TBN statistics (update point: recomputeTBN).
	tbn tbnStatistics
	// §17.1 pre-eviction statistics (update points: the pre-eviction gate
	// and the eviction completion/abort).
	preEviction preEvictionStats

	// §27 Faults (update point: UVMManager.intakePageFault).
	numGPUPageFaultRequests uint64
	numUniqueFaultServices  uint64
	numCoalescedFaults      uint64

	// §27 fault-service latency (update point:
	// faultServiceMiddleware.chargeLatency; ideal mode charges zero).
	faultServiceLatencyTotal sim.VTimeInSec
	faultServiceLatencyCount uint64

	// §27 Migration H2D (update points:
	// faultServiceMiddleware.completeMigration for the demand/prefetch
	// breakdown, migrationMiddleware.publish for the AC/write breakdown).
	numCPUToGPUMigrations       uint64
	bytesCPUToGPU               uint64
	numDemandMigrations         uint64
	bytesDemandMigrated         uint64
	numPrefetchMigrations       uint64
	bytesPrefetched             uint64
	numAccessCounterMigrations  uint64
	bytesAccessCounterMigrated  uint64
	numWriteTriggeredMigrations uint64

	// §27 Migration D2H (update point: evictionD2HTransfer.writeback).
	numGPUToCPUMigrations uint64
	bytesGPUToCPU         uint64

	// §27 Remote access (update points:
	// migrationMiddleware.intakeNotification for the reads observed through
	// the notification seam, migrationMiddleware.intakeRemoteWrite for the
	// detected writes).
	numRemoteReads             uint64
	bytesRemoteRead            uint64
	pcieRemoteReadTransactions uint64
	numRemoteWritesDetected    uint64

	// §27 Access counter (update point:
	// migrationMiddleware.intakeNotification: each notification carries its
	// region's counter value at the threshold crossing).
	numAccessCounterIncrements    uint64
	numAccessCounterNotifications uint64
	numAccessCounterThresholdHits uint64

	// §27 Eviction (update point: UVMManager.freeEvictionFrames).
	numEvictions      uint64
	bytesEvicted      uint64
	numDirtyEvictions uint64

	// §27 TLB / mapping (update points: the three startTLBI sites for the
	// range invalidations, the fault/migration publish sites for the local
	// PTE installs, and evictionMiddleware.finalPTE for the remote PTE
	// installs).
	numRemotePTEInstalls        uint64
	numLocalPTEInstalls         uint64
	numUVMTLBRangeInvalidations uint64

	// §28 oversubscription diagnostics (update points:
	// UVMManager.completeMigrationAdmission for the peak resident bytes,
	// UVMManager.NewUVMManager for the configured capacity).
	peakResidentBytes uint64
	capacityBytes     uint64
}

// recordRawFault is the one update point of num_gpu_page_fault_requests.
// sbin_codex
func (s *uvmStats) recordRawFault() { s.numGPUPageFaultRequests++ }

// recordUniqueFaultService is the one update point of
// num_unique_fault_services. sbin_codex
func (s *uvmStats) recordUniqueFaultService() { s.numUniqueFaultServices++ }

// recordCoalescedFault is the one update point of num_coalesced_faults.
// sbin_codex
func (s *uvmStats) recordCoalescedFault() { s.numCoalescedFaults++ }

// recordFaultServiceLatency is the one update point of
// fault_service_latency_total (and its average's denominator). sbin_codex
func (s *uvmStats) recordFaultServiceLatency(latency sim.VTimeInSec) {
	s.faultServiceLatencyTotal += latency
	s.faultServiceLatencyCount++
}

// recordCPUToGPUMigration is the one update point of
// num_cpu_to_gpu_migrations and bytes_cpu_to_gpu. sbin_codex
func (s *uvmStats) recordCPUToGPUMigration(bytes uint64) {
	s.numCPUToGPUMigrations++
	s.bytesCPUToGPU += bytes
}

// recordDemandMigration is the one update point of num_demand_migrations and
// bytes_demand_migrated. sbin_codex
func (s *uvmStats) recordDemandMigration(bytes uint64) {
	s.numDemandMigrations++
	s.bytesDemandMigrated += bytes
}

// recordPrefetchMigration is the one update point of num_prefetch_migrations
// and bytes_prefetched. sbin_codex
func (s *uvmStats) recordPrefetchMigration(bytes uint64) {
	s.numPrefetchMigrations++
	s.bytesPrefetched += bytes
}

// recordAccessCounterMigration is the one update point of
// num_access_counter_migrations: a threshold-triggered migration transaction
// is created (uvm-manager.md §16). sbin_codex
func (s *uvmStats) recordAccessCounterMigration() {
	s.numAccessCounterMigrations++
}

// recordAccessCounterMigratedBytes is the one update point of
// bytes_access_counter_migrated: the H2D bytes of a completed
// threshold-triggered migration. sbin_codex
func (s *uvmStats) recordAccessCounterMigratedBytes(bytes uint64) {
	s.bytesAccessCounterMigrated += bytes
}

// recordWriteTriggeredMigration is the one update point of
// num_write_triggered_migrations: a write-triggered migration transaction is
// created. sbin_codex
func (s *uvmStats) recordWriteTriggeredMigration() {
	s.numWriteTriggeredMigrations++
}

// recordGPUToCPUMigration is the one update point of
// num_gpu_to_cpu_migrations and bytes_gpu_to_cpu. sbin_codex
func (s *uvmStats) recordGPUToCPUMigration(bytes uint64) {
	s.numGPUToCPUMigrations++
	s.bytesGPUToCPU += bytes
}

// recordRemoteReads is the one update point of num_remote_reads,
// bytes_remote_read, and pcie_remote_read_transactions. count is the number
// of remote accesses observed (each remote access is one PCIe read
// transaction of one 4 KB page). sbin_codex
func (s *uvmStats) recordRemoteReads(count uint64) {
	s.numRemoteReads += count
	s.bytesRemoteRead += count * basePageSize
	s.pcieRemoteReadTransactions += count
}

// recordRemoteWriteDetected is the one update point of
// num_remote_writes_detected. sbin_codex
func (s *uvmStats) recordRemoteWriteDetected() {
	s.numRemoteWritesDetected++
}

// recordAccessCounterIncrements is the one update point of
// num_access_counter_increments. sbin_codex
func (s *uvmStats) recordAccessCounterIncrements(count uint64) {
	s.numAccessCounterIncrements += count
}

// recordAccessCounterNotification is the one update point of
// num_access_counter_notifications and num_access_counter_threshold_hits
// (each received notification is one threshold crossing). sbin_codex
func (s *uvmStats) recordAccessCounterNotification() {
	s.numAccessCounterNotifications++
	s.numAccessCounterThresholdHits++
}

// recordEviction is the one update point of num_evictions, bytes_evicted,
// and num_dirty_evictions. sbin_codex
func (s *uvmStats) recordEviction(bytes uint64, dirty bool) {
	s.numEvictions++
	s.bytesEvicted += bytes
	if dirty {
		s.numDirtyEvictions++
	}
}

// recordRemotePTEInstalls is the one update point of
// num_remote_pte_installs. sbin_codex
func (s *uvmStats) recordRemotePTEInstalls(count uint64) {
	s.numRemotePTEInstalls += count
}

// recordLocalPTEInstalls is the one update point of num_local_pte_installs.
// sbin_codex
func (s *uvmStats) recordLocalPTEInstalls(count uint64) {
	s.numLocalPTEInstalls += count
}

// recordUVMTLBRangeInvalidation is the one update point of
// num_uvm_tlb_range_invalidations. sbin_codex
func (s *uvmStats) recordUVMTLBRangeInvalidation() {
	s.numUVMTLBRangeInvalidations++
}

// recordResidentBytes is the one update point of peak_resident_bytes: it
// tracks the peak committed resident bytes R. sbin_codex
func (s *uvmStats) recordResidentBytes(resident uint64) {
	if resident > s.peakResidentBytes {
		s.peakResidentBytes = resident
	}
}

// UVMStatsSnapshot is the immutable point-in-time snapshot of every UVM
// statistic (uvm-manager.md §27, §11.12, §17.1, §28). It is produced by
// UVMManager.Snapshot under the manager lock; callers must treat it as
// read-only. Each field carries the SQLite metric name and unit used by the
// samples runner's extendedReporter. Averages are computed safely (zero when
// no observation exists). sbin_codex
type UVMStatsSnapshot struct {
	// uvm_mode reports whether the ideal (-uvm-ideal) mode is active (1) or
	// the normal mode (0). It is a mode flag, not a statistic.
	IdealUVM bool `metric:"uvm_mode" unit:"flag"`

	// §27 Faults.
	NumGPUPageFaultRequests uint64 `metric:"num_gpu_page_fault_requests" unit:"count"`
	NumUniqueFaultServices  uint64 `metric:"num_unique_fault_services" unit:"count"`
	NumCoalescedFaults      uint64 `metric:"num_coalesced_faults" unit:"count"`
	FaultServiceLatencyTotal sim.VTimeInSec `metric:"fault_service_latency_total" unit:"second"`
	FaultServiceLatencyAvg   sim.VTimeInSec `metric:"fault_service_latency_avg" unit:"second"`

	// §27 Migration.
	NumCPUToGPUMigrations       uint64 `metric:"num_cpu_to_gpu_migrations" unit:"count"`
	BytesCPUToGPU               uint64 `metric:"bytes_cpu_to_gpu" unit:"bytes"`
	NumGPUToCPUMigrations       uint64 `metric:"num_gpu_to_cpu_migrations" unit:"count"`
	BytesGPUToCPU               uint64 `metric:"bytes_gpu_to_cpu" unit:"bytes"`
	NumPrefetchMigrations       uint64 `metric:"num_prefetch_migrations" unit:"count"`
	BytesPrefetched             uint64 `metric:"bytes_prefetched" unit:"bytes"`
	NumDemandMigrations         uint64 `metric:"num_demand_migrations" unit:"count"`
	BytesDemandMigrated         uint64 `metric:"bytes_demand_migrated" unit:"bytes"`
	NumAccessCounterMigrations  uint64 `metric:"num_access_counter_migrations" unit:"count"`
	BytesAccessCounterMigrated  uint64 `metric:"bytes_access_counter_migrated" unit:"bytes"`
	NumWriteTriggeredMigrations uint64 `metric:"num_write_triggered_migrations" unit:"count"`

	// §27 Remote access.
	NumRemoteReads             uint64 `metric:"num_remote_reads" unit:"count"`
	NumRemoteWritesDetected    uint64 `metric:"num_remote_writes_detected" unit:"count"`
	BytesRemoteRead            uint64 `metric:"bytes_remote_read" unit:"bytes"`
	PCIeRemoteReadTransactions uint64 `metric:"pcie_remote_read_transactions" unit:"count"`

	// §27 Access counter.
	NumAccessCounterIncrements    uint64 `metric:"num_access_counter_increments" unit:"count"`
	NumAccessCounterNotifications uint64 `metric:"num_access_counter_notifications" unit:"count"`
	NumAccessCounterThresholdHits uint64 `metric:"num_access_counter_threshold_hits" unit:"count"`

	// §27 Eviction.
	NumEvictions      uint64 `metric:"num_evictions" unit:"count"`
	BytesEvicted      uint64 `metric:"bytes_evicted" unit:"bytes"`
	NumDirtyEvictions uint64 `metric:"num_dirty_evictions" unit:"count"`

	// §27 TBN summary (derived from the §11.12 detailed counters).
	NumTBNPrefetchEvents   uint64 `metric:"num_tbn_prefetch_events" unit:"count"`
	TBNPrefetchBytes       uint64 `metric:"tbn_prefetch_bytes" unit:"bytes"`
	TBNUsefulPrefetchBytes uint64 `metric:"tbn_useful_prefetch_bytes" unit:"bytes"`
	TBNUnusedPrefetchBytes uint64 `metric:"tbn_unused_prefetch_bytes" unit:"bytes"`

	// §27 TLB / mapping.
	NumRemotePTEInstalls        uint64 `metric:"num_remote_pte_installs" unit:"count"`
	NumLocalPTEInstalls         uint64 `metric:"num_local_pte_installs" unit:"count"`
	NumUVMTLBRangeInvalidations uint64 `metric:"num_uvm_tlb_range_invalidations" unit:"count"`

	// §11.12 detailed TBN.
	NumTBNFaultEvents                  uint64 `metric:"num_tbn_fault_events" unit:"count"`
	NumTBN64KBSelections               uint64 `metric:"num_tbn_64kb_selections" unit:"count"`
	NumTBN128KBExpansions              uint64 `metric:"num_tbn_128kb_expansions" unit:"count"`
	NumTBN256KBExpansions              uint64 `metric:"num_tbn_256kb_expansions" unit:"count"`
	NumTBN512KBExpansions              uint64 `metric:"num_tbn_512kb_expansions" unit:"count"`
	NumTBN1MBExpansions                uint64 `metric:"num_tbn_1mb_expansions" unit:"count"`
	NumTBN2MBExpansions                uint64 `metric:"num_tbn_2mb_expansions" unit:"count"`
	TBNSelectedBytes                   uint64 `metric:"tbn_selected_bytes" unit:"bytes"`
	TBNDemandBytes                     uint64 `metric:"tbn_demand_bytes" unit:"bytes"`
	TBNPrefetchCandidateBytes          uint64 `metric:"tbn_prefetch_candidate_bytes" unit:"bytes"`
	TBNActualPrefetchDMABytes          uint64 `metric:"tbn_actual_prefetch_dma_bytes" unit:"bytes"`
	TBNPrefetchSuppressedResidentBytes uint64 `metric:"tbn_prefetch_suppressed_resident_bytes" unit:"bytes"`
	TBNPrefetchSuppressedInflightBytes uint64 `metric:"tbn_prefetch_suppressed_inflight_bytes" unit:"bytes"`
	TBNUsefulPrefetchedPages           uint64 `metric:"tbn_useful_prefetched_4kb_pages" unit:"count"`
	TBNUnusedPrefetchedPages           uint64 `metric:"tbn_unused_prefetched_4kb_pages" unit:"count"`

	// §17.1 pre-eviction.
	NumPreEvictions                  uint64 `metric:"num_pre_evictions" unit:"count"`
	BytesPreEvicted                  uint64 `metric:"bytes_pre_evicted" unit:"bytes"`
	NumConcurrentPreEvictions        uint64 `metric:"num_concurrent_pre_evictions" unit:"count"`
	MaxConcurrentPreEvictions        uint64 `metric:"max_concurrent_pre_evictions" unit:"count"`
	NumPreEvictionsOverlappedWithH2D uint64 `metric:"num_pre_evictions_overlapped_with_h2d" unit:"count"`
	MigrationWaitCyclesForCapacity   uint64 `metric:"migration_wait_cycles_for_capacity" unit:"count"`
	OptionalHeadroomShortfallCount   uint64 `metric:"optional_headroom_shortfall_count" unit:"count"`
	OptionalHeadroomShortfallBytes   uint64 `metric:"optional_headroom_shortfall_bytes" unit:"bytes"`

	// §28 oversubscription diagnostics.
	PeakResidentBytes uint64 `metric:"peak_resident_bytes" unit:"bytes"`
	CapacityBytes     uint64 `metric:"uvm_capacity_bytes" unit:"bytes"`
}

// snapshot builds the immutable snapshot from the live counters. The caller
// must hold the manager lock. Averages are computed safely: a zero
// observation count yields a zero average (no division by zero). sbin_codex
func (s *uvmStats) snapshot(ideal bool) UVMStatsSnapshot {
	snap := UVMStatsSnapshot{
		IdealUVM: ideal,

		NumGPUPageFaultRequests: s.numGPUPageFaultRequests,
		NumUniqueFaultServices:  s.numUniqueFaultServices,
		NumCoalescedFaults:      s.numCoalescedFaults,
		FaultServiceLatencyTotal: s.faultServiceLatencyTotal,

		NumCPUToGPUMigrations:       s.numCPUToGPUMigrations,
		BytesCPUToGPU:               s.bytesCPUToGPU,
		NumGPUToCPUMigrations:       s.numGPUToCPUMigrations,
		BytesGPUToCPU:               s.bytesGPUToCPU,
		NumPrefetchMigrations:       s.numPrefetchMigrations,
		BytesPrefetched:             s.bytesPrefetched,
		NumDemandMigrations:         s.numDemandMigrations,
		BytesDemandMigrated:         s.bytesDemandMigrated,
		NumAccessCounterMigrations:  s.numAccessCounterMigrations,
		BytesAccessCounterMigrated:  s.bytesAccessCounterMigrated,
		NumWriteTriggeredMigrations: s.numWriteTriggeredMigrations,

		NumRemoteReads:             s.numRemoteReads,
		NumRemoteWritesDetected:    s.numRemoteWritesDetected,
		BytesRemoteRead:            s.bytesRemoteRead,
		PCIeRemoteReadTransactions: s.pcieRemoteReadTransactions,

		NumAccessCounterIncrements:    s.numAccessCounterIncrements,
		NumAccessCounterNotifications: s.numAccessCounterNotifications,
		NumAccessCounterThresholdHits: s.numAccessCounterThresholdHits,

		NumEvictions:      s.numEvictions,
		BytesEvicted:      s.bytesEvicted,
		NumDirtyEvictions: s.numDirtyEvictions,

		NumRemotePTEInstalls:        s.numRemotePTEInstalls,
		NumLocalPTEInstalls:         s.numLocalPTEInstalls,
		NumUVMTLBRangeInvalidations: s.numUVMTLBRangeInvalidations,

		NumTBNFaultEvents:                  s.tbn.FaultEvents,
		NumTBN64KBSelections:               s.tbn.Selections64KB,
		NumTBN128KBExpansions:              s.tbn.Expansions128KB,
		NumTBN256KBExpansions:              s.tbn.Expansions256KB,
		NumTBN512KBExpansions:              s.tbn.Expansions512KB,
		NumTBN1MBExpansions:                s.tbn.Expansions1MB,
		NumTBN2MBExpansions:                s.tbn.Expansions2MB,
		TBNSelectedBytes:                   s.tbn.SelectedBytes,
		TBNDemandBytes:                     s.tbn.DemandBytes,
		TBNPrefetchCandidateBytes:          s.tbn.PrefetchCandidateBytes,
		TBNActualPrefetchDMABytes:          s.tbn.ActualPrefetchDMABytes,
		TBNPrefetchSuppressedResidentBytes: s.tbn.SuppressedResidentBytes,
		TBNPrefetchSuppressedInflightBytes: s.tbn.SuppressedInflightBytes,
		TBNUsefulPrefetchedPages:           s.tbn.UsefulPrefetchedPages,
		TBNUnusedPrefetchedPages:           s.tbn.UnusedPrefetchedPages,

		NumPreEvictions:                  s.preEviction.numPreEvictions,
		BytesPreEvicted:                  s.preEviction.bytesPreEvicted,
		NumConcurrentPreEvictions:        s.preEviction.numConcurrentPreEvictions,
		MaxConcurrentPreEvictions:        s.preEviction.maxConcurrentPreEvictions,
		NumPreEvictionsOverlappedWithH2D: s.preEviction.numPreEvictionsOverlappedWithH2D,
		MigrationWaitCyclesForCapacity:   s.preEviction.migrationWaitCyclesForCapacity,
		OptionalHeadroomShortfallCount:   s.preEviction.optionalHeadroomShortfallCount,
		OptionalHeadroomShortfallBytes:   s.preEviction.optionalHeadroomShortfallBytes,

		PeakResidentBytes: s.peakResidentBytes,
		CapacityBytes:     s.capacityBytes,
	}

	// §27 TBN summary, derived from the §11.12 detailed counters.
	snap.NumTBNPrefetchEvents = s.tbn.PrefetchEvents
	snap.TBNPrefetchBytes = s.tbn.ActualPrefetchDMABytes
	snap.TBNUsefulPrefetchBytes = s.tbn.UsefulPrefetchedPages * basePageSize
	snap.TBNUnusedPrefetchBytes = s.tbn.UnusedPrefetchedPages * basePageSize

	// Averages computed safely: no division by zero.
	if s.faultServiceLatencyCount > 0 {
		snap.FaultServiceLatencyAvg = s.faultServiceLatencyTotal /
			sim.VTimeInSec(s.faultServiceLatencyCount)
	}
	return snap
}

// Snapshot returns the immutable point-in-time snapshot of every UVM
// statistic under the manager lock. sbin_codex
func (m *UVMManager) Snapshot() UVMStatsSnapshot {
	m.Lock()
	defer m.Unlock()

	return m.stats.snapshot(m.config.Ideal)
}

// UVMStats returns the immutable snapshot of every UVM statistic (an
// all-zero snapshot when UVM is disabled). sbin_codex
func (d *Driver) UVMStats() UVMStatsSnapshot {
	if d.uvm == nil {
		return UVMStatsSnapshot{}
	}
	return d.uvm.Snapshot()
}

// The following manager-level record methods are the locking update points
// used by the driver middlewares (which do not hold the manager lock). Each
// delegates to the single uvmStats update method of its metric. sbin_codex

// recordFaultServiceLatency is the locking update point of
// fault_service_latency_total (faultServiceMiddleware.chargeLatency).
func (m *UVMManager) recordFaultServiceLatency(latency sim.VTimeInSec) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordFaultServiceLatency(latency)
}

// recordCPUToGPUMigration is the locking update point of
// num_cpu_to_gpu_migrations / bytes_cpu_to_gpu (the H2D completion sites).
func (m *UVMManager) recordCPUToGPUMigration(bytes uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordCPUToGPUMigration(bytes)
}

// recordDemandMigration is the locking update point of
// num_demand_migrations / bytes_demand_migrated (the fault-service H2D
// completion).
func (m *UVMManager) recordDemandMigration(bytes uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordDemandMigration(bytes)
}

// recordPrefetchMigration is the locking update point of
// num_prefetch_migrations / bytes_prefetched (the fault-service H2D
// completion).
func (m *UVMManager) recordPrefetchMigration(bytes uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordPrefetchMigration(bytes)
}

// recordAccessCounterMigratedBytes is the locking update point of
// bytes_access_counter_migrated (the AC migration H2D completion).
func (m *UVMManager) recordAccessCounterMigratedBytes(bytes uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordAccessCounterMigratedBytes(bytes)
}

// recordGPUToCPUMigration is the locking update point of
// num_gpu_to_cpu_migrations / bytes_gpu_to_cpu (the eviction D2H
// completion).
func (m *UVMManager) recordGPUToCPUMigration(bytes uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordGPUToCPUMigration(bytes)
}

// recordRemoteReads is the locking update point of num_remote_reads /
// bytes_remote_read / pcie_remote_read_transactions (the access-counter
// notification seam).
func (m *UVMManager) recordRemoteReads(count uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordRemoteReads(count)
}

// recordRemoteWriteDetected is the locking update point of
// num_remote_writes_detected (the remote-write trigger seam).
func (m *UVMManager) recordRemoteWriteDetected() {
	m.Lock()
	defer m.Unlock()

	m.stats.recordRemoteWriteDetected()
}

// recordAccessCounterIncrements is the locking update point of
// num_access_counter_increments (the access-counter notification seam).
func (m *UVMManager) recordAccessCounterIncrements(count uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordAccessCounterIncrements(count)
}

// recordAccessCounterNotification is the locking update point of
// num_access_counter_notifications / num_access_counter_threshold_hits.
func (m *UVMManager) recordAccessCounterNotification() {
	m.Lock()
	defer m.Unlock()

	m.stats.recordAccessCounterNotification()
}

// recordRemotePTEInstalls is the locking update point of
// num_remote_pte_installs (the eviction final-PTE publication).
func (m *UVMManager) recordRemotePTEInstalls(count uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordRemotePTEInstalls(count)
}

// recordLocalPTEInstalls is the locking update point of
// num_local_pte_installs (the fault/migration GPU_LOCAL PTE publications).
func (m *UVMManager) recordLocalPTEInstalls(count uint64) {
	m.Lock()
	defer m.Unlock()

	m.stats.recordLocalPTEInstalls(count)
}

// recordUVMTLBRangeInvalidation is the locking update point of
// num_uvm_tlb_range_invalidations (the three startTLBI sites).
func (m *UVMManager) recordUVMTLBRangeInvalidation() {
	m.Lock()
	defer m.Unlock()

	m.stats.recordUVMTLBRangeInvalidation()
}