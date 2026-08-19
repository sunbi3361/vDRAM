// sbin_codex: aggregate GMMU/MMU translation and page-walk-cache statistics.
package runner

import (
	"strconv"
	"strings"
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type gmmuTask struct {
	start sim.VTimeInSec
}

// gmmuTracer aggregates translation work performed by one GMMU. It keeps only
// counters and in-flight start times; it does not retain per-fault records.
type gmmuTracer struct {
	timeTeller sim.TimeTeller

	lock           sync.Mutex
	inflight       map[string]gmmuTask
	totalCount     uint64
	totalLatency   sim.VTimeInSec
	maxInflight    uint64
	inflightArea   sim.VTimeInSec // sbin_codex: time integral of in-flight requests.
	firstEventTime sim.VTimeInSec // sbin_codex: start of the occupancy interval.
	lastEventTime  sim.VTimeInSec // sbin_codex: end of the occupancy interval.
	hasObservation bool           // sbin_codex: occupancy interval has started.
	pageWalkCounts map[string]uint64
}

func newGMMUTracer(timeTeller sim.TimeTeller) *gmmuTracer {
	return &gmmuTracer{
		timeTeller:     timeTeller,
		inflight:       make(map[string]gmmuTask),
		pageWalkCounts: make(map[string]uint64),
	}
}

func (t *gmmuTracer) StartTask(task tracing.Task) {
	if task.Kind != "req_in" {
		return
	}
	if _, ok := task.Detail.(*vm.TranslationReq); !ok {
		return
	}

	t.lock.Lock()
	defer t.lock.Unlock()

	now := t.timeTeller.CurrentTime()
	t.advanceInflight(now) // sbin_codex: integrate occupancy before adding a request.
	t.inflight[task.ID] = gmmuTask{start: now}
	if uint64(len(t.inflight)) > t.maxInflight {
		t.maxInflight = uint64(len(t.inflight))
	}
}

func (t *gmmuTracer) StepTask(task tracing.Task) {
	if len(task.Steps) == 0 {
		return
	}

	t.lock.Lock()
	defer t.lock.Unlock()

	for _, step := range task.Steps {
		if strings.HasPrefix(step.What, "pwc-hit-level") ||
			strings.HasPrefix(step.What, "pwc-miss-level") {
			if step.What == "pwc-miss-level0" {
				continue // sbin_codex: leaf PTE is not cached by the page-walk cache.
			}
			t.pageWalkCounts[step.What]++
		}
	}
}

func (t *gmmuTracer) AddMilestone(_ tracing.Milestone) {}

func (t *gmmuTracer) EndTask(task tracing.Task) {
	t.lock.Lock()
	defer t.lock.Unlock()

	original, ok := t.inflight[task.ID]
	if !ok {
		return
	}
	now := t.timeTeller.CurrentTime()
	t.advanceInflight(now) // sbin_codex: integrate occupancy before removing a request.

	t.totalCount++
	t.totalLatency += now - original.start
	delete(t.inflight, task.ID)
}

func (t *gmmuTracer) TotalCount() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.totalCount
}

func (t *gmmuTracer) AverageLatency() sim.VTimeInSec {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.totalCount == 0 {
		return 0
	}
	return t.totalLatency / sim.VTimeInSec(t.totalCount)
}

func (t *gmmuTracer) MaxInflight() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.maxInflight
}

// AverageInflight returns the time-weighted translation concurrency.
func (t *gmmuTracer) AverageInflight() sim.VTimeInSec {
	t.lock.Lock()
	defer t.lock.Unlock()

	if !t.hasObservation {
		return 0
	}

	area := t.inflightArea
	end := t.lastEventTime
	if len(t.inflight) > 0 {
		now := t.timeTeller.CurrentTime()
		area += sim.VTimeInSec(float64(len(t.inflight))) *
			(now - t.lastEventTime)
		end = now
	}

	duration := end - t.firstEventTime
	if duration <= 0 {
		return 0
	}
	return area / duration
}

func (t *gmmuTracer) StepCount(name string) uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.pageWalkCounts[name]
}

func (t *gmmuTracer) PageWalkCounts() map[string]uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()

	counts := make(map[string]uint64, len(t.pageWalkCounts))
	for name, count := range t.pageWalkCounts {
		counts[name] = count
	}
	return counts
}

func (t *gmmuTracer) advanceInflight(now sim.VTimeInSec) {
	if !t.hasObservation {
		t.firstEventTime = now
		t.lastEventTime = now
		t.hasObservation = true
		return
	}

	t.inflightArea += sim.VTimeInSec(float64(len(t.inflight))) *
		(now - t.lastEventTime)
	t.lastEventTime = now
}

func pageWalkLevel(stepName string) (string, int, bool) {
	parts := strings.Split(stepName, "-level")
	if len(parts) != 2 {
		return "", 0, false
	}
	level, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, false
	}
	return parts[0], level, true
}
