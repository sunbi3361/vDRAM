package driver

import (
	"fmt"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
)

// sbin_codex: UVMConfig carries the validated UVM experiment configuration
// (uvm-manager.md §26). The runner parses its flags into this type; the
// timingconfig builder hands it to the driver Builder.WithUVMConfig; the
// driver Builder.Build constructs the UVMManager only when Enabled.

// UVMConfig is the validated configuration for the driver-owned UVM manager.
// The zero value is a valid "disabled" configuration (Enabled == false).
type UVMConfig struct {
	Enabled               bool          // -uvm
	Ideal                 bool          // -uvm-ideal (requires Enabled)
	AccessCounter         bool          // -uvm-access-counter
	FaultHandlingLatency  time.Duration // -uvm-fault-handling-latency (>= 0)
	AccessCounterThreshold int          // -uvm-access-counter-threshold (> 0)
	VABlockSize           uint64        // -uvm-vablock-size (exactly 2MB)
	TBNMinNodeSize        uint64        // -uvm-tbn-min-node-size (exactly 64KB)
	GPUMemoryCapacity     uint64        // -uvm-gpu-memory-capacity (0 when omitted)
	CapacitySet           bool          // whether -uvm-gpu-memory-capacity was set
	Prefetcher            string        // -uvm-prefetcher (exactly "tbn")
}

// DefaultUVMConfig returns the canonical §26 defaults. The runner's CLI flag
// defaults must stay in sync with these values.
func DefaultUVMConfig() UVMConfig {
	return UVMConfig{
		Enabled:               false,
		Ideal:                 false,
		AccessCounter:         false,
		FaultHandlingLatency:  20 * time.Microsecond,
		AccessCounterThreshold: 8,
		VABlockSize:           2 * mem.MB,
		TBNMinNodeSize:         64 * mem.KB,
		GPUMemoryCapacity:     0,
		CapacitySet:           false,
		Prefetcher:            "tbn",
	}
}

// Validate checks every §26 domain/combination rule that does not depend on
// the actual GPU DRAM size. It returns a descriptive error for the first
// violation; nil means the configuration domain is valid.
func (c UVMConfig) Validate() error {
	if c.Ideal && !c.Enabled {
		return fmt.Errorf("uvm: -uvm-ideal requires -uvm to be enabled")
	}

	if c.FaultHandlingLatency < 0 {
		return fmt.Errorf(
			"uvm: -uvm-fault-handling-latency must be non-negative, got %v",
			c.FaultHandlingLatency)
	}

	if c.AccessCounterThreshold <= 0 {
		return fmt.Errorf(
			"uvm: -uvm-access-counter-threshold must be > 0, got %d",
			c.AccessCounterThreshold)
	}

	if c.VABlockSize != 2*mem.MB {
		return fmt.Errorf(
			"uvm: -uvm-vablock-size must be exactly 2MB, got %d",
			c.VABlockSize)
	}

	if c.TBNMinNodeSize != 64*mem.KB {
		return fmt.Errorf(
			"uvm: -uvm-tbn-min-node-size must be exactly 64KB, got %d",
			c.TBNMinNodeSize)
	}

	if c.Prefetcher != "tbn" {
		return fmt.Errorf(
			"uvm: -uvm-prefetcher must be exactly \"tbn\", got %q",
			c.Prefetcher)
	}

	if c.CapacitySet {
		if c.GPUMemoryCapacity < 64*mem.KB {
			return fmt.Errorf(
				"uvm: -uvm-gpu-memory-capacity must be >= 64KB, got %d",
				c.GPUMemoryCapacity)
		}
		if c.GPUMemoryCapacity%(4*mem.KB) != 0 {
			return fmt.Errorf(
				"uvm: -uvm-gpu-memory-capacity must be 4KB-aligned, got %d",
				c.GPUMemoryCapacity)
		}
	}

	return nil
}

// ValidateCapacity checks the explicit -uvm-gpu-memory-capacity against the
// actual GPU DRAM/frames availability. availableGPUMemory is the total GPU
// DRAM the allocator can back (supplied by the timingconfig builder, which
// owns the DRAM sizes). An omitted capacity is always valid.
func (c UVMConfig) ValidateCapacity(availableGPUMemory uint64) error {
	if !c.CapacitySet {
		return nil
	}
	if c.GPUMemoryCapacity > availableGPUMemory {
		return fmt.Errorf(
			"uvm: -uvm-gpu-memory-capacity %d exceeds available GPU memory %d",
			c.GPUMemoryCapacity, availableGPUMemory)
	}
	return nil
}

// ResolvedCapacity returns the effective UVM GPU capacity: the explicit
// -uvm-gpu-memory-capacity when set, otherwise the full available GPU DRAM.
func (c UVMConfig) ResolvedCapacity(availableGPUMemory uint64) uint64 {
	if c.CapacitySet {
		return c.GPUMemoryCapacity
	}
	return availableGPUMemory
}
