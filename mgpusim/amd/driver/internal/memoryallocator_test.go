package internal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
)

var _ = Describe("MemoryAllocatorImpl", func() {

	var (
		allocator     *memoryAllocatorImpl
		cpuPageTable  vm.PageTable
		gpuPageTables []vm.PageTable // sbin_codex: real per-GPU tables expose synchronization behavior.
	)

	BeforeEach(func() {
		cpuPageTable = vm.NewPageTable(12) // sbin_codex
		allocator = NewMemoryAllocator(cpuPageTable, 12).(*memoryAllocatorImpl)
		configAFourGPUSystem(allocator)

		gpuPageTables = make([]vm.PageTable, 4) // sbin_codex
		for i := range gpuPageTables {
			gpuPageTables[i] = vm.NewPageTable(12)
			allocator.RegisterPageTable(i+1, gpuPageTables[i]) // sbin_codex
		}
	})

	It("should mirror allocated pages to CPU and every GPU page table", func() { // sbin_codex
		// Given: a driver allocator with one CPU and four registered GPU tables. // sbin_codex

		// When: a private page is allocated on GPU 1. // sbin_codex
		ptr := allocator.Allocate(1, 8, 1)

		// Then: every table contains the same mapping. // sbin_codex
		Expect(ptr).To(Equal(uint64(4096)))
		expectPageInAllTables(cpuPageTable, gpuPageTables, vm.Page{
			PID:      1,
			PAddr:    0x1_0000_1000,
			VAddr:    4096,
			PageSize: 4096,
			DeviceID: 1,
			Valid:    true,
		})
	})

	It("should mirror unified pages to CPU and every GPU page table", func() { // sbin_codex
		// Given: a driver allocator with one CPU and four registered GPU tables. // sbin_codex

		// When: unified memory is allocated. // sbin_codex
		ptr := allocator.AllocateUnified(1, 8)

		// Then: every table contains the unified mapping. // sbin_codex
		Expect(ptr).To(Equal(uint64(4096)))
		expectPageInAllTables(cpuPageTable, gpuPageTables, vm.Page{
			PID:      1,
			PAddr:    0x1_0000_1000,
			VAddr:    4096,
			PageSize: 4096,
			DeviceID: 1,
			Valid:    true,
			Unified:  true,
		})
	})

	It("should mirror every page in a multi-page allocation", func() { // sbin_codex
		// Given: a driver allocator with one CPU and four registered GPU tables. // sbin_codex

		// When: an allocation spans three pages. // sbin_codex
		ptr := allocator.Allocate(1, 8196, 1)

		// Then: every page is present in every table. // sbin_codex
		Expect(ptr).To(Equal(uint64(4096)))
		for i := uint64(0); i < 3; i++ {
			expectPageInAllTables(cpuPageTable, gpuPageTables, vm.Page{
				PID:      1,
				PAddr:    0x1_0000_1000 + 0x1000*i,
				VAddr:    4096 + 0x1000*i,
				DeviceID: 1,
				PageSize: 4096,
				Valid:    true,
			})
		}
	})

	It("should update CPU and every GPU page table on remap", func() { // sbin_codex
		// Given: a private page allocated on GPU 1. // sbin_codex
		page := vm.Page{
			PID:      1,
			PAddr:    0x1_0000_1000,
			VAddr:    4096,
			PageSize: 4096,
			DeviceID: 1,
			Valid:    true,
		}
		ptr := allocator.Allocate(1, 4000, 1)

		// When: the page is remapped to GPU 2. // sbin_codex
		updatedPage := page
		updatedPage.PAddr = 0x2_0000_1000
		updatedPage.DeviceID = 2
		allocator.Remap(1, ptr, 4000, 2)

		// Then: every table exposes the GPU 2 mapping. // sbin_codex
		expectPageInAllTables(cpuPageTable, gpuPageTables, updatedPage)
	})

	It("should update CPU and every GPU page table for driver state changes", func() { // sbin_codex
		// Given: a page mirrored into every page table. // sbin_codex
		ptr := allocator.Allocate(1, 8, 1)
		page, found := cpuPageTable.Find(1, ptr)
		Expect(found).To(BeTrue())

		// When: the driver marks the page as migrating. // sbin_codex
		page.IsMigrating = true
		allocator.UpdatePage(page)

		// Then: every table exposes the updated page state. // sbin_codex
		expectPageInAllTables(cpuPageTable, gpuPageTables, page)
	})

	It("should remove freed pages from CPU and every GPU page table", func() { // sbin_codex
		// Given: a page mirrored into every page table. // sbin_codex
		ptr := allocator.Allocate(1, 8, 1)

		// When: the page is freed. // sbin_codex
		allocator.Free(ptr)

		// Then: no table retains the mapping. // sbin_codex
		_, found := cpuPageTable.Find(1, ptr)
		Expect(found).To(BeFalse())
		for _, gpuPageTable := range gpuPageTables {
			_, found = gpuPageTable.Find(1, ptr)
			Expect(found).To(BeFalse())
		}
	})
})

// expectPageInAllTables verifies the shared driver page-table contract. // sbin_codex
func expectPageInAllTables(
	cpuPageTable vm.PageTable,
	gpuPageTables []vm.PageTable,
	expected vm.Page,
) {
	page, found := cpuPageTable.Find(expected.PID, expected.VAddr)
	Expect(found).To(BeTrue())
	Expect(page).To(Equal(expected))

	for _, gpuPageTable := range gpuPageTables {
		page, found = gpuPageTable.Find(expected.PID, expected.VAddr)
		Expect(found).To(BeTrue())
		Expect(page).To(Equal(expected))
	}
}

func configAFourGPUSystem(allocator *memoryAllocatorImpl) {
	cpu := &Device{
		ID:       0,
		Type:     DeviceTypeCPU,
		MemState: NewDeviceMemoryState(12),
	}
	cpu.SetTotalMemSize(0x1_0000_0000)
	allocator.RegisterDevice(cpu)

	for i := 0; i < 4; i++ { // 5 devices = 1 CPU + 4 GPUs
		gpu := &Device{
			ID:       i + 1,
			Type:     DeviceTypeGPU,
			MemState: NewDeviceMemoryState(12),
		}
		gpu.SetTotalMemSize(0x1_0000_0000)
		allocator.RegisterDevice(gpu)
	}
}
