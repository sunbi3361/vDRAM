package driver

import (
	"container/list"
)

// UVMManager owns the functional UVM demand-paging state machine: residency,
// faults, coalescing, TBN selection, capacity enforcement, eviction,
// migration, access counters, and statistics. It is owned by the Driver and
// driven by scheduled Akita events so that the fixed fault latency never
// blocks the simulation engine.
type UVMManager struct {
	config UVMConfig
	d      *Driver

	allocations map[string]*ManagedAllocation
	pages       map[PageKey]*ManagedPage
	blocks      map[BlockKey]*VABlock
	regions     map[RegionKey]*RegionState

	faults       map[FaultKey]*PageFault
	faultsByID   map[string]*PageFault
	migrations   map[string]*Migration
	pageToMig    map[PageKey]string
	accessCounts map[AccessCounterKey]*AccessCounterState

	// sbin_codex: driver-side LRU list of GPU-resident 64KB regions for
	// eviction victim selection. The access counter is independent of it.
	lru    *list.List
	lruMap map[RegionKey]*list.Element

	// sbin_codex: pending TLB-shootdown eviction. Victim regions are reserved,
	// a ShootDownCommand flushes the GPU TLB, and only after the ACK are the
	// PTEs/frames finalized so stale translations can never be used.
	evicting    []*RegionState
	evictACK    uint64
	evictOnDone func()

	stats  UVMStats
	nextID uint64

	freeGPUFrames uint64
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
		config:       config,
		d:            d,
		allocations:  make(map[string]*ManagedAllocation),
		pages:        make(map[PageKey]*ManagedPage),
		blocks:       make(map[BlockKey]*VABlock),
		regions:      make(map[RegionKey]*RegionState),
		faults:       make(map[FaultKey]*PageFault),
		faultsByID:   make(map[string]*PageFault),
		migrations:   make(map[string]*Migration),
		pageToMig:    make(map[PageKey]string),
		accessCounts: make(map[AccessCounterKey]*AccessCounterState),
		lru:          list.New(),
		lruMap:       make(map[RegionKey]*list.Element),
		nextID:       1,
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
	cap := m.config.gpuCapacity(m.d)
	if cap != 0 {
		m.config.GPUCapacityBytes = cap
	}
	return cap
}
