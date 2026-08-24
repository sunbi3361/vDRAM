package cache

// sbin_codex: virtual-caching typed request annotation contract tests (plan
// todo 10 of mgpusim-uvm-manager). Prove the annotation type resolves from and
// attaches to access requests, survives request cloning (the cache clone
// sites rely on this), and reports generation staleness for the gate retry
// protocol.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

// TestUVMResolvedMetadataClone proves an annotated request resolves its
// VirtualAccessAnnotation, a clone of the request carries the same resolved
// metadata, and unannotated requests resolve to nil.
func TestUVMResolvedMetadataClone(t *testing.T) {
	ann := &VirtualAccessAnnotation{
		PID:        vm.PID(1),
		VAPage:     0x1000,
		HBMPA:      0x8000,
		Location:   vm.MemoryLocationGPU_LOCAL,
		Generation: 3,
	}

	read := mem.ReadReqBuilder{}.
		WithPID(1).
		WithAddress(0x1004).
		WithByteSize(4).
		Build()
	Annotate(read, ann)
	if got := ResolveAnnotation(read); got != ann {
		t.Fatalf("the read must resolve its annotation, got %+v", got)
	}

	cloned := read.Clone().(*mem.ReadReq)
	if got := ResolveAnnotation(cloned); got != ann {
		t.Fatalf("the cloned read must carry the resolved metadata, got %+v", got)
	}

	write := mem.WriteReqBuilder{}.
		WithPID(1).
		WithAddress(0x1004).
		WithData([]byte{1, 2, 3, 4}).
		Build()
	Annotate(write, ann)
	if got := ResolveAnnotation(write); got != ann {
		t.Fatalf("the write must resolve its annotation, got %+v", got)
	}

	plain := mem.ReadReqBuilder{}.
		WithPID(1).
		WithAddress(0x2000).
		WithByteSize(4).
		Build()
	if got := ResolveAnnotation(plain); got != nil {
		t.Fatalf("an unannotated request must resolve nil, got %+v", got)
	}
}

// TestUVMStaleGeneration proves the annotation generation comparison: an
// annotation whose generation differs from the current generation is stale
// and must retry; a matching generation is fresh.
func TestUVMStaleGeneration(t *testing.T) {
	ann := &VirtualAccessAnnotation{
		PID:        vm.PID(1),
		VAPage:     0x1000,
		HBMPA:      0x8000,
		Location:   vm.MemoryLocationGPU_LOCAL,
		Generation: 3,
	}
	if ann.IsStale(3) {
		t.Fatal("an annotation at the current generation must be fresh")
	}
	if !ann.IsStale(4) {
		t.Fatal("an annotation from an older generation must be stale")
	}
	if !ann.IsStale(2) {
		t.Fatal("an annotation from a newer generation must be stale")
	}

	zero := &VirtualAccessAnnotation{}
	if zero.IsStale(0) {
		t.Fatal("the zero annotation at zero current generation must be fresh")
	}
}
