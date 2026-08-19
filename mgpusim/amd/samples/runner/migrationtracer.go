// sbin_codex: aggregate page-migration timing and payload statistics only.
package runner

import (
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type migrationTask struct {
	start sim.VTimeInSec
	bytes uint64
	pages uint64
}

// migrationTracer reports logical page-migration summaries only. It does not
// retain PID, virtual address, GPU, or per-page latency records.
type migrationTracer struct {
	timeTeller sim.TimeTeller

	lock         sync.Mutex
	inflight     map[string]migrationTask
	count        uint64
	pages        uint64
	bytes        uint64
	totalLatency sim.VTimeInSec
}

func newMigrationTracer(timeTeller sim.TimeTeller) *migrationTracer {
	return &migrationTracer{
		timeTeller: timeTeller,
		inflight:   make(map[string]migrationTask),
	}
}

func (t *migrationTracer) StartTask(task tracing.Task) {
	if task.Kind != "page_migration" {
		return
	}
	req, ok := task.Detail.(*vm.PageMigrationReqToDriver)
	if !ok {
		return
	}

	pages := migrationPageCount(req)
	t.lock.Lock()
	t.inflight[task.ID] = migrationTask{
		start: t.timeTeller.CurrentTime(),
		bytes: pages * req.PageSize,
		pages: pages,
	}
	t.lock.Unlock()
}

func (t *migrationTracer) StepTask(_ tracing.Task)          {}
func (t *migrationTracer) AddMilestone(_ tracing.Milestone) {}

func (t *migrationTracer) EndTask(task tracing.Task) {
	t.lock.Lock()
	defer t.lock.Unlock()

	original, ok := t.inflight[task.ID]
	if !ok {
		return
	}
	t.count++
	t.pages += original.pages
	t.bytes += original.bytes
	t.totalLatency += t.timeTeller.CurrentTime() - original.start
	delete(t.inflight, task.ID)
}

func (t *migrationTracer) Count() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.count
}

func (t *migrationTracer) Pages() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.pages
}

func (t *migrationTracer) Bytes() uint64 {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.bytes
}

func (t *migrationTracer) AverageLatency() sim.VTimeInSec {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.count == 0 {
		return 0
	}
	return t.totalLatency / sim.VTimeInSec(t.count)
}

func migrationPageCount(req *vm.PageMigrationReqToDriver) uint64 {
	if req.MigrationInfo == nil {
		return 1
	}

	var pages uint64
	for _, vAddrs := range req.MigrationInfo.GPUReqToVAddrMap {
		pages += uint64(len(vAddrs))
	}
	if pages == 0 {
		return 1
	}
	return pages
}
