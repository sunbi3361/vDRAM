package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_codex: §28 invariant assertions and statistics-ownership documentation.
// Each check returns a descriptive error naming the violated invariant and the
// PID/GPU/block/region context; nil means the invariant holds. The
// access-counter and write invariants are enforced at the API level
// (SubBlockState.RecordRemoteAccess, RegionStateMachine.GPUWrite).

// InvariantContext binds the authoritative masks, a VA block, one of its
// regions, and the capacity reservation for a set of invariant checks.
type InvariantContext struct {
	PID         vm.PID
	GPU         int
	Block       *VABlock
	BlockIdx    uint64
	Region      *SubBlockState
	RegionIdx   uint64
	Reg         *ManagedAllocationRegistration
	Reservation *AdmissionReservation
}

// regionPageRange returns the first allocation page index and the count of
// valid (allocated) pages in the region, computed from VA ranges so a
// misaligned allocation (base not 64 KB-aligned) maps correctly.
func (c *InvariantContext) regionPageRange() (allocStart, valid uint64) {
	regionVA := c.Block.StartVA + c.RegionIdx*subblockSizeBytes
	allocEndVA := c.Reg.Base + c.Reg.PageCount*basePageSize
	lo := regionVA
	if lo < c.Reg.Base {
		lo = c.Reg.Base
	}
	hi := regionVA + subblockSizeBytes
	if hi > allocEndVA {
		hi = allocEndVA
	}
	if lo >= hi {
		return 0, 0
	}
	return (lo - c.Reg.Base) / basePageSize, (hi - lo) / basePageSize
}

// blockLocalPage returns the block-local page index (0..511) for an allocation
// page, derived from VA so misaligned allocations map correctly.
func (c *InvariantContext) blockLocalPage(allocPage uint64) uint64 {
	return (c.Reg.Base + allocPage*basePageSize - c.Block.StartVA) / basePageSize
}

// maskBit reports whether bit `page` of mask is set.
func maskBit(mask []uint64, page uint64) bool {
	return mask[page/64]&(uint64(1)<<(page%64)) != 0
}

// CheckResidencyAuthority verifies §28 "Residency": the region's transaction
// state must agree with its pages' authoritative GPU residency (the mask) — a
// region must not simultaneously hold two authoritative residences. Migrating
// states are explicitly modeled and exempt.
func (c *InvariantContext) CheckResidencyAuthority() error {
	allocStart, valid := c.regionPageRange()
	resident := uint64(0)
	for i := uint64(0); i < valid; i++ {
		if maskBit(c.Reg.ResidentMask, allocStart+i) {
			resident++
		}
	}
	switch c.Region.State {
	case RegionGPUResident:
		if resident != valid {
			return fmt.Errorf(
				"uvm: invariant residency: region pid=%d gpu=%d block=%d region=%d is GPU_RESIDENT with %d/%d resident pages",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, resident, valid)
		}
	case RegionCPUResident, RegionIDLE:
		if resident != 0 {
			return fmt.Errorf(
				"uvm: invariant residency: region pid=%d gpu=%d block=%d region=%d is %s with %d resident pages",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, c.Region.State, resident)
		}
	}
	return nil
}

// CheckGPUPhysicalAllocation verifies §28 "GPU Physical Allocation":
// GPU_RESIDENT => valid GPU physical page exists.
func (c *InvariantContext) CheckGPUPhysicalAllocation() error {
	allocStart, valid := c.regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		if maskBit(c.Reg.ResidentMask, page) &&
			c.Block.Pages[c.blockLocalPage(page)].GPUPhysicalPage == 0 {
			return fmt.Errorf(
				"uvm: invariant gpu-pa: region pid=%d gpu=%d block=%d region=%d page %d resident with no GPU physical page",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, page)
		}
	}
	return nil
}

// CheckRemoteMapping verifies §28 "Remote Mapping": REMOTE mapping => CPU
// backing page exists.
func (c *InvariantContext) CheckRemoteMapping() error {
	allocStart, valid := c.regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := &c.Block.Pages[c.blockLocalPage(page)]
		if p.RemoteMapped && p.CPUPhysicalPage == 0 {
			return fmt.Errorf(
				"uvm: invariant remote: region pid=%d gpu=%d block=%d region=%d page %d remote-mapped with no CPU backing",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, page)
		}
	}
	return nil
}

// CheckRemoteCacheability verifies §28 "Remote Cacheability": CPU_REMOTE data
// must never be inserted into GPU data caches. A page is CPU_REMOTE when it is
// remote-mapped and not GPU-resident; such pages must not be CachedOnGPU.
func (c *InvariantContext) CheckRemoteCacheability() error {
	allocStart, valid := c.regionPageRange()
	for i := uint64(0); i < valid; i++ {
		page := allocStart + i
		p := &c.Block.Pages[c.blockLocalPage(page)]
		remote := p.RemoteMapped && !maskBit(c.Reg.ResidentMask, page)
		if remote && p.CachedOnGPU {
			return fmt.Errorf(
				"uvm: invariant cacheability: region pid=%d gpu=%d block=%d region=%d page %d CPU_REMOTE cached on GPU",
				c.PID, c.GPU, c.BlockIdx, c.RegionIdx, page)
		}
	}
	return nil
}

// CheckOversubscription verifies §28 "Oversubscription": R+I+N <= C. In-flight
// bytes are the explicitly modeled transient of an atomic migration/eviction
// transaction and remain bounded by the reservation.
func (c *InvariantContext) CheckOversubscription() error {
	r, i, n := c.Reservation.ResidentBytes(),
		c.Reservation.InFlightBytes(), c.Reservation.ReservedBytes()
	if r+i+n > c.Reservation.CapacityBytes() {
		return fmt.Errorf(
			"uvm: invariant oversubscription: pid=%d gpu=%d R+I+N=%d exceeds capacity %d",
			c.PID, c.GPU, r+i+n, c.Reservation.CapacityBytes())
	}
	return nil
}

// CheckAll runs every §28 invariant check and returns the first violation.
func (c *InvariantContext) CheckAll() error {
	for _, check := range []func() error{
		c.CheckResidencyAuthority,
		c.CheckGPUPhysicalAllocation,
		c.CheckRemoteMapping,
		c.CheckRemoteCacheability,
		c.CheckOversubscription,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// StatisticOwner documents the single update point for one UVM statistic.
// Full statistics exposure is plan todo 22; this todo establishes the
// ownership structure so every counter has exactly one owner.
type StatisticOwner struct {
	Statistic string
	Owner     string
}

// StatisticOwnership is the authoritative owner table. Each statistic is
// updated by exactly one function; the invariant test asserts uniqueness.
var StatisticOwnership = []StatisticOwner{
	{Statistic: "resident_bytes_gpu", Owner: "AdmissionReservation.CommitAdmission / CompleteMigrationToGPU"},
	{Statistic: "in_flight_bytes", Owner: "AdmissionReservation.StartMigration"},
	{Statistic: "reserved_bytes", Owner: "AdmissionReservation.ReserveAdmission"},
	{Statistic: "access_counter", Owner: "SubBlockState.RecordRemoteAccess"},
	{Statistic: "migration_recency", Owner: "SubBlockState.RecordMigration"},
}
