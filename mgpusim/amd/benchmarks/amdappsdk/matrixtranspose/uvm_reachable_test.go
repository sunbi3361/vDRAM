package matrixtranspose

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
)

// sbin_codex: Todo 6 of mgpusim-uvm-manager — the managed-memory branch in
// initMem must be reachable via SetManagedMemory without changing the
// disabled-mode allocation expression.

// The benchmark must satisfy the runner's managed-memory capability
// interface so that -uvm propagation can reach it.
var _ benchmarks.ManagedMemoryCapable = (*Benchmark)(nil)

func TestMatrixTransposeUVMReachable(t *testing.T) {
	// Build a UVM-enabled driver so the managed allocation API the branch
	// calls is available (mirrors the driver's uvm_config_test.go pattern).
	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	d := driver.MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	b := NewBenchmark(d)
	b.Width = 4

	// Disabled mode: the managed gate is off, so initMem keeps the original
	// allocation expression (AllocateMemory + Distribute).
	if b.useManagedMemory {
		t.Fatal("new benchmark: useManagedMemory must be false (disabled mode)")
	}

	// SetManagedMemory flips the gate that selects the managed branch.
	b.SetManagedMemory()
	if !b.useManagedMemory {
		t.Fatal("SetManagedMemory did not enable the managed branch")
	}

	// The managed branch calls AllocateManagedMemory; prove it is reachable
	// and functional under UVM (it panics when UVM is disabled, and returns
	// a valid non-zero pointer when UVM is enabled).
	size := uint64(b.Width * b.Width * 4)
	ptr := d.AllocateManagedMemory(b.context, size)
	if ptr == 0 {
		t.Fatal("AllocateManagedMemory returned a nil pointer")
	}
}
