package driver

import (
	"fmt"
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

	// sbin_codex: registered managed allocations; a registration is appended
	// only after its boundaries and masks are fully built (atomic visibility).
	registrations []*ManagedAllocationRegistration
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

// RegisterManagedAllocation validates an allocator result and atomically
// records the managed allocation's boundaries and per-page masks. The record
// is complete before it becomes visible; on validation failure no state is
// mutated and the caller must roll the allocation back.
func (m *UVMManager) RegisterManagedAllocation(
	pid vm.PID,
	res internal.ManagedAllocationResult,
) error {
	m.Lock()
	defer m.Unlock()

	reg, err := newManagedAllocationRegistration(pid, res)
	if err != nil {
		return err
	}

	m.registrations = append(m.registrations, reg)
	return nil
}

// RegistrationCount returns the number of registered managed allocations.
func (m *UVMManager) RegistrationCount() int {
	m.Lock()
	defer m.Unlock()

	return len(m.registrations)
}

// newManagedAllocationRegistration validates an allocator result and builds
// the complete boundary + mask record. Any validation failure returns an
// error without producing a record, so a failed registration never becomes
// visible. sbin_codex
func newManagedAllocationRegistration(
	pid vm.PID,
	res internal.ManagedAllocationResult,
) (*ManagedAllocationRegistration, error) {
	if res.Size == 0 {
		return nil, fmt.Errorf("uvm: managed allocation size is 0")
	}
	if res.PageSize != basePageSize {
		return nil, fmt.Errorf(
			"uvm: managed allocation page size %d != base page %d",
			res.PageSize, basePageSize)
	}
	if want := (res.Size - 1) / res.PageSize + 1; res.PageCount != want {
		return nil, fmt.Errorf(
			"uvm: managed allocation page count %d != %d",
			res.PageCount, want)
	}
	if res.PageCount != uint64(len(res.CPUBackingPages)) {
		return nil, fmt.Errorf(
			"uvm: managed allocation has %d CPU backing pages for %d pages",
			len(res.CPUBackingPages), res.PageCount)
	}
	if res.Base == 0 {
		return nil, fmt.Errorf("uvm: managed allocation base address is 0")
	}

	numWords := (res.PageCount + 63) / 64
	reg := &ManagedAllocationRegistration{
		PID:           pid,
		Base:          res.Base,
		Size:          res.Size,
		PageCount:     res.PageCount,
		PageSize:      res.PageSize,
		ResidentMask:  make([]uint64, numWords),
		InFlightMask:  make([]uint64, numWords),
		DirtyMask:     make([]uint64, numWords),
		ValidMask:     make([]uint64, numWords),
	}
	for w := uint64(0); w < numWords; w++ {
		bits := res.PageCount - w*64
		if bits > 64 {
			bits = 64
		}
		reg.ValidMask[w] = (uint64(1) << bits) - 1
	}
	return reg, nil
}
