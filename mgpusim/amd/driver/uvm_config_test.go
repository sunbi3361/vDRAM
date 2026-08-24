package driver

import (
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: UVM config contract tests (todo 1 of mgpusim-uvm-manager).
// These tests lock the §26 defaults, the domain/combination rules, the
// capacity rules, and the builder manager construction (enabled => manager,
// disabled => nil).

func TestUVMConfigDefaults(t *testing.T) {
	cfg := DefaultUVMConfig()

	if cfg.Enabled {
		t.Error("DefaultUVMConfig: Enabled must default to false")
	}
	if cfg.Ideal {
		t.Error("DefaultUVMConfig: Ideal must default to false")
	}
	if cfg.AccessCounter {
		t.Error("DefaultUVMConfig: AccessCounter must default to false")
	}
	if cfg.FaultHandlingLatency != 20*time.Microsecond {
		t.Errorf("DefaultUVMConfig: FaultHandlingLatency = %v, want 20us",
			cfg.FaultHandlingLatency)
	}
	if cfg.AccessCounterThreshold != 8 {
		t.Errorf("DefaultUVMConfig: AccessCounterThreshold = %d, want 8",
			cfg.AccessCounterThreshold)
	}
	if cfg.VABlockSize != 2*mem.MB {
		t.Errorf("DefaultUVMConfig: VABlockSize = %d, want 2MB", cfg.VABlockSize)
	}
	if cfg.TBNMinNodeSize != 64*mem.KB {
		t.Errorf("DefaultUVMConfig: TBNMinNodeSize = %d, want 64KB",
			cfg.TBNMinNodeSize)
	}
	if cfg.Prefetcher != "tbn" {
		t.Errorf("DefaultUVMConfig: Prefetcher = %q, want tbn", cfg.Prefetcher)
	}
	if cfg.CapacitySet {
		t.Error("DefaultUVMConfig: CapacitySet must default to false")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("DefaultUVMConfig: Validate() = %v, want nil", err)
	}
	if err := cfg.ValidateCapacity(4 * mem.GB); err != nil {
		t.Errorf("DefaultUVMConfig: ValidateCapacity() = %v, want nil", err)
	}
}

func TestUVMConfigDomains(t *testing.T) {
	base := DefaultUVMConfig()
	base.Enabled = true

	valid := []struct {
		name string
		mut  func(*UVMConfig)
	}{
		{"enabled", func(c *UVMConfig) {}},
		{"enabled+ideal", func(c *UVMConfig) { c.Ideal = true }},
		{"enabled+access-counter", func(c *UVMConfig) { c.AccessCounter = true }},
		{"enabled+ideal+access-counter", func(c *UVMConfig) {
			c.Ideal = true
			c.AccessCounter = true
		}},
		{"enabled+capacity-64kb", func(c *UVMConfig) {
			c.GPUMemoryCapacity = 64 * mem.KB
			c.CapacitySet = true
		}},
		{"enabled+capacity-full-dram", func(c *UVMConfig) {
			c.GPUMemoryCapacity = 4 * mem.GB
			c.CapacitySet = true
		}},
	}

	for _, tc := range valid {
		cfg := base
		tc.mut(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want nil", tc.name, err)
		}
		if err := cfg.ValidateCapacity(4 * mem.GB); err != nil {
			t.Errorf("%s: ValidateCapacity() = %v, want nil", tc.name, err)
		}
	}
}

func TestUVMInvalidCombinations(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*UVMConfig)
	}{
		{"ideal-without-uvm", func(c *UVMConfig) { c.Ideal = true }},
		{"negative-latency", func(c *UVMConfig) {
			c.Enabled = true
			c.FaultHandlingLatency = -time.Nanosecond
		}},
		{"zero-threshold", func(c *UVMConfig) {
			c.Enabled = true
			c.AccessCounterThreshold = 0
		}},
		{"negative-threshold", func(c *UVMConfig) {
			c.Enabled = true
			c.AccessCounterThreshold = -1
		}},
		{"vablock-too-small", func(c *UVMConfig) {
			c.Enabled = true
			c.VABlockSize = 64 * mem.KB
		}},
		{"vablock-zero", func(c *UVMConfig) {
			c.Enabled = true
			c.VABlockSize = 0
		}},
		{"tbn-too-small", func(c *UVMConfig) {
			c.Enabled = true
			c.TBNMinNodeSize = 4 * mem.KB
		}},
		{"tbn-zero", func(c *UVMConfig) {
			c.Enabled = true
			c.TBNMinNodeSize = 0
		}},
		{"bad-prefetcher", func(c *UVMConfig) {
			c.Enabled = true
			c.Prefetcher = "lru"
		}},
		{"empty-prefetcher", func(c *UVMConfig) {
			c.Enabled = true
			c.Prefetcher = ""
		}},
		{"capacity-too-small", func(c *UVMConfig) {
			c.Enabled = true
			c.GPUMemoryCapacity = 4 * mem.KB
			c.CapacitySet = true
		}},
		{"capacity-unaligned", func(c *UVMConfig) {
			c.Enabled = true
			c.GPUMemoryCapacity = 64*mem.KB + 1
			c.CapacitySet = true
		}},
	}

	for _, tc := range cases {
		cfg := DefaultUVMConfig()
		tc.mut(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", tc.name)
		}
	}
}

func TestUVMCapacityValidation(t *testing.T) {
	base := DefaultUVMConfig()
	base.Enabled = true

	// Explicit capacities that are aligned, >= 64KB, and within DRAM.
	for _, cap := range []uint64{64 * mem.KB, 2 * mem.MB, 4 * mem.GB} {
		cfg := base
		cfg.GPUMemoryCapacity = cap
		cfg.CapacitySet = true
		if err := cfg.Validate(); err != nil {
			t.Errorf("capacity %d: Validate() = %v, want nil", cap, err)
		}
		if err := cfg.ValidateCapacity(4 * mem.GB); err != nil {
			t.Errorf("capacity %d: ValidateCapacity() = %v, want nil", cap, err)
		}
		if got := cfg.ResolvedCapacity(4 * mem.GB); got != cap {
			t.Errorf("capacity %d: ResolvedCapacity() = %d, want %d", cap, got, cap)
		}
	}

	// Omitted capacity resolves to the full available GPU memory.
	cfg := base
	cfg.CapacitySet = false
	if got := cfg.ResolvedCapacity(4 * mem.GB); got != 4*mem.GB {
		t.Errorf("omitted capacity: ResolvedCapacity() = %d, want 4GB", got)
	}

	// Capacity exceeding available GPU DRAM is rejected by ValidateCapacity.
	cfg = base
	cfg.GPUMemoryCapacity = 8 * mem.GB
	cfg.CapacitySet = true
	if err := cfg.ValidateCapacity(4 * mem.GB); err == nil {
		t.Error("capacity 8GB > 4GB DRAM: ValidateCapacity() = nil, want error")
	}
}

func TestUVMBuilderManager(t *testing.T) {
	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)

	cfg := DefaultUVMConfig()
	cfg.Enabled = true

	driver := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	if driver.uvm == nil {
		t.Fatal("Build with enabled UVM config: driver.uvm = nil, want non-nil manager")
	}
	if driver.uvm.capacity != 4*mem.GB {
		t.Errorf("manager capacity = %d, want 4GB (omitted capacity => full DRAM)",
			driver.uvm.capacity)
	}
	if !driver.uvm.config.Enabled {
		t.Error("manager config.Enabled = false, want true")
	}
	if driver.uvm.config.VABlockSize != 2*mem.MB {
		t.Errorf("manager config.VABlockSize = %d, want 2MB",
			driver.uvm.config.VABlockSize)
	}
}

func TestUVMDisabledManager(t *testing.T) {
	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)

	driver := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		Build("Driver")

	if driver.uvm != nil {
		t.Fatal("Build without UVM config: driver.uvm != nil, want nil")
	}

	// Disabled mode: the only rejection path is AllocateManagedMemory panicking
	// when d.uvm == nil.
	defer func() {
		if r := recover(); r == nil {
			t.Error("AllocateManagedMemory with uvm == nil: no panic, want panic")
		}
	}()
	ctx := driver.Init()
	driver.AllocateManagedMemory(ctx, 4096)
}
