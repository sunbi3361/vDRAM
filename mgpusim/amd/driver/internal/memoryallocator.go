// Package internal provides support for the driver implementation.
package internal

import (
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// A MemoryAllocator can allocate memory on the CPU and GPUs
type MemoryAllocator interface {
	RegisterDevice(device *Device)
	RegisterPageTable(deviceID int, pageTable vm.PageTable) // sbin_codex: register a GPU-owned page table.
	GetDeviceIDByPAddr(pAddr uint64) int
	Allocate(pid vm.PID, byteSize uint64, deviceID int) uint64
	AllocateUnified(pid vm.PID, byteSize uint64) uint64
	Free(vAddr uint64)
	Remap(pid vm.PID, pageVAddr, byteSize uint64, deviceID int)
	RemovePage(vAddr uint64)
	UpdatePage(page vm.Page) // sbin_codex: update CPU and GPU page tables under the allocator lock.
	AllocatePageWithGivenVAddr(
		pid vm.PID,
		deviceID int,
		vAddr uint64,
		unified bool,
	) vm.Page

	AllocateManaged(pid vm.PID, byteSize uint64) ManagedAllocationResult // sbin_uvm: allocate managed memory.
}

// NewMemoryAllocator creates a new memory allocator.
func NewMemoryAllocator(
	pageTable vm.PageTable,
	log2PageSize uint64,
) MemoryAllocator {
	a := &memoryAllocatorImpl{
		pageTable:            pageTable,
		totalStorageByteSize: 1 << log2PageSize, // Starting with a page to avoid 0 address.
		log2PageSize:         log2PageSize,
		processMemoryStates:  make(map[vm.PID]*processMemoryState),
		vAddrToPageMapping:   make(map[uint64]vm.Page),
		devices:              make(map[int]*Device),
		gpuPageTables:        make(map[int]vm.PageTable), // sbin_codex
	}
	return a
}

type processMemoryState struct {
	pid       vm.PID
	nextVAddr uint64
}

// sbin_uvm
// managedAllocationResult describes a managed allocation created by
// AllocateManaged. It carries the virtual range and per-page CPU backing
// frames so the driver UVM manager can register residency metadata.
type ManagedAllocationResult struct {
	Base            uint64
	Size            uint64
	PageCount       uint64
	PageSize        uint64
	CPUBackingPages []uint64
	PIDs            []vm.PID
}

// A memoryAllocatorImpl provides the default implementation for
// memoryAllocator
type memoryAllocatorImpl struct {
	sync.Mutex
	pageTable            vm.PageTable
	log2PageSize         uint64
	vAddrToPageMapping   map[uint64]vm.Page
	processMemoryStates  map[vm.PID]*processMemoryState
	devices              map[int]*Device
	gpuPageTables        map[int]vm.PageTable // sbin_codex: device ID to GPU page table.
	totalStorageByteSize uint64
	livePageCount        uint64 // sbin_codex: physical footprint tracer state.
	peakPageCount        uint64 // sbin_codex: physical footprint tracer state.
	totalPageCount       uint64 // sbin_codex: physical footprint tracer state.
}

func (a *memoryAllocatorImpl) RegisterDevice(device *Device) {
	a.Lock()
	defer a.Unlock()

	state := device.MemState
	state.setInitialAddress(a.totalStorageByteSize)

	a.totalStorageByteSize += state.getStorageSize()

	a.devices[device.ID] = device
}

// RegisterPageTable registers a GPU page table for subsequent page mutations.
// The v4 GMMU cannot fault to the CPU table, so every GPU table receives every
// mapping while remaining a distinct page-table instance. // sbin_codex
func (a *memoryAllocatorImpl) RegisterPageTable(
	deviceID int,
	pageTable vm.PageTable,
) {
	a.Lock()
	defer a.Unlock()

	a.gpuPageTables[deviceID] = pageTable
}

// insertPage inserts the same mapping into the CPU table and every GPU table.
// sbin_codex
func (a *memoryAllocatorImpl) insertPage(page vm.Page) {
	a.pageTable.Insert(page)
	for _, gpuPageTable := range a.gpuPageTables {
		gpuPageTable.Insert(page)
	}
}

// updatePage updates the same mapping in the CPU table and every GPU table.
// sbin_codex
func (a *memoryAllocatorImpl) updatePage(page vm.Page) {
	a.pageTable.Update(page)
	for _, gpuPageTable := range a.gpuPageTables {
		gpuPageTable.Update(page)
	}
}

// removePageFromTables removes a mapping from the CPU and every GPU table.
// sbin_codex
func (a *memoryAllocatorImpl) removePageFromTables(page vm.Page) {
	a.pageTable.Remove(page.PID, page.VAddr)
	for _, gpuPageTable := range a.gpuPageTables {
		gpuPageTable.Remove(page.PID, page.VAddr)
	}
}

func (a *memoryAllocatorImpl) GetDeviceIDByPAddr(pAddr uint64) int {
	a.Lock()
	defer a.Unlock()

	return a.deviceIDByPAddr(pAddr)
}

func (a *memoryAllocatorImpl) deviceIDByPAddr(pAddr uint64) int {
	for id, dev := range a.devices {
		state := dev.MemState
		if isPAddrOnDevice(pAddr, state) {
			return id
		}
	}

	panic("device not found")
}

func isPAddrOnDevice(
	pAddr uint64,
	state DeviceMemoryState,
) bool {
	return pAddr >= state.getInitialAddress() &&
		pAddr < state.getInitialAddress()+state.getStorageSize()
}

func (a *memoryAllocatorImpl) Allocate(
	pid vm.PID,
	byteSize uint64,
	deviceID int,
) uint64 {
	if byteSize == 0 {
		panic("Allocating 0 bytes.")
	}

	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePages(int(numPages), pid, deviceID, false)
}

func (a *memoryAllocatorImpl) AllocateUnified(
	pid vm.PID,
	byteSize uint64,
) uint64 {
	if byteSize == 0 {
		panic("Allocating 0 bytes.")
	}

	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePages(int(numPages), pid, 1, true)
}

func (a *memoryAllocatorImpl) allocatePages(
	numPages int,
	pid vm.PID,
	deviceID int,
	unified bool,
) (firstPageVAddr uint64) {
	pState, found := a.processMemoryStates[pid]
	if !found {
		a.processMemoryStates[pid] = &processMemoryState{
			pid:       pid,
			nextVAddr: uint64(1 << a.log2PageSize),
		}
		pState = a.processMemoryStates[pid]
	}
	device := a.devices[deviceID]

	pageSize := uint64(1 << a.log2PageSize)
	nextVAddr := pState.nextVAddr

	for i := 0; i < numPages; i++ {
		pAddr := device.allocatePage()
		vAddr := nextVAddr + uint64(i)*pageSize

		page := vm.Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddr,
			PageSize: pageSize,
			Valid:    true,
			Unified:  unified,
			DeviceID: uint64(a.deviceIDByPAddr(pAddr)),
		}

		// fmt.Printf("page.addr is %x piage Device ID is %d \n", page.PAddr, page.DeviceID)
		// debug.PrintStack()
		a.insertPage(page) // sbin_codex: driver owns CPU/GPU page-table synchronization.
		a.vAddrToPageMapping[page.VAddr] = page
	}
	a.recordAllocation(numPages) // sbin_codex: update physical footprint counters.

	pState.nextVAddr += pageSize * uint64(numPages)

	return nextVAddr
}

// sbin_uvm: AllocateManaged allocates a chunck of managed memory.
// Allocation is done on CPU and can be migrated to GPU.
func (a *memoryAllocatorImpl) AllocateManaged(
	pid vm.PID,
	byteSize uint64,
) ManagedAllocationResult {
	if byteSize == 0 {
		panic("Allocating 0 bytes.")
	}

	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocateManagedPages(int(numPages), byteSize, pid, 0)
}

// sbin_uvm
func (a *memoryAllocatorImpl) allocateManagedPages(
	numPages int,
	byteSize uint64,
	pid vm.PID,
	deviceID int,
) ManagedAllocationResult {
	pState, found := a.processMemoryStates[pid]
	if !found {
		a.processMemoryStates[pid] = &processMemoryState{
			pid:       pid,
			nextVAddr: uint64(1 << a.log2PageSize),
		}
		pState = a.processMemoryStates[pid]
	}
	device := a.devices[deviceID]

	pageSize := uint64(1 << a.log2PageSize)
	nextVAddr := pState.nextVAddr

	result := ManagedAllocationResult{
		Base:            nextVAddr,
		Size:            byteSize,
		PageCount:       uint64(numPages),
		PageSize:        pageSize,
		CPUBackingPages: make([]uint64, 0, numPages),
		PIDs:            make([]vm.PID, 0, numPages),
	}

	for i := 0; i < numPages; i++ {
		vAddr := nextVAddr + uint64(i)*pageSize
		cpuPAddr := device.allocatePage()

		page := vm.Page{
			DeviceID:    uint64(0), // sbin_uvm: managed memory is allocated on CPU.
			PID:         pid,
			VAddr:       vAddr,
			PAddr:       cpuPAddr,
			PageSize:    pageSize,
			Valid:       true,
			Unified:     false,
			Managed:     true, // sbin_uvm: managed memory is marked as managed.
			IsPinned:    false,
			IsMigrating: false,
		}

		// fmt.Printf("page.addr is %x piage Device ID is %d \n", page.PAddr, page.DeviceID)
		// debug.PrintStack()
		a.insertPage(page) // sbin_codex: driver owns CPU/GPU page-table synchronization.
		a.vAddrToPageMapping[page.VAddr] = page
		result.CPUBackingPages = append(result.CPUBackingPages, cpuPAddr)
		result.PIDs = append(result.PIDs, pid)
	}
	a.recordAllocation(numPages) // sbin_codex: update physical footprint counters.

	pState.nextVAddr += pageSize * uint64(numPages)

	return result
}

func (a *memoryAllocatorImpl) Remap(
	pid vm.PID,
	pageVAddr, byteSize uint64,
	deviceID int,
) {
	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	addr := pageVAddr
	vAddrs := make([]uint64, 0)
	for addr < pageVAddr+byteSize {
		vAddrs = append(vAddrs, addr)
		addr += pageSize
	}

	a.allocateMultiplePagesWithGivenVAddrs(pid, deviceID, vAddrs, false)
}

func (a *memoryAllocatorImpl) RemovePage(vAddr uint64) {
	a.Lock()
	defer a.Unlock()

	a.removePage(vAddr)
}

func (a *memoryAllocatorImpl) removePage(vAddr uint64) {
	page, ok := a.vAddrToPageMapping[vAddr]

	if !ok {
		panic("page not found")
	}

	deviceID := a.deviceIDByPAddr(page.PAddr)
	dState := a.devices[deviceID].MemState
	dState.addSinglePAddr(page.PAddr)
	a.recordFree()                      // sbin_codex: update physical footprint counters.
	delete(a.vAddrToPageMapping, vAddr) // sbin_codex: keep live-page accounting accurate.

	a.removePageFromTables(page) // sbin_codex
}

// UpdatePage updates a page's runtime state in every driver-managed table.
// sbin_codex
func (a *memoryAllocatorImpl) UpdatePage(page vm.Page) {
	a.Lock()
	defer a.Unlock()

	a.vAddrToPageMapping[page.VAddr] = page
	a.updatePage(page)
}

func (a *memoryAllocatorImpl) AllocatePageWithGivenVAddr(
	pid vm.PID,
	deviceID int,
	vAddr uint64,
	isUnified bool,
) vm.Page {
	a.Lock()
	defer a.Unlock()

	return a.allocatePageWithGivenVAddr(pid, deviceID, vAddr, isUnified)
}

func (a *memoryAllocatorImpl) allocatePageWithGivenVAddr(
	pid vm.PID,
	deviceID int,
	vAddr uint64,
	isUnified bool,
) vm.Page {
	pageSize := uint64(1 << a.log2PageSize)

	device := a.devices[deviceID]
	pAddr := device.allocatePage()

	page := vm.Page{
		PID:      pid,
		VAddr:    vAddr,
		PAddr:    pAddr,
		PageSize: pageSize,
		Valid:    true,
		DeviceID: uint64(deviceID),
		Unified:  isUnified,
	}
	a.vAddrToPageMapping[page.VAddr] = page
	a.updatePage(page)    // sbin_codex
	a.recordAllocation(1) // sbin_codex: migration allocation contributes to footprint.

	return page
}

func (a *memoryAllocatorImpl) allocateMultiplePagesWithGivenVAddrs(
	pid vm.PID,
	deviceID int,
	vAddrs []uint64,
	isUnified bool,
) (pages []vm.Page) {
	pageSize := uint64(1 << a.log2PageSize)

	device := a.devices[deviceID]
	pAddrs := device.allocateMultiplePages(len(vAddrs))

	for i, vAddr := range vAddrs {
		page := vm.Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddrs[i],
			PageSize: pageSize,
			Valid:    true,
			DeviceID: uint64(deviceID),
			Unified:  isUnified,
		}
		a.vAddrToPageMapping[page.VAddr] = page
		a.updatePage(page) // sbin_codex
		pages = append(pages, page)
	}
	a.recordAllocation(len(vAddrs)) // sbin_codex: remap allocations contribute to footprint.

	return pages
}

func (a *memoryAllocatorImpl) Free(ptr uint64) {
	a.Lock()
	defer a.Unlock()

	a.removePage(ptr)
}
