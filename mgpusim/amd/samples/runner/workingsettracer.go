// sbin_codex: aggregate distinct L1-TLB working-set pages without page rows.
package runner

import (
	"strings"
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/tracing"
)

type workingSetPage struct {
	pid  vm.PID
	page uint64
}

// workingSetTracer counts distinct process pages observed by the L1 TLBs.
// It retains page identities for deduplication but never emits them as
// per-page report records.
type workingSetTracer struct {
	pageSize uint64

	lock   sync.Mutex
	all    map[workingSetPage]struct{}
	perGPU map[string]map[workingSetPage]struct{}
}

func newWorkingSetTracer(pageSize uint64) *workingSetTracer {
	return &workingSetTracer{
		pageSize: pageSize,
		all:      make(map[workingSetPage]struct{}),
		perGPU:   make(map[string]map[workingSetPage]struct{}),
	}
}

func (t *workingSetTracer) StartTask(task tracing.Task) {
	if task.Kind != "req_in" {
		return
	}
	req, ok := task.Detail.(*vm.TranslationReq)
	if !ok || t.pageSize == 0 {
		return
	}

	page := workingSetPage{pid: req.PID, page: req.VAddr / t.pageSize}
	gpu := gpuName(task.Location)

	t.lock.Lock()
	defer t.lock.Unlock()
	t.all[page] = struct{}{}
	if _, ok := t.perGPU[gpu]; !ok {
		t.perGPU[gpu] = make(map[workingSetPage]struct{})
	}
	t.perGPU[gpu][page] = struct{}{}
}

func (t *workingSetTracer) StepTask(_ tracing.Task)          {}
func (t *workingSetTracer) AddMilestone(_ tracing.Milestone) {}
func (t *workingSetTracer) EndTask(_ tracing.Task)           {}

func (t *workingSetTracer) TotalPages() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return uint64(len(t.all))
}

func (t *workingSetTracer) PerGPUPageCounts() map[string]uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()

	counts := make(map[string]uint64, len(t.perGPU))
	for gpu, pages := range t.perGPU {
		counts[gpu] = uint64(len(pages))
	}
	return counts
}

func gpuName(location string) string {
	if dot := strings.IndexByte(location, '.'); dot > 0 {
		return location[:dot]
	}
	return location
}
