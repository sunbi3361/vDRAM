// sbin_codex: report extended memory-system summaries without page-fault detail.
package runner

import (
	"sort"
	"strings"

	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
)

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
		float64(tracer.AverageInflight()), "count") // sbin_codex: report time-weighted occupancy.

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
	// sbin_codex: level zero is the leaf PTE and has no page-walk-cache entry.
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
