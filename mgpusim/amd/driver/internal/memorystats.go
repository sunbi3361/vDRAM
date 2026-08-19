// sbin_codex: physical memory footprint statistics for the allocator.
package internal

// MemoryStats is a snapshot of physical pages managed by the allocator.
//
// The counters describe physical allocation activity, including pages created
// during migration. They are intentionally independent of the virtual address
// map so remapping and page-table updates do not hide physical usage.
type MemoryStats struct {
	PageSize uint64

	LivePageCount  uint64
	PeakPageCount  uint64
	TotalPageCount uint64

	LiveBytes  uint64
	PeakBytes  uint64
	TotalBytes uint64
}

// MemoryStatsProvider exposes allocator statistics without expanding the
// MemoryAllocator interface used by generated driver test doubles.
type MemoryStatsProvider interface {
	GetMemoryStats() MemoryStats
}

func (a *memoryAllocatorImpl) recordAllocation(numPages int) {
	if numPages <= 0 {
		return
	}

	pages := uint64(numPages)
	a.livePageCount += pages
	a.totalPageCount += pages
	if a.livePageCount > a.peakPageCount {
		a.peakPageCount = a.livePageCount
	}
}

func (a *memoryAllocatorImpl) recordFree() {
	if a.livePageCount > 0 {
		a.livePageCount--
	}
}

// GetMemoryStats returns a point-in-time snapshot of allocator usage.
func (a *memoryAllocatorImpl) GetMemoryStats() MemoryStats {
	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	return MemoryStats{
		PageSize:       pageSize,
		LivePageCount:  a.livePageCount,
		PeakPageCount:  a.peakPageCount,
		TotalPageCount: a.totalPageCount,
		LiveBytes:      a.livePageCount * pageSize,
		PeakBytes:      a.peakPageCount * pageSize,
		TotalBytes:     a.totalPageCount * pageSize,
	}
}
