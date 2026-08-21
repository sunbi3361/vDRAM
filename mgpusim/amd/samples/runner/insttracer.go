package runner

import (
	"fmt"
	"sync/atomic"

	"github.com/sarchlab/akita/v4/tracing"
	"github.com/tebeka/atexit"
)

var sharedInstCount uint64                       // sbin_codex
var progressInterval uint64                      // sbin_codex
var onMaxInstReached = func() { atexit.Exit(0) } // sbin_codex

// instTracer can trace the number of instruction completed.
type instTracer struct {
	count uint64
	// simdInst bool // sbin_codex: removed - EndTask reads the original task's What instead.
	simdCount uint64
	maxCount  uint64

	inflightInst map[string]tracing.Task
}

// newInstTracer creates a tracer that can count the number of instructions.
func newInstTracer() *instTracer {
	t := &instTracer{
		inflightInst: map[string]tracing.Task{},
	}
	return t
}

// newInstStopper with stop the execution after a given number of instructions
// is retired.
func newInstStopper(maxInst uint64) *instTracer {
	t := &instTracer{
		maxCount:     maxInst,
		inflightInst: map[string]tracing.Task{},
	}
	return t
}

func (t *instTracer) StartTask(task tracing.Task) {
	if task.Kind != "inst" {
		return
	}

	// if task.What == "VALU" { // sbin_codex: removed - a single stateful
	// 	t.simdInst = true // sbin_codex: simdInst field was clobbered by
	// } else { // sbin_codex: interleaved StartTask calls; EndTask now reads
	// 	t.simdInst = false // sbin_codex: the original task's What field.
	// } // sbin_codex

	t.inflightInst[task.ID] = task
}

func (t *instTracer) StepTask(task tracing.Task) {
	// Do nothing
}

func (t *instTracer) AddMilestone(milestone tracing.Milestone) {
	// Do nothing
}

func (t *instTracer) EndTask(task tracing.Task) {
	orgTask, found := t.inflightInst[task.ID]
	if !found {
		return
	}

	// sbin_codex: original: if t.simdInst { t.simdCount++ }
	// sbin_codex: check the original task's What instead of a stateful bool,
	// so interleaved StartTask calls cannot misattribute the VALU count.
	if orgTask.What == "VALU" {
		t.simdCount++
	}

	delete(t.inflightInst, task.ID)

	t.count++

	// sbin_codex
	n := atomic.AddUint64(&sharedInstCount, 1)
	if progressInterval > 0 && n%progressInterval == 0 {
		fmt.Printf("[inst-progress] %d instructions retired\n", n)
	}
	if t.maxCount > 0 && n >= t.maxCount {
		onMaxInstReached()
	}
}
