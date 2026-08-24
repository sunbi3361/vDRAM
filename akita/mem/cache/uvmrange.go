package cache

// sbin_codex: scoped UVM range writeback/invalidation framework (plan todo 13
// of mgpusim-uvm-manager, uvm-manager.md §19.1). One aligned 64 KB command
// carries the operation (WRITEBACK_ONLY | WRITEBACK_INVALIDATE), PID, VA base,
// valid-page mask, and the exact maximal physical runs/page mappings. The
// matcher decides which lines belong to the range: baseline caches match by
// physical runs; virtual caches match by PID+VA and validate the stored
// annotation PA (which subsumes generation staleness: a remapped line carries
// a different HBMPA). The page granularity of the mask is 4 KB (16 pages per
// 64 KB region), matching the plan's 4 KB page-mask residency tracking.

import (
	"fmt"
	"math/bits"

	"github.com/sarchlab/akita/v4/mem/vm"
)

const (
	// UVMRangeSize is the exact byte size of one UVM range flush command.
	UVMRangeSize = 64 * 1024
	// UVMRangePageSize is the page granularity of the valid-page mask.
	UVMRangePageSize = 4 * 1024
	// UVMRangeNumPages is the number of pages in one 64 KB region.
	UVMRangeNumPages = 16
	// UVMRangePageShift is the log2 of UVMRangePageSize.
	UVMRangePageShift = 12
)

// UVMRangeFlushPhase is the progress of a range flush inside a cache.
type UVMRangeFlushPhase int

const (
	// UVMRangeFlushDrain waits for matching in-flight cache/MSHR transactions.
	UVMRangeFlushDrain UVMRangeFlushPhase = iota
	// UVMRangeFlushWriteback pushes dirty matching lines to the write buffer.
	UVMRangeFlushWriteback
	// UVMRangeFlushWaitWriteback waits for the pushed writebacks to complete.
	UVMRangeFlushWaitWriteback
	// UVMRangeFlushFinalize invalidates or cleans matching lines and acks.
	UVMRangeFlushFinalize
)

// ValidateUVMCacheRangeFlushReq rejects structurally malformed range flush
// commands without mutating any cache state. The physical runs must be
// page-aligned, sorted, non-overlapping, and cover exactly the valid pages.
func ValidateUVMCacheRangeFlushReq(req *UVMCacheRangeFlushReq) error {
	if req.Operation != UVMCacheRangeFlushWritebackOnly &&
		req.Operation != UVMCacheRangeFlushWritebackInvalidate {
		return fmt.Errorf("unknown UVM range flush operation %d", req.Operation)
	}
	if req.VABase&(UVMRangeSize-1) != 0 {
		return fmt.Errorf("UVM range VABase %#x is not 64 KB aligned", req.VABase)
	}
	if req.ValidPageMask == 0 ||
		req.ValidPageMask&^(UVMRangeNumPages-1) != 0 {
		return fmt.Errorf("UVM range valid-page mask %#x is out of range",
			req.ValidPageMask)
	}
	if len(req.PhysicalRuns) == 0 {
		return fmt.Errorf("UVM range flush carries no physical runs")
	}

	var prevEnd uint64
	var total uint64
	for i, run := range req.PhysicalRuns {
		if run.Length == 0 {
			return fmt.Errorf("UVM range run %d is empty", i)
		}
		if run.Start&(UVMRangePageSize-1) != 0 {
			return fmt.Errorf("UVM range run %d start %#x is not page aligned",
				i, run.Start)
		}
		if run.Length&(UVMRangePageSize-1) != 0 {
			return fmt.Errorf("UVM range run %d length %#x is not page aligned",
				i, run.Length)
		}
		if i > 0 && run.Start < prevEnd {
			return fmt.Errorf("UVM range runs overlap or are unsorted")
		}
		prevEnd = run.Start + run.Length
		total += run.Length
	}

	want := uint64(bits.OnesCount64(req.ValidPageMask)) * UVMRangePageSize
	if total != want {
		return fmt.Errorf(
			"UVM range physical runs cover %d bytes, want %d for the valid pages",
			total, want)
	}

	return nil
}

// UVMRangeMatcher decides whether a cached line, MSHR entry, in-flight access,
// or in-flight writeback belongs to a UVM range flush command.
type UVMRangeMatcher struct {
	req     *UVMCacheRangeFlushReq
	virtual bool

	validPage  [UVMRangeNumPages]bool
	expectedPA [UVMRangeNumPages]uint64
}

// NewUVMRangeMatcher builds the matcher for a validated command. virtual
// selects PID+VA matching with stored-PA validation; otherwise matching is by
// physical runs.
func NewUVMRangeMatcher(req *UVMCacheRangeFlushReq, virtual bool) *UVMRangeMatcher {
	m := &UVMRangeMatcher{req: req, virtual: virtual}
	for i := 0; i < UVMRangeNumPages; i++ {
		if req.ValidPageMask&(1<<i) != 0 {
			m.validPage[i] = true
		}
	}
	m.computeExpectedPA()
	return m
}

// computeExpectedPA maps every valid page to the physical address the command
// assigns it: the runs encode the page mappings as maximal contiguous runs, so
// the i-th valid page in VA order maps to the i-th 4 KB chunk of the runs.
func (m *UVMRangeMatcher) computeExpectedPA() {
	runIdx := 0
	runOffset := uint64(0)
	for i := 0; i < UVMRangeNumPages; i++ {
		if !m.validPage[i] {
			continue
		}
		for runIdx < len(m.req.PhysicalRuns) &&
			runOffset >= m.req.PhysicalRuns[runIdx].Length {
			runIdx++
			runOffset = 0
		}
		if runIdx >= len(m.req.PhysicalRuns) {
			m.validPage[i] = false
			continue
		}
		m.expectedPA[i] = m.req.PhysicalRuns[runIdx].Start + runOffset
		runOffset += UVMRangePageSize
	}
}

func (m *UVMRangeMatcher) pageIndex(addr uint64) (int, bool) {
	if addr < m.req.VABase || addr >= m.req.VABase+UVMRangeSize {
		return 0, false
	}
	idx := int((addr - m.req.VABase) >> UVMRangePageShift)
	return idx, true
}

// MatchBlock reports whether a cached line belongs to the range.
func (m *UVMRangeMatcher) MatchBlock(block *Block) bool {
	if !block.IsValid {
		return false
	}
	if m.virtual {
		return m.matchVirtual(block.PID, block.Tag, block.Annotation)
	}
	return m.matchPhysical(block.Tag)
}

// MatchMSHR reports whether a pending refill entry belongs to the range.
func (m *UVMRangeMatcher) MatchMSHR(entry *MSHREntry) bool {
	if m.virtual {
		return m.matchVirtual(entry.PID, entry.Address, entry.Annotation)
	}
	return m.matchPhysical(entry.Address)
}

// MatchAccess reports whether an in-flight access belongs to the range.
func (m *UVMRangeMatcher) MatchAccess(
	pid vm.PID,
	addr uint64,
	ann *VirtualAccessAnnotation,
) bool {
	if m.virtual {
		return m.matchVirtual(pid, addr, ann)
	}
	return m.matchPhysical(addr)
}

// MatchWriteback reports whether an in-flight write buffer writeback belongs
// to the range.
func (m *UVMRangeMatcher) MatchWriteback(
	pid vm.PID,
	addr uint64,
	ann *VirtualAccessAnnotation,
) bool {
	if m.virtual {
		return m.matchVirtual(pid, addr, ann)
	}
	return m.matchPhysical(addr)
}

// MatchEvictingTag reports whether an in-flight eviction of the line at the
// given tag belongs to the range. Virtual caches match by VA range and valid
// page only: the eviction registry does not carry the PID or annotation.
func (m *UVMRangeMatcher) MatchEvictingTag(addr uint64) bool {
	if m.virtual {
		idx, ok := m.pageIndex(addr)
		return ok && m.validPage[idx]
	}
	return m.matchPhysical(addr)
}

// PagePA returns the physical address the command assigns to the page that
// contains the given virtual address, and whether the page is valid.
func (m *UVMRangeMatcher) PagePA(addr uint64) (uint64, bool) {
	idx, ok := m.pageIndex(addr)
	if !ok || !m.validPage[idx] {
		return 0, false
	}
	return m.expectedPA[idx], true
}

func (m *UVMRangeMatcher) matchPhysical(addr uint64) bool {
	for _, run := range m.req.PhysicalRuns {
		if addr >= run.Start && addr < run.Start+run.Length {
			return true
		}
	}
	return false
}

func (m *UVMRangeMatcher) matchVirtual(
	pid vm.PID,
	addr uint64,
	ann *VirtualAccessAnnotation,
) bool {
	if pid != m.req.PID {
		return false
	}
	idx, ok := m.pageIndex(addr)
	if !ok || !m.validPage[idx] {
		return false
	}
	if ann == nil || ann.PID != pid {
		return false
	}
	if ann.VAPage != addr&^(UVMRangePageSize-1) {
		return false
	}
	return ann.HBMPA == m.expectedPA[idx]
}
