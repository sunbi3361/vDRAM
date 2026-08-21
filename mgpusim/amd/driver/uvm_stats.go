package driver

import (
	"github.com/sarchlab/akita/v4/sim"
)

// UVMStats aggregates all UVM counters required by the specification.
type UVMStats struct {
	Enabled bool
	Ideal   bool

	PageFaultRequests  uint64
	UniquePageFaults   uint64
	CoalescedFaultReqs uint64

	TBNFetches       uint64
	TBN64KBFetches   uint64
	TBNLargerFetches uint64
	DemandMigPages   uint64
	PrefetchPages    uint64

	CPUToGPUMigrations uint64
	GPUToCPUMigrations uint64
	MigratedPages      uint64
	MigratedBytes      uint64

	Evictions            uint64
	EvictedPages         uint64
	EvictedBytes         uint64
	RepeatedMigrations   uint64
	GPUResidentPages     uint64
	GPUResidentBytes     uint64
	GPUResidentPagesPeak uint64
	GPUResidentBytesPeak uint64

	RemoteAccesses      uint64
	AccessCounterNotif  uint64
	AccessCounterMigr   uint64
	AccessCounterResets uint64

	FaultHandlingTime sim.VTimeInSec
	MigrationTime     sim.VTimeInSec
	EvictionTime      sim.VTimeInSec

	pageFaultReplies uint64
}

func (m *UVMManager) updateResidencyPeak() {
	if m.stats.GPUResidentPages > m.stats.GPUResidentPagesPeak {
		m.stats.GPUResidentPagesPeak = m.stats.GPUResidentPages
	}
	if m.stats.GPUResidentBytes > m.stats.GPUResidentBytesPeak {
		m.stats.GPUResidentBytesPeak = m.stats.GPUResidentBytes
	}
}
