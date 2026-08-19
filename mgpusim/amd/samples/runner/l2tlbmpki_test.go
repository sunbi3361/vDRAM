// sbin_codex: regression coverage for GPU-level L2 TLB MPKI reporting.
package runner

import (
	"testing"

	"github.com/sarchlab/akita/v4/tracing"
)

func TestCalculateL2TLBMPKI(t *testing.T) {
	if got := calculateL2TLBMPKI(25, 10000); got != 2.5 {
		t.Fatalf("L2 TLB MPKI = %v, want 2.5", got)
	}
	if got := calculateL2TLBMPKI(25, 0); got != 0 {
		t.Fatalf("L2 TLB MPKI with no instructions = %v, want 0", got)
	}
}

func TestInstructionCountTracerCountsRetiredInstructions(t *testing.T) {
	tracer := newInstructionCountTracer()

	tracer.StartTask(tracing.Task{ID: "inst-1", Kind: "inst"})
	tracer.StartTask(tracing.Task{ID: "other-1", Kind: "fetch"})
	tracer.EndTask(tracing.Task{ID: "inst-1"})
	tracer.EndTask(tracing.Task{ID: "other-1"})

	if got := tracer.Count(); got != 1 {
		t.Fatalf("retired instruction count = %d, want 1", got)
	}
}
