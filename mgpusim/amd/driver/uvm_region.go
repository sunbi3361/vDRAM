package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: per-64 KB region (sub-block) state (uvm-manager.md §5.3, §23,
// §31). The region is the TBN minimum node, the access-counter accounting
// region, and the eviction unit.

// RegionState is the per-64 KB-region transaction state (uvm-manager.md §23).
type RegionState int

const (
	RegionIDLE RegionState = iota
	RegionFaultPending
	RegionMigratingToGPU
	RegionGPUResident
	RegionEvictPending
	RegionMigratingToCPU
	RegionCPUResident
)

// String returns the symbolic region-state name.
func (s RegionState) String() string {
	switch s {
	case RegionIDLE:
		return "IDLE"
	case RegionFaultPending:
		return "FAULT_PENDING"
	case RegionMigratingToGPU:
		return "MIGRATING_TO_GPU"
	case RegionGPUResident:
		return "GPU_RESIDENT"
	case RegionEvictPending:
		return "EVICT_PENDING"
	case RegionMigratingToCPU:
		return "MIGRATING_TO_CPU"
	case RegionCPUResident:
		return "CPU_RESIDENT"
	default:
		return "UNKNOWN"
	}
}

// SubBlockState is the per-64 KB region state (uvm-manager.md §5.3). The
// transaction State is authoritative for the region; per-page residency stays
// in the registration masks.
type SubBlockState struct {
	VA    uint64 // 64 KB-aligned region base VA
	State RegionState

	// Per-64 KB region access counter (uvm-manager.md §31.1), reset on kernel
	// launch (the reset mechanism is plan todo 11; this todo owns the state).
	AccessCounter uint64

	// Migration-recency timestamp (uvm-manager.md §31.2), updated ONLY on
	// migration/admission, never on residency hits.
	LastMigrationTime sim.VTimeInSec
}

// NewSubBlockState builds an idle region at the 64 KB-aligned base va.
func NewSubBlockState(va uint64) *SubBlockState {
	return &SubBlockState{VA: va, State: RegionIDLE}
}

// ResetAccessCounter zeroes the per-region access counter (kernel launch, §31.1).
func (s *SubBlockState) ResetAccessCounter() { s.AccessCounter = 0 }

// RecordRemoteAccess increments the access counter for a CPU-remote GPU access
// (§31.1). It is only legal while the region is not GPU-resident; a
// GPU-resident region's accesses are local and must not touch the remote
// counter (§28 access-counter invariant).
func (s *SubBlockState) RecordRemoteAccess() error {
	if s.State == RegionGPUResident {
		return fmt.Errorf(
			"uvm: access counter incremented for a non-remote (GPU-resident) access")
	}
	s.AccessCounter++
	return nil
}

// RecordMigration updates the migration-recency timestamp (§31.2). Callers
// must invoke it ONLY on migration/admission, never on residency hits.
func (s *SubBlockState) RecordMigration(now sim.VTimeInSec) {
	s.LastMigrationTime = now
}
