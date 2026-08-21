package driver

import (
	"github.com/sarchlab/akita/v4/sim"
)

// UVMConfig carries the tunable UVM demand-paging parameters. Values are
// supplied through the runner flags and forwarded by the driver builder.
type UVMConfig struct {
	// Enabled enables UVM demand-paged managed memory.
	Enabled bool
	// Ideal disables fault-handling and migration timing while keeping the
	// functional state machine active.
	Ideal bool

	// Log2PageSize is the base 4KB translation page size (log2).
	Log2PageSize uint64
	// GPUCoreFrequency is used to convert the fixed fault latency into cycles.
	GPUCoreFrequency sim.Freq

	// FaultLatencyUS is the fixed host/driver page-fault handling latency in
	// microseconds, charged once per unique fault batch.
	FaultLatencyUS float64
	// AccessCounterThreshold triggers a 64KB CPU->GPU migration.
	AccessCounterThreshold uint64
	// TBNExpandThreshold is the minimum sibling-subtree activity to expand the
	// TBN neighborhood.
	TBNExpandThreshold uint64
	// TBNMaxFetchSize caps the TBN neighborhood fetch in bytes (default 2MB).
	TBNMaxFetchSize uint64

	// RegionSize is the 64KB UVM management/migration granularity.
	RegionSize uint64
	// VABlockSize is the 2MB UVM VA block granularity.
	VABlockSize uint64
	// PageSize is the 4KB translation page granularity.
	PageSize uint64

	// GPUCapacityBytes is the hard GPU physical memory capacity enforced by UVM.
	GPUCapacityBytes uint64
}

func (c *UVMConfig) regionsPerBlock() uint64 {
	return c.VABlockSize / c.RegionSize
}

func (c *UVMConfig) pagesPerRegion() uint64 {
	return c.RegionSize / c.PageSize
}

func (c *UVMConfig) pagesPerBlock() uint64 {
	return c.VABlockSize / c.PageSize
}

func (c *UVMConfig) alignDown(addr, granularity uint64) uint64 {
	return (addr / granularity) * granularity
}

// faultHandlingCycles returns the fixed fault latency expressed in GPU cycles.
func (c *UVMConfig) faultHandlingCycles() int {
	if c.Ideal {
		return 0
	}
	cycles := int(c.FaultLatencyUS * 1e-6 * float64(c.GPUCoreFrequency))
	if cycles < 1 {
		cycles = 1
	}
	return cycles
}
