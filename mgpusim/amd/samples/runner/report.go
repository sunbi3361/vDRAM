package runner

import (
	"fmt"
	"sort"
	"strings"
	// "sync" // sbin_codex: removed - instructionCountTracer no longer exists.

	"github.com/sarchlab/akita/v4/datarecording"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/driver"               // sbin_codex: integrated from extendedreport.go.
	"github.com/sarchlab/mgpusim/v4/amd/timing/accesscounter" // sbin_codex
	"github.com/sarchlab/mgpusim/v4/amd/timing/cu"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rdma"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/asu" // sbin_claude_avatar
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/rsw" // sbin_claude_utopia
)

const (
	tableName = "mgpusim_metrics"
)

type metric struct {
	Location string
	What     string
	Value    float64
	Unit     string
}

type kernelTimeTracer struct {
	tracer *tracing.BusyTimeTracer
	comp   tracing.NamedHookable
}

type instCountTracer struct {
	tracer *instTracer
	cu     tracing.NamedHookable
}

type cacheLatencyTracer struct {
	tracer *tracing.AverageTimeTracer
	cache  tracing.NamedHookable
}

type cacheHitRateTracer struct {
	tracer *tracing.StepCountTracer
	cache  tracing.NamedHookable
}

type tlbHitRateTracer struct {
	tracer *tracing.StepCountTracer
	tlb    tracing.NamedHookable
}

type dramTransactionCountTracer struct {
	tracer *dramTracer
	dram   tracing.NamedHookable
}

type rdmaTransactionCountTracer struct {
	outgoingTracer *tracing.AverageTimeTracer
	incomingTracer *tracing.AverageTimeTracer
	rdmaEngine     *rdma.Comp
}

type simdBusyTimeTracer struct {
	tracer *tracing.BusyTimeTracer
	simd   tracing.NamedHookable
}

type cuCPIStackTracer struct {
	cu     tracing.NamedHookable
	tracer *cu.CPIStackTracer
}

type reporter struct {
	dataRecorder datarecording.DataRecorder
	extended     *extendedReporter // sbin_codex: GMMU/memory/working-set reports.

	kernelTimeTracer        *kernelTimeTracer
	perGPUKernelTimeTracers []*kernelTimeTracer
	instCountTracers        []*instCountTracer
	cacheLatencyTracers     []*cacheLatencyTracer
	cacheHitRateTracers     []*cacheHitRateTracer
	tlbHitRateTracers       []*tlbHitRateTracer
	dramTracers             []*dramTransactionCountTracer
	rdmaTransactionCounters []*rdmaTransactionCountTracer
	simdBusyTimeTracers     []*simdBusyTimeTracer
	cuCPITraces             []*cuCPIStackTracer

	ReportInstCount            bool
	ReportCacheLatency         bool
	ReportCacheHitRate         bool
	ReportTLBHitRate           bool
	ReportRDMATransactionCount bool
	ReportDRAMTransactionCount bool
	ReportSIMDBusyTime         bool
	ReportCPIStack             bool

	driver *driver.Driver // sbin_codex: UVM statistics source.
	// accessCounters are the GPU-side UVM remote-access counters. They own the
	// remote-access and write-stall statistics. // sbin_codex
	accessCounters []*accesscounter.Comp

	// utopiaUnits are the per-GPU Utopia RestSeg walkers (UTU). They own the
	// RSW hit/miss and TAR/SF cache statistics. // sbin_claude_utopia
	utopiaUnits []*rsw.Comp

	// avatarUnits are the per-GPU Avatar Speculation Units (ASU). They own
	// the speculation/CAVA/EAF statistics. // sbin_claude_avatar
	avatarUnits []*asu.Comp
}

func newReporter(s *simulation.Simulation) *reporter {
	r := &reporter{
		dataRecorder: s.GetDataRecorder(),
		extended:     newExtendedReporter(s), // sbin_codex: install extended reporters.
	}

	r.injectTracers(s)

	r.dataRecorder.CreateTable(tableName, metric{})

	if c := s.GetComponentByName("Driver"); c != nil { // sbin_codex: UVM stats source.
		r.driver = c.(*driver.Driver)
	}

	r.collectAccessCounters(s) // sbin_codex
	r.collectUtopiaUnits(s)    // sbin_claude_utopia
	r.collectAvatarUnits(s)    // sbin_claude_avatar

	return r
}

// collectAvatarUnits finds every GPU-side Avatar Speculation Unit.
// sbin_claude_avatar
func (r *reporter) collectAvatarUnits(s *simulation.Simulation) {
	for i := 1; ; i++ {
		name := fmt.Sprintf("GPU[%d].ASU", i)

		c := s.GetComponentByName(name)
		if c == nil {
			return
		}

		unit, ok := c.(*asu.Comp)
		if !ok {
			return
		}

		r.avatarUnits = append(r.avatarUnits, unit)
	}
}

// collectUtopiaUnits finds every GPU-side Utopia RestSeg walker.
// sbin_claude_utopia
func (r *reporter) collectUtopiaUnits(s *simulation.Simulation) {
	for i := 1; ; i++ {
		name := fmt.Sprintf("GPU[%d].UTU", i)

		c := s.GetComponentByName(name)
		if c == nil {
			return
		}

		unit, ok := c.(*rsw.Comp)
		if !ok {
			return
		}

		r.utopiaUnits = append(r.utopiaUnits, unit)
	}
}

// collectAccessCounters finds every GPU-side UVM access counter. // sbin_codex
func (r *reporter) collectAccessCounters(s *simulation.Simulation) {
	for i := 1; ; i++ {
		name := fmt.Sprintf("GPU[%d].UVMAccessCounter", i)

		c := s.GetComponentByName(name)
		if c == nil {
			return
		}

		counter, ok := c.(*accesscounter.Comp)
		if !ok {
			return
		}

		r.accessCounters = append(r.accessCounters, counter)
	}
}

// accessCounterStats aggregates the GPU-side counters of the platform.
// sbin_codex
func (r *reporter) accessCounterStats() accesscounter.Stats {
	var total accesscounter.Stats

	for _, counter := range r.accessCounters {
		snapshot := counter.Snapshot()
		total.RemoteAccesses += snapshot.RemoteAccesses
		total.Notifications += snapshot.Notifications
		total.StalledWrites += snapshot.StalledWrites
		total.ReleasedWrites += snapshot.ReleasedWrites
		// Pre-edit code (commented per AGENTS.md convention): RefusedWrites was
		// left out of the sum, so uvm_num_remote_writes_performed reported zero
		// however many writes the driver had actually refused to migrate.
		// sbin_codex
		total.RefusedWrites += snapshot.RefusedWrites
	}

	return total
}

func (r *reporter) injectTracers(s *simulation.Simulation) {
	r.injectKernelTimeTracer(s)
	r.injectInstCountTracer(s)
	r.injectCUCPIHook(s)
	r.injectCacheLatencyTracer(s)
	r.injectCacheHitRateTracer(s)
	r.injectTLBHitRateTracer(s)
	r.injectRDMAEngineTracer(s)
	r.injectDRAMTracer(s)
	r.injectSIMDBusyTimeTracer(s)
	r.extended.injectTracers(s) // sbin_codex: install extended tracers.
	r.injectInstStopper(s)      // sbin_codex: -max-inst stops after N retired instructions.
}

// injectInstStopper attaches a -max-inst limiter to every CU so the
// simulation terminates after the given number of retired instructions.
// sbin_codex: newInstStopper was dead code; this call wires the -max-inst
// flag (runner.flag.go) into the tracer that actually enforces the limit.
func (r *reporter) injectInstStopper(s *simulation.Simulation) {
	// sbin_codex: the reporting tracers already carry maxCount when -max-inst
	// is set; this fallback only covers -max-inst without any reporting flag,
	// where instCountTracers is empty.
	if *maxInstCount == 0 || len(r.instCountTracers) > 0 {
		return
	}
	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "CU") {
			tracing.CollectTrace(comp.(tracing.NamedHookable),
				newInstStopper(*maxInstCount))
		}
	}
}

func (r *reporter) injectKernelTimeTracer(s *simulation.Simulation) {
	if *unifiedGPUFlag != "" {
		tracer := tracing.NewBusyTimeTracer(
			s.GetEngine(),
			func(task tracing.Task) bool {
				return task.What == "*driver.LaunchUnifiedMultiGPUKernelCommand"
			})
		tracing.CollectTrace(
			s.GetComponentByName("Driver").(tracing.NamedHookable),
			tracer)
		r.kernelTimeTracer = &kernelTimeTracer{
			tracer: tracer,
			comp:   s.GetComponentByName("Driver").(tracing.NamedHookable),
		}
	} else {
		tracer := tracing.NewBusyTimeTracer(
			s.GetEngine(),
			func(task tracing.Task) bool {
				return task.What == "*driver.LaunchKernelCommand"
			})
		tracing.CollectTrace(
			s.GetComponentByName("Driver").(tracing.NamedHookable),
			tracer)
		r.kernelTimeTracer = &kernelTimeTracer{
			tracer: tracer,
			comp:   s.GetComponentByName("Driver").(tracing.NamedHookable),
		}
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "CommandProcessor") {
			tracer := tracing.NewBusyTimeTracer(
				s.GetEngine(),
				func(task tracing.Task) bool {
					return task.What == "*protocol.LaunchKernelReq"
				})
			tracing.CollectTrace(
				comp.(tracing.NamedHookable),
				tracer)
			r.perGPUKernelTimeTracers = append(
				r.perGPUKernelTimeTracers,
				&kernelTimeTracer{
					tracer: tracer,
					comp:   comp.(tracing.NamedHookable),
				})
		}
	}
}

func (r *reporter) injectInstCountTracer(s *simulation.Simulation) {
	// sbin_codex: l2TLBMPKIReportFlag also needs per-CU instruction counts.
	if !*reportAll && !*instCountReportFlag && !*l2TLBMPKIReportFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "CU") {
			tracer := newInstTracer()
			if *maxInstCount > 0 {
				// sbin_codex: count and stop in one tracer per CU so the
				// global retired count is not double-counted by a second
				// attached tracer.
				tracer = newInstStopper(*maxInstCount)
			}
			r.instCountTracers = append(r.instCountTracers,
				&instCountTracer{
					tracer: tracer,
					cu:     comp.(tracing.NamedHookable),
				})
			tracing.CollectTrace(comp.(tracing.NamedHookable), tracer)
		}
	}
}

func (r *reporter) injectCUCPIHook(s *simulation.Simulation) {
	if !*reportAll && !*reportCPIStackFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "CU") {
			tracer := cu.NewCPIStackInstHook(
				comp.(*cu.ComputeUnit), s.GetEngine())
			tracing.CollectTrace(comp.(tracing.NamedHookable), tracer)

			r.cuCPITraces = append(r.cuCPITraces,
				&cuCPIStackTracer{
					tracer: tracer,
					cu:     comp.(tracing.NamedHookable),
				})
		}
	}
}

func (r *reporter) injectCacheLatencyTracer(s *simulation.Simulation) {
	if !*reportAll && !*cacheLatencyReportFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "Cache") {
			tracer := tracing.NewAverageTimeTracer(
				s.GetEngine(),
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.cacheLatencyTracers = append(r.cacheLatencyTracers,
				&cacheLatencyTracer{
					tracer: tracer,
					cache:  comp.(tracing.NamedHookable),
				})
			tracing.CollectTrace(comp.(tracing.NamedHookable), tracer)
		}
	}
}

func (r *reporter) injectCacheHitRateTracer(s *simulation.Simulation) {
	if !*reportAll && !*cacheLatencyReportFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "Cache") {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { return true })
			r.cacheHitRateTracers = append(r.cacheHitRateTracers,
				&cacheHitRateTracer{
					tracer: tracer,
					cache:  comp.(tracing.NamedHookable),
				})
			tracing.CollectTrace(comp.(tracing.NamedHookable), tracer)
		}
	}
}

func (r *reporter) injectTLBHitRateTracer(s *simulation.Simulation) {
	// sbin_codex: l2TLBMPKIReportFlag also needs L2TLB miss counts.
	if !*reportAll && !*tlbHitRateReportFlag && !*l2TLBMPKIReportFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "TLB") {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { return true })
			r.tlbHitRateTracers = append(r.tlbHitRateTracers,
				&tlbHitRateTracer{
					tracer: tracer,
					tlb:    comp.(tracing.NamedHookable),
				})
			tracing.CollectTrace(comp.(tracing.NamedHookable), tracer)
		}
	}
}

func (r *reporter) injectRDMAEngineTracer(s *simulation.Simulation) {
	if !*reportAll && !*rdmaTransactionCountReportFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "RDMA") {
			t := &rdmaTransactionCountTracer{}
			t.rdmaEngine = comp.(*rdma.Comp)
			t.incomingTracer = tracing.NewAverageTimeTracer(
				s.GetEngine(),
				func(task tracing.Task) bool {
					if task.Kind != "req_in" {
						return false
					}

					isFromOutside := strings.Contains(
						string(task.Detail.(sim.Msg).Meta().Src), "RDMA")
					if !isFromOutside {
						return false
					}

					return true
				})
			t.outgoingTracer = tracing.NewAverageTimeTracer(
				s.GetEngine(),
				func(task tracing.Task) bool {
					if task.Kind != "req_in" {
						return false
					}

					isFromOutside := strings.Contains(
						string(task.Detail.(sim.Msg).Meta().Src), "RDMA")
					if isFromOutside {
						return false
					}

					return true
				})

			tracing.CollectTrace(t.rdmaEngine, t.incomingTracer)
			tracing.CollectTrace(t.rdmaEngine, t.outgoingTracer)

			r.rdmaTransactionCounters = append(r.rdmaTransactionCounters, t)
		}
	}
}

func (r *reporter) injectDRAMTracer(s *simulation.Simulation) {
	if !*reportAll && !*dramTransactionCountReportFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "DRAM") {
			t := &dramTransactionCountTracer{}
			t.dram = comp.(tracing.NamedHookable)
			t.tracer = newDramTracer(s.GetEngine())

			tracing.CollectTrace(t.dram, t.tracer)

			r.dramTracers = append(r.dramTracers, t)
		}
	}
}

func (r *reporter) injectSIMDBusyTimeTracer(s *simulation.Simulation) {
	if !*reportAll && !*simdBusyTimeTracerFlag {
		return
	}

	for _, comp := range s.Components() {
		if strings.Contains(comp.Name(), "SIMD") {
			perSIMDBusyTimeTracer := tracing.NewBusyTimeTracer(
				s.GetEngine(),
				func(task tracing.Task) bool {
					return task.Kind == "pipeline"
				})
			r.simdBusyTimeTracers = append(r.simdBusyTimeTracers,
				&simdBusyTimeTracer{
					tracer: perSIMDBusyTimeTracer,
					simd:   comp.(tracing.NamedHookable),
				})
			tracing.CollectTrace(comp.(tracing.NamedHookable), perSIMDBusyTimeTracer)
		}
	}
}

func (r *reporter) report() {
	r.reportKernelTime()
	r.reportInstCount()
	r.reportCPIStack()
	r.reportSIMDBusyTime()
	r.reportCacheLatency()
	r.reportCacheHitRate()
	r.reportTLBHitRate()
	r.reportRDMATransactionCount()
	r.reportDRAMTransactionCount()
	r.extended.report(r) // sbin_codex: emit summary metrics without page-fault detail rows.
	r.reportL2TLBMPKI()  // sbin_codex: L2 TLB MPKI from the shared tracers.
	r.reportUVM()        // sbin_codex: UVM demand-paging statistics.
	r.reportUtopia()     // sbin_claude_utopia: RestSeg walk statistics.
	r.reportAvatar()     // sbin_claude_avatar: speculation statistics.
}

// reportAvatar emits the speculation, CAVA-validation, and EAF counters of
// every ASU, plus the registry-side metadata/region counters
// (refs/avatar.md 5.13 statistics row). // sbin_claude_avatar
func (r *reporter) reportAvatar() {
	for i, unit := range r.avatarUnits {
		stats := unit.Stats()
		location := fmt.Sprintf("GPU[%d].ASU", i+1)

		rows := []struct {
			what string
			val  float64
		}{
			{"avatar_l1_miss_forwarded_count", float64(stats.Forwarded)},
			{"avatar_speculation_count", float64(stats.Speculations)},
			{"avatar_cava_pass_count", float64(stats.CAVAPass)},
			{"avatar_cava_mismatch_count", float64(stats.CAVAMismatch)},
			{"avatar_cava_incompressible_count",
				float64(stats.CAVAIncompressible)},
			{"avatar_cava_no_metadata_count", float64(stats.CAVANoMetadata)},
			{"avatar_early_completion_count",
				float64(stats.EarlyCompletions)},
			{"avatar_real_response_first_count",
				float64(stats.RealResponseFirst)},
			{"avatar_swallowed_rsp_count", float64(stats.SwallowedRsps)},
			{"avatar_page_table_veto_count", float64(stats.PageTableVetoes)},
		}

		if r.driver != nil && r.driver.AvatarRegistry() != nil {
			regStats := r.driver.AvatarRegistry().Stats()
			bound, free := r.driver.AvatarRegistry().Occupancy(i + 1)
			rows = append(rows, []struct {
				what string
				val  float64
			}{
				{"avatar_frame_install_count",
					float64(regStats.FrameInstalls)},
				{"avatar_frame_invalidate_count",
					float64(regStats.FrameInvalidates)},
				{"avatar_region_bound_count", float64(bound)},
				{"avatar_region_free_count", float64(free)},
			}...)
		}

		for _, row := range rows {
			r.dataRecorder.InsertData(tableName, metric{
				Location: location,
				What:     row.what,
				Value:    row.val,
				Unit:     "",
			})
		}
	}
}

// reportUtopia emits the RestSeg walk and TAR/SF cache counters of every UTU,
// plus the RestSeg occupancy from the driver-owned registry (utopia.md 4.14).
// sbin_claude_utopia
func (r *reporter) reportUtopia() {
	for i, unit := range r.utopiaUnits {
		stats := unit.Stats()
		location := fmt.Sprintf("GPU[%d].UTU", i+1)

		rows := []struct {
			what string
			val  float64
		}{
			{"utopia_rsw_hit_count", float64(stats.RSWHits)},
			{"utopia_rsw_miss_count", float64(stats.RSWMisses)},
			{"utopia_sf_filtered_count", float64(stats.SFFiltered)},
			{"utopia_sf_cache_hit_count", float64(stats.SFCacheHits)},
			{"utopia_sf_cache_miss_count", float64(stats.SFCacheMisses)},
			{"utopia_tar_cache_hit_count", float64(stats.TARCacheHits)},
			{"utopia_tar_cache_miss_count", float64(stats.TARCacheMisses)},
			{"utopia_flexseg_walk_count", float64(stats.FlexSegWalks)},
			{"utopia_passthrough_count", float64(stats.Passthrough)},
		}

		if r.driver != nil && r.driver.UtopiaRegistry() != nil {
			rows = append(rows, struct {
				what string
				val  float64
			}{
				"utopia_restseg_occupied_frames",
				float64(r.driver.UtopiaRegistry().OccupiedFrames(i + 1)),
			})
		}

		for _, row := range rows {
			r.dataRecorder.InsertData(tableName, metric{
				Location: location,
				What:     row.what,
				Value:    row.val,
				Unit:     "",
			})
		}
	}
}

// reportUVM emits the UVM counter set required by spec 27. Every counter has
// the same definition in normal and in ideal mode. // sbin_codex
//
//nolint:funlen // one flat table of required counters reads better than splits.
func (r *reporter) reportUVM() {
	if r.driver == nil || !r.driver.UVMEnabled() {
		return
	}

	stats := r.driver.UVMStats()
	counters := r.accessCounterStats() // sbin_codex

	rows := []struct {
		what string
		val  float64
		unit string
	}{
		{"uvm_enabled", boolToFloat(stats.Enabled), ""},
		{"uvm_ideal", boolToFloat(stats.Ideal), ""},

		{"uvm_raw_page_fault_count", float64(stats.RawPageFaults), ""},
		{"uvm_unique_fault_service_count", float64(stats.UniqueFaultServices), ""},
		{"uvm_coalesced_page_fault_count", float64(stats.CoalescedFaults), ""},

		{"uvm_num_tbn_fault_events", float64(stats.TBNFaultEvents), ""},
		{"uvm_num_tbn_64kb_selections", float64(stats.TBNSelections[0]), ""},
		{"uvm_num_tbn_128kb_expansions", float64(stats.TBNSelections[1]), ""},
		{"uvm_num_tbn_256kb_expansions", float64(stats.TBNSelections[2]), ""},
		{"uvm_num_tbn_512kb_expansions", float64(stats.TBNSelections[3]), ""},
		{"uvm_num_tbn_1mb_expansions", float64(stats.TBNSelections[4]), ""},
		{"uvm_num_tbn_2mb_expansions", float64(stats.TBNSelections[5]), ""},
		{"uvm_tbn_selected_bytes", float64(stats.TBNSelectedBytes), ""},
		{"uvm_tbn_demand_bytes", float64(stats.TBNDemandBytes), ""},
		{"uvm_tbn_prefetch_candidate_bytes",
			float64(stats.TBNPrefetchCandidateByte), ""},
		{"uvm_tbn_actual_prefetch_dma_bytes",
			float64(stats.TBNActualPrefetchBytes), ""},
		{"uvm_tbn_prefetch_suppressed_resident_bytes",
			float64(stats.TBNSuppressedResident), ""},
		{"uvm_tbn_prefetch_suppressed_inflight_bytes",
			float64(stats.TBNSuppressedInflight), ""},

		{"uvm_num_cpu_to_gpu_migrations", float64(stats.CPUToGPUMigrations), ""},
		{"uvm_bytes_cpu_to_gpu", float64(stats.BytesCPUToGPU), ""},
		{"uvm_num_gpu_to_cpu_migrations", float64(stats.GPUToCPUMigrations), ""},
		{"uvm_bytes_gpu_to_cpu", float64(stats.BytesGPUToCPU), ""},
		{"uvm_num_demand_migrations", float64(stats.DemandMigrations), ""},
		{"uvm_num_access_counter_migrations",
			float64(stats.AccessCounterMigrations), ""},
		{"uvm_bytes_access_counter_migrated",
			float64(stats.BytesAccessCounterMigrated), ""},
		{"uvm_num_write_triggered_migrations",
			float64(counters.StalledWrites), ""},
		{"uvm_migrated_pages", float64(stats.MigratedPages), ""},
		{"uvm_migrated_bytes", float64(stats.MigratedBytes), ""},
		{"uvm_repeated_migrations", float64(stats.RepeatedMigrations), ""},

		{"uvm_num_evictions", float64(stats.Evictions), ""},
		{"uvm_evicted_pages", float64(stats.EvictedPages), ""},
		{"uvm_bytes_evicted", float64(stats.EvictedBytes), ""},
		{"uvm_num_pre_evictions", float64(stats.PreEvictions), ""},
		{"uvm_bytes_pre_evicted", float64(stats.PreEvictedBytes), ""},
		{"uvm_max_concurrent_pre_evictions",
			float64(stats.MaxConcurrentPreEvictions), ""},
		{"uvm_num_pre_evictions_overlapped_with_h2d",
			float64(stats.PreEvictionsOverlappedH2D), ""},
		{"uvm_migration_wait_for_capacity",
			float64(stats.MigrationWaitForCapacity), "second"},

		{"uvm_num_remote_accesses", float64(counters.RemoteAccesses), ""},
		{"uvm_num_remote_writes_detected", float64(counters.StalledWrites), ""},
		{"uvm_num_write_replays", float64(counters.ReleasedWrites), ""},
		{"uvm_num_access_counter_services",
			float64(stats.AccessCounterServices), ""}, // sbin_codex
		{"uvm_num_access_counter_notifications",
			float64(stats.AccessCounterNotify), ""},
		{"uvm_num_access_counter_suppressed",
			float64(stats.AccessCounterSuppressed), ""},
		{"uvm_num_access_counter_resets", float64(stats.AccessCounterResets), ""},

		{"uvm_num_remote_pte_installs", float64(stats.RemotePTEInstalls), ""},
		{"uvm_num_local_pte_installs", float64(stats.LocalPTEInstalls), ""},
		{"uvm_num_uvm_tlb_range_invalidations",
			float64(stats.TLBRangeInvalidations), ""},
		{"uvm_num_uvm_cache_range_flushes",
			float64(stats.CacheRangeFlushes), ""},
		{"uvm_num_fault_replays", float64(stats.FaultReplays), ""},
		{"uvm_num_refused_migrations", float64(stats.RefusedMigrations), ""},
		{"uvm_num_remote_writes_performed", float64(counters.RefusedWrites), ""},

		{"uvm_gpu_resident_pages_peak", float64(stats.GPUResidentPagesPeak), ""},
		{"uvm_gpu_resident_bytes_peak", float64(stats.GPUResidentBytesPeak), ""},

		{"uvm_fault_service_latency_total",
			float64(stats.FaultHandlingTime), "second"},
		{"uvm_migration_time", float64(stats.MigrationTime), "second"},
		{"uvm_eviction_time", float64(stats.EvictionTime), "second"},
	}

	for _, row := range rows {
		r.dataRecorder.InsertData(tableName, metric{
			Location: "UVM",
			What:     row.what,
			Value:    row.val,
			Unit:     row.unit,
		})
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// terminateInflightKernels closes out kernel-launch tasks that are still
// running when the simulation is cut off (-max-inst). The busy-time tracer
// only accumulates completed tasks, so without this a truncated run reports
// kernel_time = 0. // sbin_codex
func (r *reporter) terminateInflightKernels(now sim.VTimeInSec) {
	if r.kernelTimeTracer != nil {
		r.kernelTimeTracer.tracer.TerminateAllTasks(now)
	}

	for _, t := range r.perGPUKernelTimeTracers {
		t.tracer.TerminateAllTasks(now)
	}
}

func (r *reporter) reportKernelTime() {
	kernelTime := float64(r.kernelTimeTracer.tracer.BusyTime())
	r.dataRecorder.InsertData(
		tableName,
		metric{
			Location: r.kernelTimeTracer.comp.Name(),
			What:     "kernel_time",
			Value:    kernelTime,
			Unit:     "second",
		},
	)

	for _, t := range r.perGPUKernelTimeTracers {
		kernelTime := float64(t.tracer.BusyTime())
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.comp.Name(),
				What:     "kernel_time",
				Value:    kernelTime,
				Unit:     "second",
			},
		)
	}
}

func (r *reporter) reportInstCount() {
	kernelTime := float64(r.kernelTimeTracer.tracer.BusyTime())
	for _, t := range r.instCountTracers {
		cuFreq := float64(t.cu.(*cu.ComputeUnit).Freq)
		numCycle := kernelTime * cuFreq

		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.cu.Name(),
				What:     "cu_inst_count",
				Value:    float64(t.tracer.count),
				Unit:     "count",
			},
		)

		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.cu.Name(),
				What:     "cu_CPI",
				Value:    numCycle / float64(t.tracer.count),
				Unit:     "cycles/inst",
			},
		)

		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.cu.Name(),
				What:     "simd_inst_count",
				Value:    float64(t.tracer.simdCount),
				Unit:     "count",
			},
		)

		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.cu.Name(),
				What:     "simd_CPI",
				Value:    numCycle / float64(t.tracer.simdCount),
				Unit:     "cycles/inst",
			},
		)
	}
}

func (r *reporter) reportCPIStack() {
	for _, t := range r.cuCPITraces {
		cu := t.cu
		hook := t.tracer

		r.reportCPIStackEntries(hook, cu, false)
		r.reportCPIStackEntries(hook, cu, true)
	}
}

func (r *reporter) reportCPIStackEntries(
	hook *cu.CPIStackTracer,
	cu tracing.NamedHookable,
	simdStack bool,
) {
	cpiStack := hook.GetCPIStack()
	if simdStack {
		cpiStack = hook.GetSIMDCPIStack()
	}

	keys := make([]string, 0, len(cpiStack))
	for k := range cpiStack {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	stackTypeName := "CPIStack"
	if simdStack {
		stackTypeName = "SIMDCPIStack"
	}

	for _, name := range keys {
		value := cpiStack[name]
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: cu.Name(),
				What:     stackTypeName + "." + name,
				Value:    value,
				Unit:     "cycles/inst",
			},
		)
	}
}

func (r *reporter) reportSIMDBusyTime() {
	for _, t := range r.simdBusyTimeTracers {
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.simd.Name(),
				What:     "busy_time",
				Value:    float64(t.tracer.BusyTime()),
				Unit:     "second",
			},
		)
	}
}

func (r *reporter) reportCacheLatency() {
	for _, tracer := range r.cacheLatencyTracers {
		if tracer.tracer.AverageTime() == 0 {
			continue
		}

		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: tracer.cache.Name(),
				What:     "req_average_latency",
				Value:    float64(tracer.tracer.AverageTime()),
				Unit:     "second",
			},
		)
	}
}

func (r *reporter) reportCacheHitRate() {
	for _, tracer := range r.cacheHitRateTracers {
		readHit := tracer.tracer.GetStepCount("read-hit")
		readMiss := tracer.tracer.GetStepCount("read-miss")
		readMSHRHit := tracer.tracer.GetStepCount("read-mshr-hit")
		writeHit := tracer.tracer.GetStepCount("write-hit")
		writeMiss := tracer.tracer.GetStepCount("write-miss")
		writeMSHRHit := tracer.tracer.GetStepCount("write-mshr-hit")

		totalTransaction := readHit + readMiss + readMSHRHit +
			writeHit + writeMiss + writeMSHRHit

		if totalTransaction == 0 {
			continue
		}

		r.dataRecorder.InsertData(tableName, metric{
			Location: tracer.cache.Name(),
			What:     "read-hit",
			Value:    float64(readHit),
			Unit:     "count",
		})
		r.dataRecorder.InsertData(tableName, metric{
			Location: tracer.cache.Name(),
			What:     "read-miss",
			Value:    float64(readMiss),
			Unit:     "count",
		})
		r.dataRecorder.InsertData(tableName, metric{
			Location: tracer.cache.Name(),
			What:     "read-mshr-hit",
			Value:    float64(readMSHRHit),
			Unit:     "count",
		})
		r.dataRecorder.InsertData(tableName, metric{
			Location: tracer.cache.Name(),
			What:     "write-hit",
			Value:    float64(writeHit),
			Unit:     "count",
		})
		r.dataRecorder.InsertData(tableName, metric{
			Location: tracer.cache.Name(),
			What:     "write-miss",
			Value:    float64(writeMiss),
			Unit:     "count",
		})
		r.dataRecorder.InsertData(tableName, metric{
			Location: tracer.cache.Name(),
			What:     "write-mshr-hit",
			Value:    float64(writeMSHRHit),
			Unit:     "count",
		})
	}
}

func (r *reporter) reportTLBHitRate() {
	for _, tracer := range r.tlbHitRateTracers {
		hit := tracer.tracer.GetStepCount("hit")
		miss := tracer.tracer.GetStepCount("miss")
		mshrHit := tracer.tracer.GetStepCount("mshr-hit")

		totalTransaction := hit + miss + mshrHit

		if totalTransaction == 0 {
			continue
		}

		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: tracer.tlb.Name(),
				What:     "hit",
				Value:    float64(hit),
				Unit:     "count",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: tracer.tlb.Name(),
				What:     "miss",
				Value:    float64(miss),
				Unit:     "count",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: tracer.tlb.Name(),
				What:     "mshr-hit",
				Value:    float64(mshrHit),
				Unit:     "count",
			},
		)
	}
}

func (r *reporter) reportRDMATransactionCount() {
	for _, t := range r.rdmaTransactionCounters {
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.rdmaEngine.Name(),
				What:     "outgoing_trans_count",
				Value:    float64(t.outgoingTracer.TotalCount()),
				Unit:     "count",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.rdmaEngine.Name(),
				What:     "incoming_trans_count",
				Value:    float64(t.incomingTracer.TotalCount()),
				Unit:     "count",
			},
		)
	}
}

func (r *reporter) reportDRAMTransactionCount() {
	for _, t := range r.dramTracers {
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.dram.Name(),
				What:     "read_trans_count",
				Value:    float64(t.tracer.readCount),
				Unit:     "count",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.dram.Name(),
				What:     "write_trans_count",
				Value:    float64(t.tracer.writeCount),
				Unit:     "count",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.dram.Name(),
				What:     "read_avg_latency",
				Value:    float64(t.tracer.readAvgLatency),
				Unit:     "second",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.dram.Name(),
				What:     "write_avg_latency",
				Value:    float64(t.tracer.writeAvgLatency),
				Unit:     "second",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.dram.Name(),
				What:     "read_size",
				Value:    float64(t.tracer.readSize),
				Unit:     "bytes",
			},
		)
		r.dataRecorder.InsertData(
			tableName,
			metric{
				Location: t.dram.Name(),
				What:     "write_size",
				Value:    float64(t.tracer.writeSize),
				Unit:     "bytes",
			},
		)
	}
}

// sbin_codex: the following types and methods were integrated from
// extendedreport.go (summary-only extended memory-system reporting for the
// GMMU/MMU, working set, migration, and memory footprint).

type componentGMMUTracer struct {
	component tracing.NamedHookable
	tracer    *gmmuTracer
}

type componentWorkingSetTracer struct {
	component tracing.NamedHookable
	tracer    *workingSetTracer
}

type extendedReporter struct {
	driver *driver.Driver

	gmmuTracers []*componentGMMUTracer
	mmuTracers  []*componentGMMUTracer
	workingSet  *componentWorkingSetTracer
	migration   *migrationTracer
	// l2TLBMPKI *l2TLBMPKIReporter // sbin_codex: removed - MPKI reuses the main reporter's tracers.
}

func newExtendedReporter(s *simulation.Simulation) *extendedReporter {
	d, _ := s.GetComponentByName("Driver").(*driver.Driver)
	return &extendedReporter{driver: d}
}

func (r *extendedReporter) injectTracers(s *simulation.Simulation) {
	if *reportAll || *gmmuReportFlag {
		r.injectTranslationTracers(s)
	}
	if *reportAll || *workingSetReportFlag {
		r.injectWorkingSetTracer(s)
	}
	if *reportAll || *pageMigrationReportFlag {
		r.injectMigrationTracer(s)
	}
	// r.injectL2TLBMPKITracer(s) // sbin_codex: removed - MPKI reuses the main reporter's tracers.
}

func (r *extendedReporter) injectTranslationTracers(s *simulation.Simulation) {
	for _, comp := range s.Components() {
		hookable, ok := comp.(tracing.NamedHookable)
		if !ok {
			continue
		}

		switch {
		case strings.HasSuffix(comp.Name(), ".GMMU"):
			tracer := newGMMUTracer(s.GetEngine())
			tracing.CollectTrace(hookable, tracer)
			r.gmmuTracers = append(r.gmmuTracers, &componentGMMUTracer{
				component: hookable,
				tracer:    tracer,
			})
		case comp.Name() == "MMU":
			tracer := newGMMUTracer(s.GetEngine())
			tracing.CollectTrace(hookable, tracer)
			r.mmuTracers = append(r.mmuTracers, &componentGMMUTracer{
				component: hookable,
				tracer:    tracer,
			})
		}
	}
}

func (r *extendedReporter) injectWorkingSetTracer(s *simulation.Simulation) {
	pageSize := uint64(1 << 12)
	if r.driver != nil && r.driver.Log2PageSize < 64 {
		pageSize = uint64(1 << r.driver.Log2PageSize)
	}

	tracer := newWorkingSetTracer(pageSize)
	for _, comp := range s.Components() {
		if !isL1TLB(comp.Name()) {
			continue
		}
		hookable, ok := comp.(tracing.NamedHookable)
		if !ok {
			continue
		}
		tracing.CollectTrace(hookable, tracer)
	}
	r.workingSet = &componentWorkingSetTracer{tracer: tracer}
}

func (r *extendedReporter) injectMigrationTracer(s *simulation.Simulation) {
	driverComp, ok := s.GetComponentByName("Driver").(tracing.NamedHookable)
	if !ok {
		return
	}

	tracer := newMigrationTracer(s.GetEngine())
	tracing.CollectTrace(driverComp, tracer)
	r.migration = tracer
}

func isL1TLB(name string) bool {
	return strings.Contains(name, ".L1VTLB[") ||
		strings.HasSuffix(name, ".L1STLB") ||
		strings.HasSuffix(name, ".L1ITLB")
}

func (r *extendedReporter) report(base *reporter) {
	if *reportAll || *gmmuReportFlag {
		r.reportTranslationMetrics(base)
	}
	if *reportAll || *workingSetReportFlag {
		r.reportWorkingSetMetrics(base)
	}
	if *reportAll || *memoryFootprintReportFlag {
		r.reportMemoryMetrics(base)
	}
	if *reportAll || *pageMigrationReportFlag {
		r.reportMigrationMetrics(base)
	}
	// r.reportL2TLBMPKI(base) // sbin_codex: removed - now reported by the main reporter.
}

func (r *extendedReporter) reportTranslationMetrics(base *reporter) {
	for _, item := range r.gmmuTracers {
		reportTranslation(base, item.component.Name(), item.tracer, "gmmu")
	}
	for _, item := range r.mmuTracers {
		reportTranslation(base, item.component.Name(), item.tracer, "mmu")
	}
}

func reportTranslation(
	base *reporter,
	location string,
	tracer *gmmuTracer,
	prefix string,
) {
	insertMetric(base, location, prefix+"_translation_count",
		float64(tracer.TotalCount()), "count")
	insertMetric(base, location, prefix+"_translation_avg_latency",
		float64(tracer.AverageLatency()), "second")
	insertMetric(base, location, prefix+"_max_inflight",
		float64(tracer.MaxInflight()), "count")
	insertMetric(base, location, prefix+"_avg_inflight",
		float64(tracer.AverageInflight()), "count")

	counts := tracer.PageWalkCounts()
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kind, level, ok := pageWalkLevel(key)
		if !ok {
			continue
		}
		if !shouldReportPageWalkMetric(kind, level) {
			continue
		}
		kind = strings.ReplaceAll(kind, "-", "_")
		insertMetric(base, location,
			prefix+"_"+kind+"_"+itoa(level),
			float64(counts[key]), "count")
	}
}

func shouldReportPageWalkMetric(kind string, level int) bool {
	return kind != "pwc-miss" || level != 0
}

func (r *extendedReporter) reportWorkingSetMetrics(base *reporter) {
	if r.workingSet == nil {
		return
	}

	tracer := r.workingSet.tracer
	pageSize := uint64(1 << 12)
	if r.driver != nil && r.driver.Log2PageSize < 64 {
		pageSize = uint64(1 << r.driver.Log2PageSize)
	}
	pages := tracer.TotalPages()
	insertMetric(base, "WorkingSet", "working_set_pages",
		float64(pages), "count")
	insertMetric(base, "WorkingSet", "working_set_bytes",
		float64(pages*pageSize), "bytes")

	counts := tracer.PerGPUPageCounts()
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		insertMetric(base, key, "working_set_pages",
			float64(counts[key]), "count")
		insertMetric(base, key, "working_set_bytes",
			float64(counts[key]*pageSize), "bytes")
	}
}

func (r *extendedReporter) reportMemoryMetrics(base *reporter) {
	if r.driver == nil {
		return
	}

	stats := r.driver.MemoryStats()
	location := "Driver"
	insertMetric(base, location, "memory_page_size",
		float64(stats.PageSize), "bytes")
	insertMetric(base, location, "memory_footprint_live_pages",
		float64(stats.LivePageCount), "count")
	insertMetric(base, location, "memory_footprint_peak_pages",
		float64(stats.PeakPageCount), "count")
	insertMetric(base, location, "memory_footprint_total_pages",
		float64(stats.TotalPageCount), "count")
	insertMetric(base, location, "memory_footprint_live_bytes",
		float64(stats.LiveBytes), "bytes")
	insertMetric(base, location, "memory_footprint_peak_bytes",
		float64(stats.PeakBytes), "bytes")
	insertMetric(base, location, "memory_footprint_total_bytes",
		float64(stats.TotalBytes), "bytes")
}

func (r *extendedReporter) reportMigrationMetrics(base *reporter) {
	if r.migration == nil {
		return
	}

	location := "Driver"
	insertMetric(base, location, "page_migration_count",
		float64(r.migration.Count()), "count")
	insertMetric(base, location, "page_migration_pages",
		float64(r.migration.Pages()), "count")
	insertMetric(base, location, "page_migration_bytes",
		float64(r.migration.Bytes()), "bytes")
	insertMetric(base, location, "page_migration_avg_latency",
		float64(r.migration.AverageLatency()), "second")
	insertMetric(base, "PCIe", "pcie_page_migration_payload_bytes",
		float64(r.migration.Bytes()), "bytes")
}

func insertMetric(base *reporter, location, what string, value float64, unit string) {
	base.dataRecorder.InsertData(tableName, metric{
		Location: location,
		What:     what,
		Value:    value,
		Unit:     unit,
	})
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}

	negative := value < 0
	if negative {
		value = -value
	}
	buf := [20]byte{}
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// sbin_codex: L2 TLB MPKI reporting now reuses the main reporter's
// tlbHitRateTracers (L2TLB misses) and instCountTracers (retired
// instructions) instead of instrumenting every CU and L2TLB a second time.
// The removed per-GPU instrumentation is preserved below as comments.

/*
type instructionCountTracer struct {
	lock     sync.Mutex
	inflight map[string]struct{}
	count    uint64
}

func newInstructionCountTracer() *instructionCountTracer {
	return &instructionCountTracer{inflight: make(map[string]struct{})}
}

func (t *instructionCountTracer) StartTask(task tracing.Task) {
	if task.Kind != "inst" {
		return
	}
	t.lock.Lock()
	t.inflight[task.ID] = struct{}{}
	t.lock.Unlock()
}

func (t *instructionCountTracer) StepTask(_ tracing.Task) {}

func (t *instructionCountTracer) AddMilestone(_ tracing.Milestone) {}

func (t *instructionCountTracer) EndTask(task tracing.Task) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if _, ok := t.inflight[task.ID]; !ok {
		return
	}
	delete(t.inflight, task.ID)
	t.count++
}

func (t *instructionCountTracer) Count() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.count
}

type l2TLBMPKITracer struct {
	component tracing.NamedHookable
	gpu       string
	miss      *tracing.StepCountTracer
}

type l2TLBMPKIReporter struct {
	tracers      []*l2TLBMPKITracer
	instructions map[string]*instructionCountTracer
}

func (r *extendedReporter) injectL2TLBMPKITracer(s *simulation.Simulation) {
	if !*reportAll && !*tlbHitRateReportFlag && !*l2TLBMPKIReportFlag {
		return
	}

	reporter := &l2TLBMPKIReporter{
		instructions: make(map[string]*instructionCountTracer),
	}
	for _, comp := range s.Components() {
		hookable, ok := comp.(tracing.NamedHookable)
		if !ok {
			continue
		}

		if strings.Contains(comp.Name(), ".CU[") {
			gpu := gpuName(comp.Name())
			instructionTracer, found := reporter.instructions[gpu]
			if !found {
				instructionTracer = newInstructionCountTracer()
				reporter.instructions[gpu] = instructionTracer
			}
			tracing.CollectTrace(hookable, instructionTracer)
		}

		if strings.HasSuffix(comp.Name(), ".L2TLB") {
			missTracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { return true })
			tracing.CollectTrace(hookable, missTracer)
			reporter.tracers = append(reporter.tracers, &l2TLBMPKITracer{
				component: hookable,
				gpu:       gpuName(comp.Name()),
				miss:      missTracer,
			})
		}
	}
	r.l2TLBMPKI = reporter
}
*/

func calculateL2TLBMPKI(misses, instructions uint64) float64 {
	if instructions == 0 {
		return 0
	}
	return float64(misses) * 1000 / float64(instructions)
}

// reportL2TLBMPKI reports the L2 TLB miss rate per 1000 retired
// instructions: (L2TLB miss count / total CU instruction count) * 1000.
// sbin_codex: this reporter method reuses the tlbHitRateTracers and
// instCountTracers already injected by the main reporter, replacing the
// extendedReporter variant that carried its own second instrumentation.
func (r *reporter) reportL2TLBMPKI() {
	if len(r.tlbHitRateTracers) == 0 || len(r.instCountTracers) == 0 {
		return
	}

	var l2Miss uint64
	for _, tracer := range r.tlbHitRateTracers {
		if !strings.HasSuffix(tracer.tlb.Name(), ".L2TLB") {
			continue
		}
		l2Miss += tracer.tracer.GetStepCount("miss")
	}

	var instCount uint64
	for _, tracer := range r.instCountTracers {
		instCount += tracer.tracer.count
	}

	insertMetric(r, "GPU.L2TLB", "l2_tlb_mpki",
		calculateL2TLBMPKI(l2Miss, instCount),
		"miss/kilo-inst")
}

/*
func (r *extendedReporter) reportL2TLBMPKI(base *reporter) {
	if r.l2TLBMPKI == nil {
		return
	}

	for _, tracer := range r.l2TLBMPKI.tracers {
		instructionTracer := r.l2TLBMPKI.instructions[tracer.gpu]
		if instructionTracer == nil {
			continue
		}
		insertMetric(
			base,
			tracer.component.Name(),
			"l2_tlb_mpki",
			calculateL2TLBMPKI(
				tracer.miss.GetStepCount("miss"),
				instructionTracer.Count(),
			),
			"misses/1000 instructions",
		)
	}
}
*/
