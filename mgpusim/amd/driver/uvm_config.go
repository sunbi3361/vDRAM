package driver

import (
	"github.com/sarchlab/akita/v4/sim"
)

// UVMConfig carries the tunable UVM parameters. Values are supplied through the
// runner flags and forwarded by the driver builder.
type UVMConfig struct {
	// Enabled enables UVM demand-paged managed memory.
	Enabled bool
	// Ideal keeps the whole functional state machine but zeroes the fixed
	// fault-handling latency and the migration transfer latency.
	Ideal bool

	// Log2PageSize is the base 4KB translation page size (log2).
	Log2PageSize uint64
	// GPUCoreFrequency converts the fixed fault latency into cycles.
	GPUCoreFrequency sim.Freq

	// FaultLatencyUS is the fixed host/driver page-fault handling latency in
	// microseconds, charged once per unique 64KB fault-service transaction.
	FaultLatencyUS float64

	// AccessCounterEnabled makes a cold managed page remotely accessible
	// instead of invalid, so the first GPU read is a counted remote access
	// rather than a demand fault (spec 7.1, 12.1). // sbin_codex
	AccessCounterEnabled bool
	// AccessCounterThreshold is the number of remote accesses to one 64KB
	// region that triggers a CPU->GPU migration.
	AccessCounterThreshold uint64

	// TBNExpandRatio is the occupancy percentage an ancestor node must exceed
	// for TBN to expand into it. The comparison is strictly greater than.
	TBNExpandRatio float64
	// TBNMaxFetchSize caps the TBN neighborhood fetch in bytes (default 2MB).
	TBNMaxFetchSize uint64
	// PrefetchDisabled restricts every fault service to its 64KB leaf.
	PrefetchDisabled bool
	// EvictionDisabled turns off capacity-driven eviction.
	EvictionDisabled bool

	// RegionSize is the 64KB UVM fault-service, counting, and eviction unit.
	RegionSize uint64
	// VABlockSize is the 2MB UVM VA block granularity.
	VABlockSize uint64
	// PageSize is the 4KB translation page granularity.
	PageSize uint64

	// GPUCapacityBytes is the hard GPU capacity UVM managed memory may occupy.
	GPUCapacityBytes uint64

	// OversubscriptionRatio expresses the capacity relative to the managed
	// allocation footprint rather than to the GPU's physical memory (spec 20):
	//
	//	Oversubscription Ratio =
	//	    total AllocateManaged bytes / UVM GPU Capacity
	//
	// The numerator is what the benchmark allocated, not the pages it goes on
	// to touch. A ratio of 1.5 therefore gives every benchmark exactly 150%
	// oversubscription regardless of its own footprint. It is resolved as the
	// managed allocations are registered, and overrides GPUCapacityBytes.
	// Zero disables it. // sbin_codex
	OversubscriptionRatio float64
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
	return addr / granularity * granularity
}

// faultHandlingCycles returns the fixed fault latency expressed in GPU cycles.
// Ideal mode returns zero so the transaction advances to the next functional
// stage immediately (spec 10.4).
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
