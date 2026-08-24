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
type UVMManager struct {
	sync.Mutex
}

// RegisterManagedAllocation registers the residency metadata for a managed
// allocation produced by the allocator.
func (m *UVMManager) RegisterManagedAllocation(pid vm.PID, res internal.ManagedAllocationResult) {
	m.Lock()
	defer m.Unlock()
}
