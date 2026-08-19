// sbin_codex: regression coverage for summary-only extended tracers.
package runner

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type testTimeTeller struct {
	now sim.VTimeInSec
}

func (t *testTimeTeller) CurrentTime() sim.VTimeInSec {
	return t.now
}

func TestGMMUTracerAggregatesTranslationAndPageWalkCacheStats(t *testing.T) {
	clock := &testTimeTeller{}
	tracer := newGMMUTracer(clock)
	req := &vm.TranslationReq{PID: 7, VAddr: 0x1234}
	task := tracing.Task{
		ID:     "translation-1",
		Kind:   "req_in",
		Detail: req,
	}

	tracer.StartTask(task)
	clock.now = 3
	tracer.StepTask(tracing.Task{Steps: []tracing.TaskStep{{What: "pwc-hit-level2"}}})
	tracer.StepTask(tracing.Task{Steps: []tracing.TaskStep{{What: "pwc-miss-level1"}}})
	tracer.StepTask(tracing.Task{Steps: []tracing.TaskStep{{What: "pwc-miss-level0"}}})
	clock.now = 9
	tracer.EndTask(tracing.Task{ID: task.ID})

	if got := tracer.TotalCount(); got != 1 {
		t.Fatalf("translation count = %d, want 1", got)
	}
	if got := tracer.AverageLatency(); got != 9 {
		t.Fatalf("average latency = %v, want 9", got)
	}
	if got := tracer.MaxInflight(); got != 1 {
		t.Fatalf("max inflight = %d, want 1", got)
	}
	if got := tracer.StepCount("pwc-hit-level2"); got != 1 {
		t.Fatalf("PWC hit count = %d, want 1", got)
	}
	if got := tracer.StepCount("pwc-miss-level1"); got != 1 {
		t.Fatalf("PWC miss count = %d, want 1", got)
	}
	if got := tracer.StepCount("pwc-miss-level0"); got != 0 {
		t.Fatalf("leaf PWC miss count = %d, want 0", got)
	}
}

func TestGMMUTracerReportsTimeWeightedAverageInflight(t *testing.T) {
	clock := &testTimeTeller{}
	tracer := newGMMUTracer(clock)
	request := func(id string) tracing.Task {
		return tracing.Task{
			ID:     id,
			Kind:   "req_in",
			Detail: &vm.TranslationReq{PID: 1, VAddr: 0x1000},
		}
	}

	tracer.StartTask(request("translation-1"))
	clock.now = 2
	tracer.StartTask(request("translation-2"))
	clock.now = 4
	tracer.EndTask(tracing.Task{ID: "translation-1"})
	clock.now = 6
	tracer.EndTask(tracing.Task{ID: "translation-2"})

	if got := tracer.AverageInflight(); got != sim.VTimeInSec(4.0/3.0) {
		t.Fatalf("average inflight = %v, want %v", got, 4.0/3.0)
	}
}

func TestPageWalkMetricSkipsLastLevelCacheMiss(t *testing.T) {
	if shouldReportPageWalkMetric("pwc-miss", 0) {
		t.Fatal("last-level page-walk-cache miss should not be reported")
	}
	if !shouldReportPageWalkMetric("pwc-miss", 1) {
		t.Fatal("non-last-level page-walk-cache miss should be reported")
	}
}

func TestWorkingSetTracerDeduplicatesPagesWithoutPerPageOutput(t *testing.T) {
	tracer := newWorkingSetTracer(4096)

	tracer.StartTask(tracing.Task{
		Kind:     "req_in",
		Location: "GPU[1].ShaderArray[0].L1VTLB[0]",
		Detail:   &vm.TranslationReq{PID: 1, VAddr: 0x1001},
	})
	tracer.StartTask(tracing.Task{
		Kind:     "req_in",
		Location: "GPU[1].ShaderArray[0].L1STLB",
		Detail:   &vm.TranslationReq{PID: 1, VAddr: 0x1fff},
	})
	tracer.StartTask(tracing.Task{
		Kind:     "req_in",
		Location: "GPU[2].ShaderArray[0].L1VTLB[0]",
		Detail:   &vm.TranslationReq{PID: 2, VAddr: 0x1001},
	})

	if got := tracer.TotalPages(); got != 2 {
		t.Fatalf("working-set pages = %d, want 2", got)
	}
	perGPU := tracer.PerGPUPageCounts()
	if perGPU["GPU[1]"] != 1 || perGPU["GPU[2]"] != 1 {
		t.Fatalf("per-GPU working set = %#v, want one page per GPU", perGPU)
	}
}

func TestMigrationTracerReportsSummaryOnly(t *testing.T) {
	clock := &testTimeTeller{}
	tracer := newMigrationTracer(clock)
	req := &vm.PageMigrationReqToDriver{
		PageSize: 4096,
		MigrationInfo: &vm.PageMigrationInfo{
			GPUReqToVAddrMap: map[uint64][]uint64{
				1: {0x1000, 0x2000},
				2: {0x3000},
			},
		},
	}

	tracer.StartTask(tracing.Task{
		ID:     "migration-1",
		Kind:   "page_migration",
		Detail: req,
	})
	clock.now = 5
	tracer.EndTask(tracing.Task{ID: "migration-1"})

	if got := tracer.Count(); got != 1 {
		t.Fatalf("migration count = %d, want 1", got)
	}
	if got := tracer.Pages(); got != 3 {
		t.Fatalf("migrated pages = %d, want 3", got)
	}
	if got := tracer.Bytes(); got != 3*4096 {
		t.Fatalf("migrated bytes = %d, want %d", got, 3*4096)
	}
	if got := tracer.AverageLatency(); got != 5 {
		t.Fatalf("migration latency = %v, want 5", got)
	}
}
