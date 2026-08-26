package runner

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sarchlab/akita/v4/tracing"
)

var sharedInstCount uint64  // sbin_codex
var progressInterval uint64 // sbin_codex

// Pre-edit code (commented per project convention):
// var onMaxInstReached = func() { atexit.Exit(0) } // sbin_codex
//
// sbin_claude: EndTask runs inside a CU's tracing hook, i.e. on one of the
// ParallelEngine's event goroutines while every other component is still
// handling events. Reporting from there walked the CU tracers' maps while
// their owners were writing them, which Go answers with either
// "concurrent map iteration and map write" or a corrupted heap (SIGSEGV in
// the GC's stack scan). The tracer now only raises a signal; the reporting
// happens on a separate goroutine that first pauses the engine.
var maxInstOnce sync.Once                // sbin_claude
var maxInstReached = make(chan struct{}) // sbin_claude

// MaxInstReached returns the channel that is closed once -max-inst retired
// instructions have completed. // sbin_claude
func MaxInstReached() <-chan struct{} {
	return maxInstReached
}

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
	// Pre-edit code (commented per project convention):
	// if t.maxCount > 0 && n >= t.maxCount {
	// 	onMaxInstReached()
	// }
	//
	// sbin_claude: signal only. Closing the channel is the whole handler, so
	// nothing that touches tracer state runs on the engine's goroutines.
	if t.maxCount > 0 && n >= t.maxCount {
		maxInstOnce.Do(func() { close(maxInstReached) })
	}
}
