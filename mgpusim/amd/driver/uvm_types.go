package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_uvm
// ManagedAllocationRegistration is the atomic registration record for a
// managed allocation. It owns the VA boundary and the per-base-page mask
// arrays; the VA-block / 64 KB region model (plan todo 4) builds on this
// structure, so only the boundary and mask fields are established here.
//
//	type ManagedAllocationRegistration struct {
//		PID       vm.PID
//		Base      uint64 // first virtual address of the allocation
//		Size      uint64 // requested byte size
//		PageCount uint64 // number of base pages covering Size
//		PageSize  uint64 // base page size (4 KB)
//
//		// Per-base-page bit masks: bit i of word w covers page (w*64 + i).
//		// ResidentMask tracks GPU residency (zero at allocation: no GPU frames);
//		// InFlightMask tracks migrations in flight; DirtyMask tracks dirty state;
//		// ValidMask marks the allocated pages (all ones at allocation).
//		ResidentMask []uint64
//		InFlightMask []uint64
//		DirtyMask    []uint64
//		ValidMask    []uint64
//	}
//
// sbin_codex (todo 4): the registration now also carries the per-page CPU
// backing frames and the VA-block model (uvm-manager.md §5.1) built over the
// authoritative masks.
type ManagedAllocationRegistration struct {
	PID       vm.PID
	Base      uint64 // first virtual address of the allocation
	Size      uint64 // requested byte size
	PageCount uint64 // number of base pages covering Size
	PageSize  uint64 // base page size (4 KB)

	// CPUBackingPages holds the authoritative CPU backing PA for every base
	// page (uvm-manager.md §4.2 "CPU physical page").
	CPUBackingPages []uint64

	// VABlocks is the 2 MB VA-block model built over the masks (plan todo 4).
	VABlocks []*VABlock

	// Per-base-page bit masks: bit i of word w covers page (w*64 + i).
	// ResidentMask tracks GPU residency (zero at allocation: no GPU frames);
	// InFlightMask tracks migrations in flight; DirtyMask tracks dirty state;
	// ValidMask marks the allocated pages (all ones at allocation).
	ResidentMask []uint64
	InFlightMask []uint64
	DirtyMask    []uint64
	ValidMask    []uint64

	// sbin_codex (todo 17): PrefetchedMask marks pages whose GPU residency
	// came from a TBN prefetch (uvm-manager.md §11.11). It is set when a
	// prefetch migration commits and cleared when a later fault resolves the
	// outcome: useful (still resident when demanded) or unused (left the GPU
	// before any demand) (§11.12). It is NOT part of the TBN occupancy mask.
	PrefetchedMask []uint64
}

// basePageSize is the 4 KB UVM base-page granularity (uvm-manager.md §4).
// sbin_uvm
const basePageSize = 4096
