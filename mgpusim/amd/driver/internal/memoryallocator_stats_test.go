// sbin_codex: regression coverage for allocator footprint accounting.
package internal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
)

var _ = Describe("MemoryAllocatorStats", func() {
	It("tracks live, peak, and cumulative physical allocation", func() {
		pageTable := vm.NewPageTable(12)
		allocator := NewMemoryAllocator(pageTable, 12).(*memoryAllocatorImpl)
		configAFourGPUSystem(allocator)

		ptr := allocator.Allocate(1, 8193, 1)
		stats := allocator.GetMemoryStats()

		Expect(ptr).To(Equal(uint64(4096)))
		Expect(stats.LivePageCount).To(Equal(uint64(3)))
		Expect(stats.PeakPageCount).To(Equal(uint64(3)))
		Expect(stats.TotalPageCount).To(Equal(uint64(3)))
		Expect(stats.LiveBytes).To(Equal(uint64(3 * 4096)))
		Expect(stats.PeakBytes).To(Equal(uint64(3 * 4096)))

		allocator.Free(ptr)
		allocator.Free(ptr + 4096)
		allocator.Free(ptr + 8192)
		stats = allocator.GetMemoryStats()

		Expect(stats.LivePageCount).To(Equal(uint64(0)))
		Expect(stats.PeakPageCount).To(Equal(uint64(3)))
		Expect(stats.TotalPageCount).To(Equal(uint64(3)))
	})
})
