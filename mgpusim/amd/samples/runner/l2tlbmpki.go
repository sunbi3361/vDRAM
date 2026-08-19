// sbin_codex: collect and report GPU-level L2 TLB MPKI.
package runner

import (
	"strings"
	"sync"

	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/akita/v4/tracing"
)

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

func calculateL2TLBMPKI(misses, instructions uint64) float64 {
	if instructions == 0 {
		return 0
	}
	return float64(misses) * 1000 / float64(instructions)
}

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
