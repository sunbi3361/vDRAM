package driver

import (
	"fmt"
	"math/bits"

	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: NVIDIA-style hierarchical TBN selection over exact occupancy
// masks (plan todo 17 of mgpusim-uvm-manager, uvm-manager.md §11). The
// selector is a pure function over the registration's authoritative masks:
// the mandatory 64 KB leaf of the fault (all valid pages of the aligned
// 64 KB node become occupied, §11.2), strict >51% ancestor expansion through
// 64 KB -> 128 KB -> 256 KB -> 512 KB -> 1 MB -> 2 MB (§11.4), occupancy =
// GPUResidentMask OR CurrentFaultExpanded64KBMask (§11.3, §11.13), and
// valid-allocation clipping of every candidate (§11.10). Prefetch-in-flight
// and migrating-to-GPU pages are NOT occupancy; they only suppress duplicate
// migration pages when the actual prefetch set is formed (§11.8). The fault
// service (todo 15) calls recomputeTBN, whose migration set flows into
// prepareFaultMigration (todo 16) for maximal-run formation.

// tbnMaxLevel is the 2 MB VA-block root of the candidate hierarchy.
const tbnMaxLevel = 5

// tbnNodeSize returns the byte size of the TBN node at level (0 = 64 KB leaf,
// 5 = 2 MB VA block).
func tbnNodeSize(level int) uint64 { return subblockSizeBytes << level }

// tbnNodePages returns the number of 4 KB pages of the TBN node at level.
func tbnNodePages(level int) uint64 { return pagesPerSubBlock << level }

// tbnNodeBaseVA returns the size-aligned base VA of the node containing
// faultVA at level. Every node is aligned to its own size, so a node never
// crosses a 2 MB VA-block boundary (§11.10).
func tbnNodeBaseVA(faultVA uint64, level int) uint64 {
	return faultVA &^ (tbnNodeSize(level) - 1)
}

// tbnSelection is the outcome of one TBN selection (uvm-manager.md §11).
type tbnSelection struct {
	// RegionBase is the VA base of the selected TBN region (64 KB aligned).
	RegionBase uint64
	// RegionSize is the selected region size: 64 KB .. 2 MB.
	RegionSize uint64
	// Level is the selected level: 0 = 64 KB leaf .. 5 = 2 MB.
	Level int

	// DemandPages are the valid 4 KB pages of the mandatory 64 KB fault leaf
	// (allocation page indices): the demand set of the fault transaction
	// (§11.7). The exact demand mask stays 4 KB-granular even though the
	// occupancy tree expands the fault to the full leaf.
	DemandPages []uint64
	// MissingDemand are the demand pages that still require migration
	// (neither GPU-resident nor in flight).
	MissingDemand []uint64
	// PrefetchPages are the ACTUAL prefetch migration pages: the selected
	// region's valid pages minus resident, demand, migrating-to-GPU, and
	// prefetch-in-flight pages (§11.8).
	PrefetchPages []uint64

	// Byte statistics (§11.9, §11.12). The accounting is consistent:
	// SelectedBytes = DemandBytes + PrefetchCandidateBytes and
	// PrefetchCandidateBytes = ActualPrefetchDMABytes +
	// SuppressedResidentBytes + SuppressedInflightBytes.
	SelectedBytes           uint64 // tbn_selected_bytes
	DemandBytes             uint64 // tbn_demand_bytes
	PrefetchCandidateBytes  uint64 // tbn_prefetch_candidate_bytes
	ActualPrefetchDMABytes  uint64 // tbn_actual_prefetch_dma_bytes
	SuppressedResidentBytes uint64 // tbn_prefetch_suppressed_resident_bytes
	SuppressedInflightBytes uint64 // tbn_prefetch_suppressed_inflight_bytes
}

// selectTBNRegion implements the hierarchical TBN selector (uvm-manager.md
// §11). faultVA is the faulting 4 KB address; residentMask is the GPU
// residency mask, migratingMask the migrating-to-GPU mask, and
// prefetchInFlightMask the prefetch-in-flight mask (the latter two are the
// same registration mask today and are passed separately so the suppression
// formula is exact). All masks are indexed by allocation page.
func selectTBNRegion(
	reg *ManagedAllocationRegistration,
	faultVA uint64,
	residentMask, migratingMask, prefetchInFlightMask []uint64,
) tbnSelection {
	sel := tbnSelection{
		RegionBase: SubBlockStartVA(faultVA),
		RegionSize: subblockSizeBytes,
	}

	// Mandatory 64 KB leaf (§11.2): the 4 KB fault expands to its aligned
	// leaf; the leaf's valid pages are the demand set (§11.7).
	sel.DemandPages = validPagesOf(reg, sel.RegionBase, subblockSizeBytes)
	sel.DemandBytes = uint64(len(sel.DemandPages)) * basePageSize

	// Strict >51% ancestor expansion (§11.4): expand while
	// occupied_pages*100 > total_pages*51 with integer arithmetic, where
	// total is the candidate's valid-allocation page count (§11.10) and
	// occupied is the popcount of the occupancy mask within the candidate.
	// Prefetch-in-flight and migrating-to-GPU pages are NOT occupancy
	// (§11.3, §11.13).
	for level := 1; level <= tbnMaxLevel; level++ {
		nodeVA := tbnNodeBaseVA(faultVA, level)
		total := countValidPages(reg, nodeVA, tbnNodeSize(level))
		occupied := countOccupiedPages(reg, nodeVA, tbnNodeSize(level),
			sel.RegionBase, residentMask)
		if occupied*100 <= total*51 {
			break
		}
		sel.RegionBase = nodeVA
		sel.RegionSize = tbnNodeSize(level)
		sel.Level = level
	}

	// Demand pages requiring migration: neither resident nor in flight.
	demandSet := make(map[uint64]bool, len(sel.DemandPages))
	for _, page := range sel.DemandPages {
		demandSet[page] = true
		if !maskBit(residentMask, page) &&
			!maskBit(migratingMask, page) &&
			!maskBit(prefetchInFlightMask, page) {
			sel.MissingDemand = append(sel.MissingDemand, page)
		}
	}

	// Actual prefetch migration pages (§11.8): SelectedTBNRegionMask AND NOT
	// GPUResidentMask AND NOT DemandFaultMask AND NOT MigratingToGPUMask AND
	// NOT PrefetchInFlightMask.
	selectedPages := validPagesOf(reg, sel.RegionBase, sel.RegionSize)
	sel.SelectedBytes = uint64(len(selectedPages)) * basePageSize
	sel.PrefetchCandidateBytes = sel.SelectedBytes - sel.DemandBytes
	for _, page := range selectedPages {
		if demandSet[page] {
			continue
		}
		resident := maskBit(residentMask, page)
		inflight := maskBit(migratingMask, page) ||
			maskBit(prefetchInFlightMask, page)
		switch {
		case resident:
			sel.SuppressedResidentBytes += basePageSize
		case inflight:
			sel.SuppressedInflightBytes += basePageSize
		default:
			sel.PrefetchPages = append(sel.PrefetchPages, page)
		}
	}
	sel.ActualPrefetchDMABytes = uint64(len(sel.PrefetchPages)) * basePageSize
	return sel
}

// validPagesOf returns the allocation page indices of the valid (allocated)
// 4 KB pages of the VA range [va, va+size). Every TBN candidate is clipped by
// the registration's ValidMask (uvm-manager.md §11.10: "TBN candidate AND
// VABlock.ValidAllocationMask must be used for both threshold denominator and
// final migration selection").
func validPagesOf(
	reg *ManagedAllocationRegistration,
	va, size uint64,
) []uint64 {
	pages := make([]uint64, 0, size/basePageSize)
	for pva := va; pva < va+size; pva += basePageSize {
		if pva < reg.Base || pva >= reg.Base+reg.PageCount*basePageSize {
			continue
		}
		page := (pva - reg.Base) / basePageSize
		if maskBit(reg.ValidMask, page) {
			pages = append(pages, page)
		}
	}
	return pages
}

// countValidPages returns the number of valid 4 KB pages of the VA range.
func countValidPages(reg *ManagedAllocationRegistration, va, size uint64) uint64 {
	return uint64(len(validPagesOf(reg, va, size)))
}

// countOccupiedPages returns the popcount of the TBN occupancy mask within
// the candidate: GPUResidentMask OR CurrentFaultExpanded64KBMask, clipped to
// the candidate's valid pages (§11.3, §11.4). leafBase is the fault leaf's
// 64 KB-aligned base; every valid page of the leaf is occupied regardless of
// residency. Prefetch-in-flight and migrating-to-GPU pages are never counted.
func countOccupiedPages(
	reg *ManagedAllocationRegistration,
	va, size, leafBase uint64,
	residentMask []uint64,
) uint64 {
	leafEnd := leafBase + subblockSizeBytes
	occupied := uint64(0)
	for pva := va; pva < va+size; pva += basePageSize {
		if pva < reg.Base || pva >= reg.Base+reg.PageCount*basePageSize {
			continue
		}
		page := (pva - reg.Base) / basePageSize
		if !maskBit(reg.ValidMask, page) {
			continue
		}
		if pva >= leafBase && pva < leafEnd {
			occupied++
			continue
		}
		if maskBit(residentMask, page) {
			occupied++
		}
	}
	return occupied
}

// tbnStatistics records the TBN selection counters (uvm-manager.md §11.12).
// Each fault event records exactly one selection; the level counters
// partition the selections by their final level (64 KB selections vs
// 128 KB .. 2 MB expansions). The byte counters accumulate the per-selection
// accounting of tbnSelection.
type tbnStatistics struct {
	FaultEvents             uint64 // num_tbn_fault_events
	Selections64KB          uint64 // num_tbn_64kb_selections
	Expansions128KB         uint64 // num_tbn_128kb_expansions
	Expansions256KB         uint64 // num_tbn_256kb_expansions
	Expansions512KB         uint64 // num_tbn_512kb_expansions
	Expansions1MB           uint64 // num_tbn_1mb_expansions
	Expansions2MB           uint64 // num_tbn_2mb_expansions
	SelectedBytes           uint64 // tbn_selected_bytes
	DemandBytes             uint64 // tbn_demand_bytes
	PrefetchCandidateBytes  uint64 // tbn_prefetch_candidate_bytes
	ActualPrefetchDMABytes  uint64 // tbn_actual_prefetch_dma_bytes
	SuppressedResidentBytes uint64 // tbn_prefetch_suppressed_resident_bytes
	SuppressedInflightBytes uint64 // tbn_prefetch_suppressed_inflight_bytes
	UsefulPrefetchedPages   uint64 // tbn_useful_prefetched_4kb_pages
	UnusedPrefetchedPages   uint64 // tbn_unused_prefetched_4kb_pages
}

// recomputeTBN re-reads the transaction's demand residency from the current
// masks and recomputes the TBN migration set: the missing demand pages plus
// the TBN-selected prefetch pages (uvm-manager.md §11). It records the
// selection statistics (§11.12) and resolves the prefetch outcomes of the
// demand leaf: a prefetched page that is still resident when demanded was
// useful; a prefetched page that left the GPU before any demand was unused.
// The returned page list is sorted in VA order so maximal-run formation is
// optimal. The fault service (todo 15) calls this at service time. // sbin_codex
func (m *UVMManager) recomputeTBN(tx *faultTransaction) []uint64 {
	m.Lock()
	defer m.Unlock()

	reg := tx.reg
	if reg == nil {
		return nil
	}

	// Resolve prefetch outcomes (§11.12): demand-leaf pages with the
	// prefetched mark are useful when still resident, unused otherwise; the
	// mark is cleared in both cases (the provenance is now resolved).
	for _, page := range tx.DemandPages {
		if maskBit(reg.PrefetchedMask, page) {
			if maskBit(reg.ResidentMask, page) {
				m.tbnStats.UsefulPrefetchedPages++
			} else {
				m.tbnStats.UnusedPrefetchedPages++
			}
			setMaskBit(reg.PrefetchedMask, page, false)
		}
	}
	// Prefetched pages outside the demand leaf that are no longer resident
	// were never used (they left the GPU before any demand).
	for w := range reg.PrefetchedMask {
		word := reg.PrefetchedMask[w]
		for word != 0 {
			bit := word & -word
			page := uint64(w*64) + uint64(bits.TrailingZeros64(bit))
			if !maskBit(reg.ResidentMask, page) {
				m.tbnStats.UnusedPrefetchedPages++
				setMaskBit(reg.PrefetchedMask, page, false)
			}
			word &^= bit
		}
	}

	// The occupancy is GPUResidentMask OR CurrentFaultExpanded64KBMask; the
	// registration's single in-flight mask plays both the migrating-to-GPU
	// and prefetch-in-flight roles for duplicate suppression (§11.8).
	sel := selectTBNRegion(reg, tx.RegionBase,
		reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)

	m.tbnStats.FaultEvents++
	switch sel.Level {
	case 0:
		m.tbnStats.Selections64KB++
	case 1:
		m.tbnStats.Expansions128KB++
	case 2:
		m.tbnStats.Expansions256KB++
	case 3:
		m.tbnStats.Expansions512KB++
	case 4:
		m.tbnStats.Expansions1MB++
	case 5:
		m.tbnStats.Expansions2MB++
	}
	m.tbnStats.SelectedBytes += sel.SelectedBytes
	m.tbnStats.DemandBytes += sel.DemandBytes
	m.tbnStats.PrefetchCandidateBytes += sel.PrefetchCandidateBytes
	m.tbnStats.ActualPrefetchDMABytes += sel.ActualPrefetchDMABytes
	m.tbnStats.SuppressedResidentBytes += sel.SuppressedResidentBytes
	m.tbnStats.SuppressedInflightBytes += sel.SuppressedInflightBytes

	// The migration set is the missing demand pages plus the actual prefetch
	// pages, merged in VA order.
	tx.prefetchPages = sel.PrefetchPages
	migration := make([]uint64, 0, len(sel.MissingDemand)+len(sel.PrefetchPages))
	i, j := 0, 0
	for i < len(sel.MissingDemand) || j < len(sel.PrefetchPages) {
		switch {
		case i >= len(sel.MissingDemand):
			migration = append(migration, sel.PrefetchPages[j])
			j++
		case j >= len(sel.PrefetchPages):
			migration = append(migration, sel.MissingDemand[i])
			i++
		case sel.MissingDemand[i] < sel.PrefetchPages[j]:
			migration = append(migration, sel.MissingDemand[i])
			i++
		default:
			migration = append(migration, sel.PrefetchPages[j])
			j++
		}
	}
	return migration
}

// TBNStats returns a copy of the TBN selection statistics (§11.12). // sbin_codex
func (m *UVMManager) TBNStats() tbnStatistics {
	m.Lock()
	defer m.Unlock()

	return m.tbnStats
}

// regionsTouchedByPlanLocked returns the set of 64 KB region base VAs touched
// by the migration plan's pages. The caller must hold the manager lock. // sbin_codex
func (m *UVMManager) regionsTouchedByPlanLocked(
	reg *ManagedAllocationRegistration,
	tx *faultTransaction,
) map[uint64]bool {
	regions := make(map[uint64]bool)
	if tx.plan == nil {
		return regions
	}
	for _, run := range tx.plan.Runs {
		for _, page := range run.Pages {
			regions[SubBlockStartVA(reg.Base+page*basePageSize)] = true
		}
	}
	return regions
}

// regionFullyResidentLocked reports whether every valid page of the 64 KB
// region containing regionBase is GPU-resident. The caller must hold the
// manager lock. // sbin_codex
func (m *UVMManager) regionFullyResidentLocked(
	reg *ManagedAllocationRegistration,
	regionBase uint64,
) bool {
	blockIdx := (BlockForVA(regionBase) - BlockForVA(reg.Base)) / vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (regionBase - block.StartVA) / subblockSizeBytes
	allocStart, valid := (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
	for i := uint64(0); i < valid; i++ {
		if !maskBit(reg.ResidentMask, allocStart+i) {
			return false
		}
	}
	return true
}

// completeRegionAdmissionLocked transitions one region to GPU_RESIDENT when
// every valid page is resident; a region with pages still in flight (an
// overlapping migration) stays MIGRATING_TO_GPU. The caller must hold the
// manager lock. // sbin_codex
func (m *UVMManager) completeRegionAdmissionLocked(
	reg *ManagedAllocationRegistration,
	gpu int,
	regionBase uint64,
	now sim.VTimeInSec,
) error {
	sm := m.faultRegionMachineLocked(reg, gpu, regionBase)
	switch sm.Region.State {
	case RegionMigratingToGPU:
		if !m.regionFullyResidentLocked(reg, regionBase) {
			return nil
		}
		return sm.Transition(RegionGPUResident, now)
	case RegionGPUResident:
		// Already resident (an overlapping migration completed first).
		return nil
	default:
		return fmt.Errorf(
			"uvm: migration completion in illegal region state %s", sm.Region.State)
	}
}

// completePrefetchRegions publishes GPU_RESIDENT for every TBN-prefetched
// region whose pages the migration made fully resident; a region with pages
// still in flight stays MIGRATING_TO_GPU. The fault region's admission
// completion is completeFaultMigration's job. The fault service calls it
// after completeFaultMigration. // sbin_codex
func (m *UVMManager) completePrefetchRegions(
	tx *faultTransaction,
	now sim.VTimeInSec,
) error {
	m.Lock()
	defer m.Unlock()

	reg := tx.reg
	if reg == nil || tx.plan == nil {
		return nil
	}
	for regionBase := range m.regionsTouchedByPlanLocked(reg, tx) {
		if err := m.completeRegionAdmissionLocked(reg, tx.GPU, regionBase, now); err != nil {
			return err
		}
	}
	return nil
}

// markPrefetched sets the prefetched-provenance bits of the pages a fault
// transaction's TBN prefetch migrated (uvm-manager.md §11.11), so later
// faults can account useful/unused prefetch outcomes (§11.12). The fault
// service calls it after the migration commit. // sbin_codex
func (m *UVMManager) markPrefetched(tx *faultTransaction) {
	m.Lock()
	defer m.Unlock()

	if tx.reg == nil {
		return
	}
	for _, page := range tx.prefetchPages {
		setMaskBit(tx.reg.PrefetchedMask, page, true)
	}
}