// sbin_codex: expose allocator footprint statistics to the runner.
package driver

import "github.com/sarchlab/mgpusim/v4/amd/driver/internal"

// MemoryStats is an alias for the allocator snapshot exposed by Driver.
type MemoryStats = internal.MemoryStats

// MemoryStats returns the physical memory footprint managed by the driver.
// The snapshot includes pages allocated for migration and therefore describes
// physical allocator usage rather than only user-visible allocation requests.
func (d *Driver) MemoryStats() MemoryStats {
	provider, ok := d.memAllocator.(internal.MemoryStatsProvider)
	if !ok {
		return MemoryStats{PageSize: uint64(1 << d.Log2PageSize)}
	}
	return provider.GetMemoryStats()
}
