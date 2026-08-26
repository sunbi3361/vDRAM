// sbin_claude_utopia
package internal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
)

var _ = Describe("Utopia RestSeg allocation", func() {
	var (
		allocator    *memoryAllocatorImpl
		cpuPageTable vm.PageTable
		gpuPageTable vm.PageTable
		registry     *restseg.Registry
		cfg          restseg.Config
	)

	const (
		pageSize     = uint64(4096)
		restSegBytes = uint64(1 << 20) // 1MB -> 256 frames
		assoc        = 4
	)

	BeforeEach(func() {
		cpuPageTable = vm.NewPageTable(12)
		allocator = NewMemoryAllocator(cpuPageTable, 12).(*memoryAllocatorImpl)

		cpu := &Device{
			ID:       0,
			Type:     DeviceTypeCPU,
			MemState: NewDeviceMemoryState(12),
		}
		cpu.SetTotalMemSize(1 << 30)
		allocator.RegisterDevice(cpu)

		gpu := &Device{
			ID:       1,
			Type:     DeviceTypeGPU,
			MemState: NewDeviceMemoryState(12),
		}
		gpu.SetTotalMemSize(1 << 30)
		allocator.RegisterDevice(gpu)

		gpuPageTable = vm.NewPageTable(12)
		allocator.RegisterPageTable(1, gpuPageTable)

		registry = restseg.NewRegistry()
		allocator.SetUtopiaRegistry(registry)
		cfg = allocator.ReserveRestSeg(1, restSegBytes, assoc)
	})

	It("should reserve the leading contiguous frames as the RestSeg", func() {
		// Given: a GPU whose frame pool starts at its initial address.
		gpuBase := allocator.devices[1].MemState.getInitialAddress()

		// Then: the RestSeg covers the leading region and the next FlexSeg
		// frame is the first frame after the RestSeg.
		Expect(cfg.BasePAddr).To(Equal(gpuBase))
		Expect(cfg.SegmentBytes).To(Equal(restSegBytes))
		Expect(cfg.NumSets).To(Equal(int(restSegBytes/pageSize) / assoc))

		nextFlexFrame := allocator.devices[1].MemState.popNextAvailablePAddrs()
		Expect(nextFlexFrame).To(Equal(gpuBase + restSegBytes))
	})

	It("should place pages into the hashed RestSeg set and keep them out of "+
		"the GPU page table", func() {
		// When: a page is allocated on the GPU.
		vAddr := allocator.Allocate(1, 8, 1)

		// Then: the frame is the RestSeg frame of the hashed set, the CPU
		// table has the mapping, and the GPU table does not (utopia.md 4.9).
		page, found := cpuPageTable.Find(1, vAddr)
		Expect(found).To(BeTrue())
		Expect(cfg.Contains(page.PAddr)).To(BeTrue())

		set, _, ok := cfg.SetWayOf(page.PAddr)
		Expect(ok).To(BeTrue())
		Expect(set).To(Equal(cfg.SetOf(vAddr)))

		_, foundInGPU := gpuPageTable.Find(1, vAddr)
		Expect(foundInGPU).To(BeFalse())

		// And: the authoritative TAR resolves the page.
		pAddr, resolved := registry.Lookup(1, vm.PID(1), vAddr)
		Expect(resolved).To(BeTrue())
		Expect(pAddr).To(Equal(page.PAddr))
	})

	It("should fall back to FlexSeg when the hashed set is full", func() {
		// Given: enough allocations to overflow at least one set. With N
		// sets and M ways, N*M+N pages guarantee a full set is re-hit.
		numPages := cfg.NumFrames() + cfg.NumSets

		vAddr := allocator.Allocate(1, uint64(numPages)*pageSize, 1)

		flexPages := 0
		for i := 0; i < numPages; i++ {
			page, found := cpuPageTable.Find(1, vAddr+uint64(i)*pageSize)
			Expect(found).To(BeTrue())

			if cfg.Contains(page.PAddr) {
				// RestSeg page: TAR-mapped only.
				_, inGPU := gpuPageTable.Find(1, page.VAddr)
				Expect(inGPU).To(BeFalse())
			} else {
				// FlexSeg page: normal page-table mapping in every table.
				flexPages++
				gpuPage, inGPU := gpuPageTable.Find(1, page.VAddr)
				Expect(inGPU).To(BeTrue())
				Expect(gpuPage.PAddr).To(Equal(page.PAddr))
			}
		}

		// Then: overflowing pages went to FlexSeg (utopia.md 4.8).
		Expect(flexPages).To(BeNumerically(">", 0))
		Expect(flexPages + registry.OccupiedFrames(1)).To(Equal(numPages))
	})

	It("should return freed RestSeg frames to the TAR, not the frame pool", func() {
		vAddr := allocator.Allocate(1, 8, 1)
		page, _ := cpuPageTable.Find(1, vAddr)
		Expect(cfg.Contains(page.PAddr)).To(BeTrue())

		allocator.Free(vAddr)

		// Then: the TAR way is free again and the CPU mapping is gone.
		Expect(registry.IsResident(vm.PID(1), vAddr)).To(BeFalse())
		Expect(registry.OccupiedFrames(1)).To(Equal(0))
		_, found := cpuPageTable.Find(1, vAddr)
		Expect(found).To(BeFalse())

		// And: the released capacity is usable by a following allocation.
		vAddr2 := allocator.Allocate(1, 8, 1)
		page2, _ := cpuPageTable.Find(1, vAddr2)
		Expect(cfg.Contains(page2.PAddr)).To(BeTrue())
	})
})
