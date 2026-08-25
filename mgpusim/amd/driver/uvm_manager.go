package driver

import (
	"container/list"
	"sync" // sbin_codex: serialize the UVM state machine under ParallelEngine.

	"github.com/sarchlab/akita/v4/sim" // sbin_codex
)

// UVMManager owns the functional UVM state machine: managed allocations, 4KB
// residency, 64KB region phases, fault coalescing and serialization, TBN
// selection, capacity accounting, eviction, migration, access counters, and
// statistics.
//
// It is owned by the Driver and driven by scheduled Akita events and by GPU
// control responses, so the fixed fault latency never blocks the engine and no
// UVM operation ever quiesces the GPU.
type UVMManager struct {
	config  UVMConfig    // sbin_codex
	d       *Driver      // sbin_codex
	stateMu sync.RWMutex // sbin_codex: protects all mutable UVM state below.

	allocations map[string]*ManagedAllocation
	pages       map[PageKey]*ManagedPage
	blocks      map[BlockKey]*VABlock
	regions     map[RegionKey]*RegionState

	// sbin_codex: exactly one 64KB fault service transaction is active at a
	// time and the queue is FIFO by creation time (spec 8.4).
	faults          map[RegionKey]*FaultTransaction
	faultsByID      map[string]*FaultTransaction
	faultServiceCue []string
	activeFaultID   string
	// faultsDeferred holds transactions parked behind an in-progress eviction
	// of the same region. They are re-admitted when it finishes. // sbin_codex
	faultsDeferred map[RegionKey][]string
	// retryRegions are regions whose admission found no free GPU frame. They
	// are retried once an eviction releases capacity. // sbin_codex
	retryRegions     []RegionKey
	retryRegionSeen  map[RegionKey]bool
	migrations       map[string]*Migration
	migrationsByPage map[PageKey]string

	// sbin_codex: outstanding region-scoped GPU control operations, keyed by
	// the request ID the GPU echoes back.
	controlOps map[string]*pendingControlOp

	// sbin_codex: MemCopy requests issued for UVM migrations, mapped to their
	// owning migration so the response is not mistaken for a user copy.
	dmaToMigration map[string]string

	// sbin_codex: continuations waiting on an in-flight eviction.
	evictionDone map[string]func()

	accessCounterResetDestination sim.RemotePort                 // sbin_codex
	pendingAccessCounterResets    []pendingAccessCounterReset    // sbin_codex
	pendingAccessCounterResetKeys map[AccessCounterResetKey]bool // sbin_codex

	// sbin_codex: migration-recency LRU of GPU-resident 64KB regions. Per spec
	// 18.1 an ordinary GPU access does not reorder this list.
	lru    *list.List
	lruMap map[RegionKey]*list.Element

	// sbin_codex: messages waiting for the UVM control port.
	sendQueue []sim.Msg

	stats  UVMStats
	nextID uint64

	// sbin_codex: capacity accounting (spec 17.1). A frame counts as in use
	// from the moment an incoming migration reserves it until the eviction
	// that releases it completes, so capacity can never be over-committed.
	// evictingFrames are still occupied but already promised back.
	gpuFramesInUse  uint64
	evictingFrames  uint64
	activeEvictions uint64
	// totalManagedBytes is the managed allocation footprint: the sum of every
	// AllocateManaged request, rounded up to whole pages. It is what the
	// benchmark asked for, not the set of pages it goes on to touch, and it
	// is used to resolve an allocation-relative capacity. // sbin_codex
	totalManagedBytes uint64
}

// freeGPUFrames reports the GPU frame budget still available to UVM.
func (m *UVMManager) freeGPUFrames() uint64 {
	capacity := m.capacityFrames()
	if m.gpuFramesInUse >= capacity {
		return 0
	}

	return capacity - m.gpuFramesInUse
}

func newUVMManager(d *Driver, config UVMConfig) *UVMManager {
	config.PageSize = uint64(1) << config.Log2PageSize
	if config.RegionSize == 0 {
		config.RegionSize = 64 * 1024
	}
	if config.VABlockSize == 0 {
		config.VABlockSize = 2 * 1024 * 1024
	}
	if config.TBNMaxFetchSize == 0 {
		config.TBNMaxFetchSize = config.VABlockSize
	}
	if config.GPUCapacityBytes == 0 {
		// The GPU (device 1) is registered after the manager is constructed.
		// Defer the capacity derivation to the first use.
		config.GPUCapacityBytes = ^uint64(0)
	}

	m := &UVMManager{
		config:                        config,
		d:                             d,
		allocations:                   make(map[string]*ManagedAllocation),
		pages:                         make(map[PageKey]*ManagedPage),
		blocks:                        make(map[BlockKey]*VABlock),
		regions:                       make(map[RegionKey]*RegionState),
		faults:                        make(map[RegionKey]*FaultTransaction),
		faultsDeferred:                make(map[RegionKey][]string),
		faultsByID:                    make(map[string]*FaultTransaction),
		migrations:                    make(map[string]*Migration),
		migrationsByPage:              make(map[PageKey]string),
		controlOps:                    make(map[string]*pendingControlOp),
		dmaToMigration:                make(map[string]string),
		evictionDone:                  make(map[string]func()),
		pendingAccessCounterResetKeys: make(map[AccessCounterResetKey]bool), // sbin_codex
		lru:                           list.New(),
		lruMap:                        make(map[RegionKey]*list.Element),
		nextID:                        1,
	}

	return m
}

func (c *UVMConfig) gpuCapacity(d *Driver) uint64 {
	// GPU device 1 is the first (and, for UVM, the only supported) GPU.
	if len(d.devices) > 1 {
		return d.devices[1].GetStorageSize()
	}

	return 0
}

// resolveCapacity derives the effective GPU capacity, preferring the explicit
// config value and falling back to the registered GPU device size.
func (m *UVMManager) resolveCapacity() uint64 {
	if m.config.GPUCapacityBytes != ^uint64(0) {
		return m.config.GPUCapacityBytes
	}

	capacity := m.config.gpuCapacity(m.d)
	if capacity != 0 {
		m.config.GPUCapacityBytes = capacity
	}

	return capacity
}

// capacityFrames is the hard GPU frame budget UVM may occupy.
func (m *UVMManager) capacityFrames() uint64 {
	return m.resolveCapacity() / m.config.PageSize
}

func (m *UVMManager) newID(prefix string) string {
	m.nextID++

	return prefix + "-" + sim.GetIDGenerator().Generate() + "-" + itoa(m.nextID)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}

	var buf [20]byte

	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}

	return string(buf[i:])
}
