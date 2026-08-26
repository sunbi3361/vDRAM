package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// PageKey identifies a 4KB base page. Page faults, PTEs, and residency are all
// tracked at this granularity even though fault service, prefetching, access
// counting, and eviction operate on 64KB regions.
type PageKey struct {
	PID   vm.PID
	VAddr uint64
}

// RegionKey identifies a 64KB UVM region on one GPU. It is the key of a fault
// service transaction, of the access counter, and of the eviction LRU.
type RegionKey struct {
	PID      vm.PID
	Base     uint64
	DeviceID uint64
}

// BlockKey identifies a 2MB VA block, the largest region TBN may select.
type BlockKey struct {
	PID  vm.PID
	Base uint64
}

// AccessCounterKey identifies a 64KB remote-access counter.
type AccessCounterKey struct {
	PID        vm.PID
	RegionBase uint64
	DeviceID   uint64
}

// AccessCounterResetKey identifies one counter reset target. // sbin_codex
type AccessCounterResetKey struct {
	PID        vm.PID
	RegionBase uint64
}

type pendingAccessCounterReset struct { // sbin_codex
	Key      AccessCounterResetKey
	DeviceID uint64
}

// ResidencyState is the residency of a single 4KB managed page.
type ResidencyState int

const (
	// CPUResident means the authoritative copy lives in host memory. The GPU
	// PTE is INVALID or REMOTE depending on the access-counter mode.
	CPUResident ResidencyState = iota
	// GPUResident means a GPU frame holds the page and the PTE is GPU_LOCAL.
	GPUResident
	// MigratingToGPU covers the H2D transfer window.
	MigratingToGPU
	// MigratingToCPU covers the eviction window.
	MigratingToCPU
)

// ManagedPage tracks UVM state for one 4KB managed page.
type ManagedPage struct {
	Key             PageKey
	AllocationID    string
	CPUBackingPAddr uint64
	GPUFramePAddr   uint64
	GPUFrameValid   bool
	// RemoteMapped records that the cold page completed its first-touch remote
	// mapping, so its GPU PTE is REMOTE rather than INVALID. // sbin_codex
	RemoteMapped  bool
	State         ResidencyState
	RegionBase    uint64
	VABlockBase   uint64
	TimesMigrated uint64
}

// ManagedAllocation tracks a contiguous managed virtual allocation.
type ManagedAllocation struct {
	ID        string
	PID       vm.PID
	Base      uint64
	Size      uint64
	PageBase  uint64
	PageCount uint64
}

// RegionPhase is the UVM state machine phase of a 64KB region. A region stays
// coalescible for new faults through every phase up to the point where it
// becomes replayable. // sbin_codex
type RegionPhase int

const (
	// RegionIdle means no UVM transaction owns the region.
	RegionIdle RegionPhase = iota
	// RegionFaultPending means a fault transaction exists but the fixed
	// software latency has not elapsed.
	RegionFaultPending
	// RegionFaultHandling means the driver is selecting and reserving.
	RegionFaultHandling
	// RegionMigratingToGPU covers the H2D DMA window.
	RegionMigratingToGPU
	// RegionInvalidating covers the PTE update and range TLB invalidation.
	RegionInvalidating
	// RegionEvicting covers cache flush, TLB invalidation, and the D2H DMA.
	RegionEvicting
)

// RegionState tracks one 64KB UVM region.
type RegionState struct {
	Key   RegionKey
	Pages []PageKey
	Phase RegionPhase

	// LastMigrationTime drives the eviction LRU. Per spec 18.1 it is updated
	// only on migration/admission, never on an ordinary GPU access, so the
	// policy is a migration-recency approximation of LRU. // sbin_codex
	LastMigrationTime sim.VTimeInSec

	MigrationID string
	FaultID     string
	Generation  uint64
}

// busy reports whether a UVM transaction currently owns the region.
func (r *RegionState) busy() bool {
	return r.Phase != RegionIdle
}

// VABlock tracks a 2MB VA block and its 32 constituent 64KB regions.
type VABlock struct {
	Key          BlockKey
	AllocationID string
	Regions      []*RegionState
}

// FaultState is the life cycle of a 64KB fault service transaction.
type FaultState int

const (
	// FaultPending waits out the fixed software fault-handling latency.
	FaultPending FaultState = iota
	// FaultHandling runs TBN selection, capacity checks, and reservation.
	FaultHandling
	// FaultMigrating waits for the H2D DMA.
	FaultMigrating
	// FaultInvalidating waits for the 64KB TLB range invalidation.
	FaultInvalidating
	// FaultComplete has been replayed and is no longer coalescible.
	FaultComplete
)

// FaultTransaction is one unique 64KB fault service. Every 4KB fault that
// targets the same region while this transaction is outstanding joins it and
// does not incur another fixed software delay (spec 8.3, 10.1). // sbin_codex
type FaultTransaction struct {
	ID          string
	Key         RegionKey
	VABlockBase uint64

	// Trigger says what asked for the service. Both kinds occupy the single
	// service slot and are charged the fixed software latency (spec 8.4,
	// 10.1); they differ in what they select. A demand fault runs TBN, an
	// access-counter service migrates its own 64KB region (spec 16).
	// sbin_codex
	Trigger MigrationTrigger

	CreatedAt sim.VTimeInSec
	ReadyAt   sim.VTimeInSec
	State     FaultState

	// RawFaults counts every 4KB fault request folded into this transaction.
	RawFaults uint64
	// IsWrite records that at least one folded fault came from a write.
	IsWrite bool
	// demandVAddrs is the exact 4KB demand-fault mask. TBN expands the fault
	// to its 64KB leaf for the occupancy tree only; the pages counted as
	// prefetched must exclude these (spec 11.7). // sbin_codex
	demandVAddrs map[uint64]bool

	Selected      []PageKey
	DemandPages   uint64
	PrefetchPages uint64

	MigrationID        string
	NeedsTLBInvalidate bool
}

// MigrationTrigger identifies what started a migration.
type MigrationTrigger int

const (
	// TriggerFault is a demand fault, possibly widened by TBN.
	TriggerFault MigrationTrigger = iota
	// TriggerAccessCounter is a remote-access threshold crossing.
	TriggerAccessCounter
	// TriggerEviction is a capacity-driven GPU to CPU transfer.
	TriggerEviction
)

// MigrationDirection identifies the transfer direction.
type MigrationDirection int

const (
	// CPUToGPU is an H2D admission.
	CPUToGPU MigrationDirection = iota
	// GPUToCPU is a D2H eviction.
	GPUToCPU
)

// Migration tracks one batched page transfer. It may be split into several DMA
// requests, one per maximal run of contiguous source and destination physical
// addresses (spec 23.1.2). // sbin_codex
type Migration struct {
	ID        string
	Direction MigrationDirection
	Trigger   MigrationTrigger
	DeviceID  uint64
	PID       vm.PID

	Pages         []PageKey
	Bytes         uint64
	DemandPages   uint64
	PrefetchPages uint64

	CreatedAt      sim.VTimeInSec
	DataStartedAt  sim.VTimeInSec
	DataFinishedAt sim.VTimeInSec

	FaultIDs   []string
	RegionKeys []RegionKey

	// PendingDMA counts the outstanding MemCopy requests of this migration.
	PendingDMA int
	// NeedsTLBInvalidate is set when at least one page was REMOTE-mapped, so
	// a stale translation may be cached in the L2 TLB (spec 21.3).
	NeedsTLBInvalidate bool
}

// controlOpKind identifies an outstanding GPU control operation.
type controlOpKind int

const (
	opTLBInvalidate controlOpKind = iota
	opCacheRangeFlush
	opRemoteDrain
)

// pendingControlOp resumes a UVM sequence once the GPU acknowledges a
// region-scoped control operation. // sbin_codex
type pendingControlOp struct {
	Kind controlOpKind
	Done func()
}
