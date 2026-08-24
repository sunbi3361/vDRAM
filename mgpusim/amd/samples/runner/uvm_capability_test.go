package runner

import (
	"flag"
	"testing"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
)

// sbin_codex: Todo 6 of mgpusim-uvm-manager — managed-memory capability
// propagation contract tests. -uvm must cause every benchmark registered via
// either runner path to receive SetManagedMemory, exactly once per path;
// -use-unified-memory takes precedence; combining both fails before
// allocation; disabled mode propagates nothing.

// recordingBenchmark records which capability methods the runner invoked.
type recordingBenchmark struct {
	selectedGPU  []int
	unifiedCalls int
	managedCalls int
	runCalls     int
	verifyCalls  int
}

func (b *recordingBenchmark) SelectGPU(gpuIDs []int) { b.selectedGPU = gpuIDs }
func (b *recordingBenchmark) Run()                   { b.runCalls++ }
func (b *recordingBenchmark) Verify()                { b.verifyCalls++ }
func (b *recordingBenchmark) SetUnifiedMemory()      { b.unifiedCalls++ }
func (b *recordingBenchmark) SetManagedMemory()      { b.managedCalls++ }

var _ benchmarks.Benchmark = (*recordingBenchmark)(nil)
var _ benchmarks.ManagedMemoryCapable = (*recordingBenchmark)(nil)

// plainBenchmark implements benchmarks.Benchmark but NOT
// benchmarks.ManagedMemoryCapable: it must not be forced to change.
type plainBenchmark struct {
	selectedGPU  []int
	unifiedCalls int
}

func (b *plainBenchmark) SelectGPU(gpuIDs []int) { b.selectedGPU = gpuIDs }
func (b *plainBenchmark) Run()                   {}
func (b *plainBenchmark) Verify()                {}
func (b *plainBenchmark) SetUnifiedMemory()      { b.unifiedCalls++ }

var _ benchmarks.Benchmark = (*plainBenchmark)(nil)

func TestUVMCapability(t *testing.T) {
	// UVM enabled: each registration path records exactly one managed
	// capability decision and no unified decision.
	r := &Runner{GPUIDs: []int{1}, uvmConfig: driver.UVMConfig{Enabled: true}}

	b1 := &recordingBenchmark{}
	r.AddBenchmark(b1)
	if b1.managedCalls != 1 {
		t.Errorf("AddBenchmark: managedCalls = %d, want exactly 1", b1.managedCalls)
	}
	if b1.unifiedCalls != 0 {
		t.Errorf("AddBenchmark: unifiedCalls = %d, want 0", b1.unifiedCalls)
	}
	if len(b1.selectedGPU) != 1 || b1.selectedGPU[0] != 1 {
		t.Errorf("AddBenchmark: selectedGPU = %v, want [1]", b1.selectedGPU)
	}

	b2 := &recordingBenchmark{}
	r.AddBenchmarkWithoutSettingGPUsToUse(b2)
	if b2.managedCalls != 1 {
		t.Errorf("AddBenchmarkWithoutSettingGPUsToUse: managedCalls = %d, want exactly 1",
			b2.managedCalls)
	}
	if b2.unifiedCalls != 0 {
		t.Errorf("AddBenchmarkWithoutSettingGPUsToUse: unifiedCalls = %d, want 0",
			b2.unifiedCalls)
	}

	// Unified memory takes precedence over UVM: only SetUnifiedMemory fires.
	r2 := &Runner{GPUIDs: []int{1}, UseUnifiedMemory: true,
		uvmConfig: driver.UVMConfig{Enabled: true}}
	b3 := &recordingBenchmark{}
	r2.AddBenchmark(b3)
	if b3.unifiedCalls != 1 {
		t.Errorf("unified precedence: unifiedCalls = %d, want exactly 1", b3.unifiedCalls)
	}
	if b3.managedCalls != 0 {
		t.Errorf("unified precedence: managedCalls = %d, want 0", b3.managedCalls)
	}

	// Disabled mode: no capability is propagated; the benchmark keeps its
	// original allocation expression.
	r3 := &Runner{GPUIDs: []int{1}}
	b4 := &recordingBenchmark{}
	r3.AddBenchmark(b4)
	if b4.unifiedCalls != 0 || b4.managedCalls != 0 {
		t.Errorf("disabled mode: capability calls = (%d, %d), want (0, 0)",
			b4.unifiedCalls, b4.managedCalls)
	}

	// A benchmark that is not managed-memory capable must not be forced to
	// change: UVM propagation is a no-op for it.
	r4 := &Runner{GPUIDs: []int{1}, uvmConfig: driver.UVMConfig{Enabled: true}}
	pb := &plainBenchmark{}
	r4.AddBenchmark(pb)
	if pb.unifiedCalls != 0 {
		t.Errorf("plain benchmark under UVM: unifiedCalls = %d, want 0", pb.unifiedCalls)
	}
}

func TestRejectUnifiedAndUVM(t *testing.T) {
	// Combining -use-unified-memory and -uvm must fail before allocation.
	if err := flag.Set("use-unified-memory", "true"); err != nil {
		t.Fatalf("flag.Set(use-unified-memory): %v", err)
	}
	if err := flag.Set("uvm", "true"); err != nil {
		t.Fatalf("flag.Set(uvm): %v", err)
	}
	defer func() {
		flag.Set("use-unified-memory", "false")
		flag.Set("uvm", "false")
	}()

	r := &Runner{}
	defer func() {
		if rcv := recover(); rcv == nil {
			t.Error("combining -use-unified-memory and -uvm: no panic, want panic")
		}
	}()
	r.parseFlag()
}
