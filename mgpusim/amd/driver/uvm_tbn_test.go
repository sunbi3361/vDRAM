package driver

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: NVIDIA-style hierarchical TBN selection over exact occupancy
// masks (todo 17 of plan mgpusim-uvm-manager, uvm-manager.md §11). The QA
// regex 'TestUVMTBN(Leaf|MultiLevelExpansion|StrictMajority|AllocationMask|
// Stats)' runs the fixtures in this file: the mandatory 64 KB leaf, strict
// >51% ancestor expansion through 64 KB -> 128 KB -> 256 KB -> 512 KB ->
// 1 MB -> 2 MB, occupancy = GPUResidentMask OR CurrentFaultExpanded64KBMask,
// valid-allocation clipping, duplicate suppression (resident /
// prefetch-in-flight / migrating) in the actual migration pages, and the
// exact §11.12 statistics. For every normative node size (16, 32, 64, 128,
// 256, 512 pages) the table-driven masks cover the greatest integer
// occupancy satisfying 100*occupied <= 51*pages (does NOT expand) and the
// immediately higher integer occupancy satisfying 100*occupied > 51*pages
// (expands through the applicable levels). No test requires an impossible
// integer mask equal to 51% (51*pages is never divisible by 100 for the
// power-of-two node sizes).

// tbnReg512 builds a full 2 MB VA-block registration (512 pages at base).
func tbnReg512(base uint64) *ManagedAllocationRegistration {
	return buildTestRegistration(vm.PID(1), base, 512)
}

// tbnResident sets the GPU-residency bit of allocation page `page`.
func tbnResident(reg *ManagedAllocationRegistration, page uint64) {
	setMaskBit(reg.ResidentMask, page, true)
}

// tbnInFlight sets the in-flight bit of allocation page `page`.
func tbnInFlight(reg *ManagedAllocationRegistration, page uint64) {
	setMaskBit(reg.InFlightMask, page, true)
}

// tbnPagesRange returns the inclusive page range [lo, hi].
func tbnPagesRange(lo, hi uint64) []uint64 {
	pages := make([]uint64, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		pages = append(pages, p)
	}
	return pages
}

// TestUVMTBNLeaf proves the mandatory 64 KB leaf (§11.2): one 4 KB fault
// expands to the full aligned leaf in the occupancy tree, the leaf's valid
// pages are the demand set (§11.7), and a lone leaf (16/32 = 50%) never
// expands (§11.5).
func TestUVMTBNLeaf(t *testing.T) {
	reg := tbnReg512(0)

	// A single 4 KB fault at page 1: the whole leaf 0 becomes occupied.
	sel := selectTBNRegion(reg, 0x1000, reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)
	if sel.RegionBase != 0 || sel.RegionSize != 64*mem.KB || sel.Level != 0 {
		t.Fatalf("selection = %#x+%d level %d, want leaf 0 (64 KB, level 0)",
			sel.RegionBase, sel.RegionSize, sel.Level)
	}
	if len(sel.DemandPages) != 16 || sel.DemandPages[0] != 0 || sel.DemandPages[15] != 15 {
		t.Fatalf("demand pages = %v, want allocation pages 0..15 (full leaf)",
			sel.DemandPages)
	}
	if len(sel.MissingDemand) != 16 {
		t.Errorf("missing demand = %d pages, want 16 (no residents)", len(sel.MissingDemand))
	}
	if len(sel.PrefetchPages) != 0 {
		t.Errorf("prefetch pages = %v, want none (16/32 = 50%% <= 51%%)", sel.PrefetchPages)
	}
	if sel.SelectedBytes != 64*mem.KB || sel.DemandBytes != 64*mem.KB ||
		sel.PrefetchCandidateBytes != 0 || sel.ActualPrefetchDMABytes != 0 {
		t.Errorf("bytes = %d/%d/%d/%d, want 64KB/64KB/0/0",
			sel.SelectedBytes, sel.DemandBytes, sel.PrefetchCandidateBytes,
			sel.ActualPrefetchDMABytes)
	}

	// A fault in a different leaf selects that leaf.
	sel4 := selectTBNRegion(reg, 0x100000, reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)
	if sel4.RegionBase != 0x100000 || sel4.RegionSize != 64*mem.KB {
		t.Errorf("selection = %#x+%d, want leaf 4 (0x100000, 64 KB)",
			sel4.RegionBase, sel4.RegionSize)
	}
	if len(sel4.DemandPages) != 16 || sel4.DemandPages[0] != 256 {
		t.Errorf("demand pages = %v, want allocation pages 256..271", sel4.DemandPages)
	}

	// Partial leaf at the allocation edge: a 64 KB allocation at 4096 has 15
	// valid pages in leaf 0; the demand set is clipped by the valid
	// allocation (§11.10).
	regEdge := buildTestRegistration(vm.PID(1), 4096, 16)
	selEdge := selectTBNRegion(regEdge, 4096,
		regEdge.ResidentMask, regEdge.InFlightMask, regEdge.InFlightMask)
	if len(selEdge.DemandPages) != 15 || selEdge.DemandPages[0] != 0 ||
		selEdge.DemandPages[14] != 14 {
		t.Fatalf("edge demand pages = %v, want allocation pages 0..14 (15 valid)",
			selEdge.DemandPages)
	}
	if selEdge.DemandBytes != 15*basePageSize {
		t.Errorf("edge demand bytes = %d, want %d", selEdge.DemandBytes, 15*basePageSize)
	}
}

// TestUVMTBNStrictMajority table-drives the strict >51% boundary for every
// normative node size: the greatest integer occupancy satisfying
// 100*occupied <= 51*pages does NOT expand to the candidate level, and the
// immediately higher integer occupancy expands through it (§11.4). Lower
// levels are fully occupied so the boundary at the candidate level decides.
func TestUVMTBNStrictMajority(t *testing.T) {
	for _, tc := range []struct {
		level     int
		pages     uint64 // candidate node size in pages
		notExpand uint64 // greatest occupied with occupied*100 <= pages*51
		expand    uint64 // immediately higher: occupied*100 > pages*51
	}{
		{1, 32, 16, 17},
		{2, 64, 32, 33},
		{3, 128, 65, 66},
		{4, 256, 130, 131},
		{5, 512, 261, 262},
	} {
		n := tc.pages
		// The greatest integer occupancy at or below 51% is the floor; the
		// immediately higher integer is floor+1. No integer equals 51%
		// exactly for these node sizes.
		if k := (51 * n) / 100; k != tc.notExpand {
			t.Fatalf("level %d: floor(51%% of %d) = %d, want %d",
				tc.level, n, k, tc.notExpand)
		}
		if (tc.notExpand*100 <= n*51) != true || (tc.expand*100 > n*51) != true {
			t.Fatalf("level %d: boundary pair %d/%d does not straddle 51%% of %d",
				tc.level, tc.notExpand, tc.expand, n)
		}

		// Not-expand case: the candidate has exactly K occupied pages (the
		// leaf plus K-16 residents); every lower level is full.
		reg := tbnReg512(0)
		for page := uint64(16); page < tc.notExpand; page++ {
			tbnResident(reg, page)
		}
		sel := selectTBNRegion(reg, 0x1000, reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)
		wantLevel := tc.level - 1
		if sel.Level != wantLevel || sel.RegionSize != tbnNodeSize(wantLevel) {
			t.Errorf("level %d not-expand: selected level %d (%d bytes), want %d (%d bytes)",
				tc.level, sel.Level, sel.RegionSize, wantLevel, tbnNodeSize(wantLevel))
		}
		if len(sel.MissingDemand) != 16 {
			t.Errorf("level %d not-expand: missing demand = %d, want 16",
				tc.level, len(sel.MissingDemand))
		}
		if len(sel.PrefetchPages) != 0 {
			t.Errorf("level %d not-expand: prefetch = %v, want none", tc.level, sel.PrefetchPages)
		}
		half := n / 2
		wantSelected := half * basePageSize
		wantCandidate := (half - 16) * basePageSize
		wantSuppressed := uint64(0)
		if tc.notExpand < half {
			wantSuppressed = (tc.notExpand - 16) * basePageSize
		} else {
			wantSuppressed = (half - 16) * basePageSize
		}
		if sel.SelectedBytes != wantSelected || sel.DemandBytes != 16*basePageSize ||
			sel.PrefetchCandidateBytes != wantCandidate ||
			sel.ActualPrefetchDMABytes != 0 ||
			sel.SuppressedResidentBytes != wantSuppressed ||
			sel.SuppressedInflightBytes != 0 {
			t.Errorf("level %d not-expand bytes = %d/%d/%d/%d/%d/%d, want %d/%d/%d/0/%d/0",
				tc.level, sel.SelectedBytes, sel.DemandBytes, sel.PrefetchCandidateBytes,
				sel.ActualPrefetchDMABytes, sel.SuppressedResidentBytes,
				sel.SuppressedInflightBytes, wantSelected, 16*basePageSize,
				wantCandidate, wantSuppressed)
		}

		// Expand case: the candidate has exactly K+1 occupied pages; every
		// lower level is full, so the selection expands to the candidate and
		// stops there (the next ancestor has only K+1 of 2N pages).
		reg2 := tbnReg512(0)
		for page := uint64(16); page < tc.expand; page++ {
			tbnResident(reg2, page)
		}
		sel2 := selectTBNRegion(reg2, 0x1000,
			reg2.ResidentMask, reg2.InFlightMask, reg2.InFlightMask)
		if sel2.Level != tc.level || sel2.RegionSize != tbnNodeSize(tc.level) {
			t.Errorf("level %d expand: selected level %d (%d bytes), want %d (%d bytes)",
				tc.level, sel2.Level, sel2.RegionSize, tc.level, tbnNodeSize(tc.level))
		}
		if len(sel2.MissingDemand) != 16 {
			t.Errorf("level %d expand: missing demand = %d, want 16",
				tc.level, len(sel2.MissingDemand))
		}
		wantPrefetch := tbnPagesRange(tc.expand, n-1)
		if len(sel2.PrefetchPages) != len(wantPrefetch) {
			t.Fatalf("level %d expand: prefetch = %d pages, want %d",
				tc.level, len(sel2.PrefetchPages), len(wantPrefetch))
		}
		for i := range wantPrefetch {
			if sel2.PrefetchPages[i] != wantPrefetch[i] {
				t.Errorf("level %d expand: prefetch[%d] = %d, want %d",
					tc.level, i, sel2.PrefetchPages[i], wantPrefetch[i])
			}
		}
		if sel2.SelectedBytes != n*basePageSize ||
			sel2.DemandBytes != 16*basePageSize ||
			sel2.PrefetchCandidateBytes != (n-16)*basePageSize ||
			sel2.ActualPrefetchDMABytes != (n-tc.expand)*basePageSize ||
			sel2.SuppressedResidentBytes != (tc.expand-16)*basePageSize ||
			sel2.SuppressedInflightBytes != 0 {
			t.Errorf("level %d expand bytes = %d/%d/%d/%d/%d/%d, want %d/%d/%d/%d/%d/0",
				tc.level, sel2.SelectedBytes, sel2.DemandBytes,
				sel2.PrefetchCandidateBytes, sel2.ActualPrefetchDMABytes,
				sel2.SuppressedResidentBytes, sel2.SuppressedInflightBytes,
				n*basePageSize, 16*basePageSize, (n-16)*basePageSize,
				(n-tc.expand)*basePageSize, (tc.expand-16)*basePageSize)
		}
	}
}

// TestUVMTBNMultiLevelExpansion proves the hierarchical walk through the
// 64 KB -> 128 KB -> 256 KB -> 512 KB -> 1 MB -> 2 MB candidates (§11.4):
// full occupancy expands to the 2 MB root, the §11.6 example selects 256 KB,
// a dense-but-not-majority occupancy stops at 1 MB, and prefetch-in-flight /
// migrating pages never count as occupancy (§11.3, §11.13).
func TestUVMTBNMultiLevelExpansion(t *testing.T) {
	// Full occupancy: every level passes -> the 2 MB root.
	reg := tbnReg512(0)
	for page := uint64(16); page < 512; page++ {
		tbnResident(reg, page)
	}
	sel := selectTBNRegion(reg, 0x1000, reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)
	if sel.Level != 5 || sel.RegionSize != 2*mem.MB || sel.RegionBase != 0 {
		t.Errorf("full occupancy: selected %#x+%d level %d, want 2 MB root",
			sel.RegionBase, sel.RegionSize, sel.Level)
	}
	if len(sel.PrefetchPages) != 0 {
		t.Errorf("full occupancy: prefetch = %v, want none (everything resident)",
			sel.PrefetchPages)
	}

	// §11.6 example: a 256 KB candidate with 192 KB occupied (fault leaf +
	// two resident leaves) selects 256 KB.
	reg6 := tbnReg512(0)
	for page := uint64(16); page < 48; page++ {
		tbnResident(reg6, page)
	}
	sel6 := selectTBNRegion(reg6, 0x1000,
		reg6.ResidentMask, reg6.InFlightMask, reg6.InFlightMask)
	if sel6.Level != 2 || sel6.RegionSize != 256*mem.KB {
		t.Errorf("§11.6: selected level %d (%d bytes), want 256 KB (192/256 = 75%%)",
			sel6.Level, sel6.RegionSize)
	}
	if len(sel6.PrefetchPages) != 16 || sel6.PrefetchPages[0] != 48 ||
		sel6.PrefetchPages[15] != 63 {
		t.Errorf("§11.6: prefetch = %v, want pages 48..63", sel6.PrefetchPages)
	}

	// Dense occupancy that fails the 2 MB ancestor: 256 occupied of 512
	// pages (50%) stops at 1 MB.
	regDense := tbnReg512(0)
	for page := uint64(16); page < 256; page++ {
		tbnResident(regDense, page)
	}
	selDense := selectTBNRegion(regDense, 0x1000,
		regDense.ResidentMask, regDense.InFlightMask, regDense.InFlightMask)
	if selDense.Level != 4 || selDense.RegionSize != 1*mem.MB {
		t.Errorf("dense: selected level %d (%d bytes), want 1 MB (256/512 = 50%%)",
			selDense.Level, selDense.RegionSize)
	}

	// Prefetch-in-flight / migrating pages are NOT occupancy: with the leaf
	// and one resident page (17/32 > 51%) the 128 KB candidate passes, but
	// the 15 in-flight pages in its other half do not push the 256 KB
	// candidate past 51% (32/64 = 50%). If in-flight counted as occupancy
	// the selection would expand to 256 KB.
	regInf := tbnReg512(0)
	for page := uint64(16); page < 32; page++ {
		tbnResident(regInf, page)
	}
	for page := uint64(32); page < 48; page++ {
		tbnInFlight(regInf, page)
	}
	selInf := selectTBNRegion(regInf, 0x1000,
		regInf.ResidentMask, regInf.InFlightMask, regInf.InFlightMask)
	if selInf.Level != 1 || selInf.RegionSize != 128*mem.KB {
		t.Errorf("in-flight: selected level %d (%d bytes), want 128 KB (in-flight not occupancy)",
			selInf.Level, selInf.RegionSize)
	}

	// In-flight pages inside the prefetch candidate are suppressed from the
	// actual DMA pages (§11.8): residents 16..47 expand to 256 KB; the
	// candidate pages 48..63 split into 8 in-flight (suppressed) and 8
	// actual prefetch pages.
	regInf2 := tbnReg512(0)
	for page := uint64(16); page < 48; page++ {
		tbnResident(regInf2, page)
	}
	for page := uint64(48); page < 56; page++ {
		tbnInFlight(regInf2, page)
	}
	selInf2 := selectTBNRegion(regInf2, 0x1000,
		regInf2.ResidentMask, regInf2.InFlightMask, regInf2.InFlightMask)
	if selInf2.Level != 2 || selInf2.RegionSize != 256*mem.KB {
		t.Fatalf("in-flight suppression: selected level %d (%d bytes), want 256 KB",
			selInf2.Level, selInf2.RegionSize)
	}
	if len(selInf2.PrefetchPages) != 8 || selInf2.PrefetchPages[0] != 56 {
		t.Errorf("in-flight suppression: prefetch = %v, want pages 56..63",
			selInf2.PrefetchPages)
	}
	if selInf2.SuppressedInflightBytes != 8*basePageSize ||
		selInf2.ActualPrefetchDMABytes != 8*basePageSize {
		t.Errorf("in-flight suppression bytes = %d/%d, want %d/%d",
			selInf2.SuppressedInflightBytes, selInf2.ActualPrefetchDMABytes,
			8*basePageSize, 8*basePageSize)
	}

	// Sparse residents do not expand the leaf.
	regSparse := tbnReg512(0)
	for _, page := range []uint64{100, 200, 300} {
		tbnResident(regSparse, page)
	}
	selSparse := selectTBNRegion(regSparse, 0x1000,
		regSparse.ResidentMask, regSparse.InFlightMask, regSparse.InFlightMask)
	if selSparse.Level != 0 {
		t.Errorf("sparse: selected level %d, want leaf", selSparse.Level)
	}
}

// TestUVMTBNAllocationMask proves the valid-allocation clipping (§11.10):
// the threshold denominator is the candidate's valid page count, the TBN
// region never crosses a 2 MB VA-block boundary, and no invalid allocation
// bit ever migrates.
func TestUVMTBNAllocationMask(t *testing.T) {
	// Partial first VA block: a 256 KB allocation at 4096 has 31 valid pages in
	// the [0, 128 KB) candidate. The fault leaf occupies 15 of them
	// (48.4% <= 51%), so the selection stops at the leaf: the threshold
	// denominator is the valid page count (31), not the node size (32).
	reg := buildTestRegistration(vm.PID(1), 4096, 64)
	sel := selectTBNRegion(reg, 4096, reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)
	if sel.Level != 0 || sel.RegionSize != 64*mem.KB {
		t.Fatalf("partial block: selected level %d (%d bytes), want the 64 KB leaf",
			sel.Level, sel.RegionSize)
	}
	if len(sel.MissingDemand) != 15 {
		t.Errorf("partial block: missing demand = %d, want 15 (pages 0..14)",
			len(sel.MissingDemand))
	}
	if len(sel.PrefetchPages) != 0 {
		t.Errorf("partial block: prefetch = %v, want none (15/31 <= 51%%)",
			sel.PrefetchPages)
	}
	for _, page := range append(append([]uint64{}, sel.MissingDemand...), sel.PrefetchPages...) {
		if !maskBit(reg.ValidMask, page) {
			t.Errorf("partial block: invalid page %d migrated", page)
		}
	}

	// Aligned 64 KB allocation: the valid-allocation clipping makes every
	// candidate fully occupied (16/16 = 100%), so the selection expands all
	// the way to the 2 MB root even though the node sizes are 32..512 pages.
	regSmall := buildTestRegistration(vm.PID(1), 0, 16)
	selSmall := selectTBNRegion(regSmall, 0x1000,
		regSmall.ResidentMask, regSmall.InFlightMask, regSmall.InFlightMask)
	if selSmall.Level != 5 || selSmall.RegionSize != 2*mem.MB {
		t.Fatalf("aligned 64 KB: selected level %d (%d bytes), want the 2 MB root",
			selSmall.Level, selSmall.RegionSize)
	}
	if len(selSmall.MissingDemand) != 16 {
		t.Errorf("aligned 64 KB: missing demand = %d, want 16", len(selSmall.MissingDemand))
	}
	for _, page := range append(append([]uint64{}, selSmall.MissingDemand...), selSmall.PrefetchPages...) {
		if !maskBit(regSmall.ValidMask, page) {
			t.Errorf("aligned 64 KB: invalid page %d migrated", page)
		}
	}

	// Fault near the end of a full VA block: every candidate stays inside
	// the block (§11.10: TBN never crosses the 2 MB boundary).
	regFull := tbnReg512(0)
	selEnd := selectTBNRegion(regFull, 0x1FF000,
		regFull.ResidentMask, regFull.InFlightMask, regFull.InFlightMask)
	if selEnd.RegionBase != 0x1F0000 || selEnd.RegionSize != 64*mem.KB {
		t.Errorf("block end: selected %#x+%d, want leaf 31 (0x1F0000, 64 KB)",
			selEnd.RegionBase, selEnd.RegionSize)
	}
	if selEnd.RegionBase+selEnd.RegionSize > 2*mem.MB {
		t.Error("block end: selection crosses the 2 MB VA-block boundary")
	}

	// With the sibling leaf resident, the 128 KB candidate at the block end
	// passes (32/32) but the 256 KB candidate fails (32/64 = 50%): the
	// selection stops at 128 KB inside the block.
	regEnd2 := tbnReg512(0)
	for page := uint64(480); page < 496; page++ {
		tbnResident(regEnd2, page)
	}
	selEnd2 := selectTBNRegion(regEnd2, 0x1FF000,
		regEnd2.ResidentMask, regEnd2.InFlightMask, regEnd2.InFlightMask)
	if selEnd2.Level != 1 || selEnd2.RegionBase != 0x1E0000 ||
		selEnd2.RegionSize != 128*mem.KB {
		t.Errorf("block end resident: selected %#x+%d level %d, want 0x1E0000+128 KB",
			selEnd2.RegionBase, selEnd2.RegionSize, selEnd2.Level)
	}
	if selEnd2.RegionBase+selEnd2.RegionSize != 2*mem.MB {
		t.Error("block end resident: selection does not end at the block boundary")
	}
	for _, page := range append(append([]uint64{}, selEnd2.MissingDemand...), selEnd2.PrefetchPages...) {
		if !maskBit(regEnd2.ValidMask, page) {
			t.Errorf("block end resident: invalid page %d migrated", page)
		}
	}
}

// TestUVMTBNStats proves the exact §11.12 statistics: the byte accounting is
// consistent (selected = demand + prefetch-candidate; prefetch-candidate =
// actual DMA + suppressed resident + suppressed in-flight), each selection
// increments exactly one level counter, and the useful/unused prefetch
// outcomes are resolved from the prefetched-provenance mask.
func TestUVMTBNStats(t *testing.T) {
	// Pure selector: a 256 KB selection with resident and in-flight
	// suppression in the prefetch candidate.
	reg := tbnReg512(0)
	for page := uint64(16); page < 48; page++ {
		tbnResident(reg, page)
	}
	for page := uint64(48); page < 56; page++ {
		tbnInFlight(reg, page)
	}
	sel := selectTBNRegion(reg, 0x1000,
		reg.ResidentMask, reg.InFlightMask, reg.InFlightMask)
	if sel.Level != 2 {
		t.Fatalf("stats selection level = %d, want 2 (256 KB)", sel.Level)
	}
	if sel.SelectedBytes != 256*mem.KB || sel.DemandBytes != 64*mem.KB ||
		sel.PrefetchCandidateBytes != 192*mem.KB {
		t.Errorf("stats bytes = %d/%d/%d, want 256KB/64KB/192KB",
			sel.SelectedBytes, sel.DemandBytes, sel.PrefetchCandidateBytes)
	}
	if sel.ActualPrefetchDMABytes != 8*basePageSize ||
		sel.SuppressedResidentBytes != 128*mem.KB ||
		sel.SuppressedInflightBytes != 8*basePageSize {
		t.Errorf("stats prefetch bytes = %d/%d/%d, want 32KB/128KB/32KB",
			sel.ActualPrefetchDMABytes, sel.SuppressedResidentBytes,
			sel.SuppressedInflightBytes)
	}
	if sel.SelectedBytes != sel.DemandBytes+sel.PrefetchCandidateBytes {
		t.Error("stats: selected != demand + prefetch-candidate")
	}
	if sel.PrefetchCandidateBytes != sel.ActualPrefetchDMABytes+
		sel.SuppressedResidentBytes+sel.SuppressedInflightBytes {
		t.Error("stats: prefetch-candidate != actual + suppressed resident + suppressed in-flight")
	}

	// Manager recording: recomputeTBN accumulates the counters and resolves
	// useful/unused prefetch outcomes from the provenance mask.
	d, _, _ := buildFaultDriver(t, false)
	ctx := d.Init()
	d.AllocateManagedMemory(ctx, 2*mem.MB)
	regM := d.uvm.registrations[0]

	// First service: 15 demand pages (leaf 0 of the 4096-based allocation),
	// no residents -> leaf selection, 60 KB selected/demand.
	tx1 := &faultTransaction{
		PID:         ctx.pid,
		GPU:         1,
		RegionBase:  0,
		DemandPages: tbnPagesRange(0, 14),
		reg:         regM,
	}
	mig1 := d.uvm.recomputeTBN(tx1)
	if len(mig1) != 15 {
		t.Fatalf("migration 1 = %d pages, want 15", len(mig1))
	}
	st := d.uvm.TBNStats()
	if st.FaultEvents != 1 || st.Selections64KB != 1 ||
		st.SelectedBytes != 15*basePageSize || st.DemandBytes != 15*basePageSize {
		t.Errorf("stats after 1 = %+v, want 1 fault event, 1 leaf selection, 60 KB",
			st)
	}

	// The demand pages are resident and prefetched (a prior TBN prefetch
	// satisfied them): the second service counts them useful and clears the
	// provenance marks.
	for page := uint64(0); page < 15; page++ {
		setResident(regM, page, true)
		setMaskBit(regM.PrefetchedMask, page, true)
	}
	tx2 := &faultTransaction{
		PID:         ctx.pid,
		GPU:         1,
		RegionBase:  0,
		DemandPages: tbnPagesRange(0, 14),
		reg:         regM,
	}
	if mig2 := d.uvm.recomputeTBN(tx2); len(mig2) != 0 {
		t.Fatalf("migration 2 = %d pages, want 0 (demand satisfied)", len(mig2))
	}
	st = d.uvm.TBNStats()
	if st.FaultEvents != 2 || st.Selections64KB != 2 || st.UsefulPrefetchedPages != 15 {
		t.Errorf("stats after 2 = %+v, want 2 events, 15 useful prefetched pages", st)
	}
	for page := uint64(0); page < 15; page++ {
		if maskBit(regM.PrefetchedMask, page) {
			t.Errorf("page %d prefetched mark not cleared after useful accounting", page)
		}
	}

	// Prefetched pages that left the GPU before any demand are unused.
	setMaskBit(regM.PrefetchedMask, 16, true)
	setMaskBit(regM.PrefetchedMask, 17, true)
	tx3 := &faultTransaction{
		PID:         ctx.pid,
		GPU:         1,
		RegionBase:  0,
		DemandPages: tbnPagesRange(0, 14),
		reg:         regM,
	}
	d.uvm.recomputeTBN(tx3)
	st = d.uvm.TBNStats()
	if st.FaultEvents != 3 || st.UnusedPrefetchedPages != 2 {
		t.Errorf("stats after 3 = %+v, want 3 events, 2 unused prefetched pages", st)
	}
	if maskBit(regM.PrefetchedMask, 16) || maskBit(regM.PrefetchedMask, 17) {
		t.Error("unused prefetched marks not cleared")
	}

	// Prefetched pages still resident outside the demand leaf are not
	// resolved yet: their marks survive.
	setResident(regM, 18, true)
	setResident(regM, 19, true)
	setMaskBit(regM.PrefetchedMask, 18, true)
	setMaskBit(regM.PrefetchedMask, 19, true)
	tx4 := &faultTransaction{
		PID:         ctx.pid,
		GPU:         1,
		RegionBase:  0,
		DemandPages: tbnPagesRange(0, 14),
		reg:         regM,
	}
	d.uvm.recomputeTBN(tx4)
	st = d.uvm.TBNStats()
	if st.FaultEvents != 4 || st.UsefulPrefetchedPages != 15 ||
		st.UnusedPrefetchedPages != 2 {
		t.Errorf("stats after 4 = %+v, want 4 events, 15 useful, 2 unused", st)
	}
	if !maskBit(regM.PrefetchedMask, 18) || !maskBit(regM.PrefetchedMask, 19) {
		t.Error("resident prefetched marks outside the demand leaf were resolved")
	}
	if st.Selections64KB+st.Expansions128KB+st.Expansions256KB+
		st.Expansions512KB+st.Expansions1MB+st.Expansions2MB != st.FaultEvents {
		t.Error("level counters do not partition the fault events")
	}

	// End-to-end: a fault service's TBN selection prefetches pages 47..62
	// (residents 15..46 expand the selection to 256 KB); the commit marks
	// them prefetched (§11.11) and the statistics record the selection.
	d2, mw2, _ := buildFaultDriver(t, false)
	ctx2 := d2.Init()
	pid2 := ctx2.pid
	ptr2 := d2.AllocateManagedMemory(ctx2, 2*mem.MB)
	reg2 := d2.uvm.registrations[0]
	makeRegionResident(t, d2, reg2, 0, 1) // region 1 (pages 15..30) resident
	makeRegionResident(t, d2, reg2, 0, 2) // region 2 (pages 31..46) resident

	intakeFault(t, d2, pid2, 1, uint64(ptr2))
	txA := mw2.queue[0]
	mw2.Tick()
	mw2.Handle(txA.latencyEvent)
	mw2.Tick()
	reqs := drainRequests(d2)
	if len(reqs) != 2 {
		t.Fatalf("A DMA reqs = %d, want 2 (demand run 0..14 + prefetch run 47..62)",
			len(reqs))
	}
	for _, req := range reqs {
		deliverGeneralRsp(t, d2, req)
	}
	mw2.Tick()
	reqs = drainRequests(d2)
	// sbin_codex (todo 25): §21.2 — the AC-off fault migration is
	// INVALID -> GPU_LOCAL, which needs no TLB invalidation; the replay
	// follows the DMA directly.
	// if len(reqs) != 1 {
	// 	t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	// }
	// tlbA, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	// if !ok {
	// 	t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	// }
	// deliverTLBAck(t, d2, tlbA)
	// mw2.Tick()
	// reqs = drainRequests(d2)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 replay", len(reqs))
	}
	replayA, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMFaultReplayReq", reqs[0])
	}
	for page := uint64(47); page < 63; page++ {
		if !maskBit(reg2.PrefetchedMask, page) {
			t.Errorf("prefetched page %d not marked after the prefetch commit", page)
		}
	}
	if maskBit(reg2.PrefetchedMask, 0) {
		t.Error("demand page 0 marked prefetched")
	}
	st2 := d2.uvm.TBNStats()
	if st2.FaultEvents != 1 || st2.Expansions256KB != 1 ||
		st2.ActualPrefetchDMABytes != 16*basePageSize {
		t.Errorf("end-to-end stats = %+v, want 1 event, 1 x 256 KB expansion, 64 KB prefetch DMA",
			st2)
	}
	deliverReplayAck(t, d2, replayA)
	mw2.Tick()
}