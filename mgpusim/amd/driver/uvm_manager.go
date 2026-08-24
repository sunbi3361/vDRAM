package driver

import (
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
)

// UVMManager owns the functional UVM demand-paging state machine: residency,
// faults, coalescing, TBN selection, capacity enforcement, eviction,
// migration, access counters, and statistics. It is owned by the Driver and
// driven by scheduled Akita events so that the fixed fault latency never
// blocks the simulation engine.
//
// sbin_codex: the manager now carries the validated UVMConfig and the
// resolved GPU capacity so later todos (TBN, capacity enforcement, eviction)
// can consult them without re-deriving configuration.
// type UVMManager struct {
//
//	sync.Mutex
// }
type UVMManager struct {
	sync.Mutex

	config   UVMConfig // sbin_codex: validated UVM configuration.
	capacity uint64    // sbin_codex: resolved UVM GPU capacity in bytes.
}

// NewUVMManager constructs a UVM manager for an enabled UVM configuration.
// availableGPUMemory is the total GPU DRAM the allocator can back; the
// resolved capacity is the explicit -uvm-gpu-memory-capacity when set,
// otherwise the full available GPU memory. sbin_codex
func NewUVMManager(cfg UVMConfig, availableGPUMemory uint64) *UVMManager {
	return &UVMManager{
		config:   cfg,
		capacity: cfg.ResolvedCapacity(availableGPUMemory),
	}
}

// RegisterManagedAllocation registers the residency metadata for a managed
// allocation produced by the allocator.
func (m *UVMManager) RegisterManagedAllocation(pid vm.PID, res internal.ManagedAllocationResult) {
	m.Lock()
	defer m.Unlock()
}
