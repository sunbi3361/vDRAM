package driver

import (
	"github.com/sarchlab/akita/v4/sim"
)

// UVMStats aggregates the counters required by spec 27. Every counter has the
// same definition in normal and ideal mode; only elapsed time differs.
type UVMStats struct {
	Enabled bool
	Ideal   bool

	// Faults.
	RawPageFaults       uint64
	UniqueFaultServices uint64
	CoalescedFaults     uint64

	// TBN.
	TBNFaultEvents           uint64
	TBNSelections            [6]uint64
	TBNSelectedBytes         uint64
	TBNDemandBytes           uint64
	TBNPrefetchCandidateByte uint64
	TBNActualPrefetchBytes   uint64
	TBNSuppressedResident    uint64
	TBNSuppressedInflight    uint64

	// Migration.
	CPUToGPUMigrations         uint64
	BytesCPUToGPU              uint64
	GPUToCPUMigrations         uint64
	BytesGPUToCPU              uint64
	DemandMigrations           uint64
	AccessCounterMigrations    uint64
	BytesAccessCounterMigrated uint64
	WriteTriggeredMigrations   uint64
	MigratedPages              uint64
	MigratedBytes              uint64
	RepeatedMigrations         uint64

	// Eviction.
	Evictions    uint64
	EvictedPages uint64
	EvictedBytes uint64

	// Pre-eviction (spec 17.1).
	PreEvictions              uint64
	PreEvictedBytes           uint64
	MaxConcurrentPreEvictions uint64
	PreEvictionsOverlappedH2D uint64
	MigrationWaitForCapacity  sim.VTimeInSec

	// Residency.
	GPUResidentPages     uint64
	GPUResidentBytes     uint64
	GPUResidentPagesPeak uint64
	GPUResidentBytesPeak uint64

	// Remote access and access counter.
	RemoteAccesses      uint64
	AccessCounterNotify uint64
	// AccessCounterServices counts the notifications that opened a service
	// transaction, i.e. the ones charged the fixed software latency. // sbin_codex
	AccessCounterServices   uint64
	AccessCounterResets     uint64
	AccessCounterSuppressed uint64

	// Mapping control.
	// LazyRemoteMaps counts the INVALID -> REMOTE region installs performed on
	// a first-access fault under LazyRemotePTE. It is a mapping publication,
	// not a residency service, so it is counted apart from
	// UniqueFaultServices and never charged a migration. // sbin_claude_uvm
	LazyRemoteMaps        uint64
	RemotePTEInstalls     uint64
	LocalPTEInstalls      uint64
	TLBRangeInvalidations uint64
	CacheRangeFlushes     uint64
	FaultReplays          uint64
	RefusedMigrations     uint64
	RemoteDrains          uint64

	// Timing.
	FaultHandlingTime sim.VTimeInSec
	MigrationTime     sim.VTimeInSec
	EvictionTime      sim.VTimeInSec
}

func (m *UVMManager) updateResidencyPeak() {
	if m.stats.GPUResidentPages > m.stats.GPUResidentPagesPeak {
		m.stats.GPUResidentPagesPeak = m.stats.GPUResidentPages
		m.stats.GPUResidentBytesPeak =
			m.stats.GPUResidentPagesPeak * m.config.PageSize
	}
}
