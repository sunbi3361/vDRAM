package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// PageKey identifies a 4KB translation page.
type PageKey struct {
	PID   vm.PID
	VAddr uint64
}

// FaultKey identifies a unique outstanding page fault. DeviceID is the GPU
// that requested the page.
type FaultKey struct {
	Page     PageKey
	DeviceID uint64
}

// RegionKey identifies a 64KB UVM region.
type RegionKey struct {
	PID      vm.PID
	Base     uint64
	DeviceID uint64
}

// BlockKey identifies a 2MB UVM VA block.
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

// ResidencyState is the residency of a managed page.
type ResidencyState int

const (
	CPUResident ResidencyState = iota
	GPUResident
	MigratingToGPU
	MigratingToCPU
)

// ManagedPage tracks UVM state for one 4KB managed page.
type ManagedPage struct {
	Key             PageKey
	AllocationID    string
	CPUBackingPAddr uint64
	GPUFramePAddr   uint64
	GPUFrameValid   bool
	State           ResidencyState
	RegionBase      uint64
	VABlockBase     uint64
	TimesMigrated   uint64
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

// FaultWaiter records a GPU translation request waiting on a page fault.
type FaultWaiter struct {
	RequestID string
	ReplyTo   sim.RemotePort
	DeviceID  uint64
	PID       vm.PID
	VAddr     uint64
}

// FaultState tracks the life of a coalesced page fault.
type FaultState int

const (
	FaultPending FaultState = iota
	FaultReady
	FaultMigrating
	FaultComplete
)

// PageFault is a coalesced demand page fault for one 4KB page.
type PageFault struct {
	ID            string
	Key           FaultKey
	RegionBase    uint64
	VABlockBase   uint64
	CreatedAt     sim.VTimeInSec
	ReadyAt       sim.VTimeInSec
	State         FaultState
	Waiters       []FaultWaiter
	DemandPages   uint64
	PrefetchPages uint64
	MigrationID   string
}

// MigrationTrigger identifies what started a migration.
type MigrationTrigger int

const (
	TriggerFault MigrationTrigger = iota
	TriggerAccessCounter
)

// MigrationDirection identifies the transfer direction.
type MigrationDirection int

const (
	CPUToGPU MigrationDirection = iota
	GPUToCPU
)

// Migration tracks a batched page transfer.
type Migration struct {
	ID             string
	Direction      MigrationDirection
	Trigger        MigrationTrigger
	DeviceID       uint64
	Pages          []PageKey
	Bytes          uint64
	DemandPages    uint64
	PrefetchPages  uint64
	CreatedAt      sim.VTimeInSec
	DataStartedAt  sim.VTimeInSec
	DataFinishedAt sim.VTimeInSec
	FaultIDs       []string
}

// RegionState tracks the 64KB UVM region for residency and LRU.
type RegionState struct {
	Key            RegionKey
	Pages          []PageKey
	LastAccess     sim.VTimeInSec
	AccessEpoch    uint64
	ActiveFaults   uint32
	MigrationID    string
	EvictionLocked bool
	Generation     uint64
}

// VABlock tracks a 2MB UVM VA block and its 64KB regions.
type VABlock struct {
	Key          BlockKey
	AllocationID string
	Regions      []*RegionState
	Activity     []uint32
}

// AccessCounterState is the 64KB remote-access counter.
type AccessCounterState struct {
	Count        uint64
	Epoch        uint64
	Notification bool
	LastAccess   sim.VTimeInSec
}
