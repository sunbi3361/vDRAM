// Package internal provides support for the driver implementation.
package internal

import (
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"    // sbin_claude_avatar: shared Avatar metadata/placement state.
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg" // sbin_claude_utopia: shared RestSeg layout/state.
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

	// sbin_codex: UVM demand-paging allocator APIs.
	// AllocateManaged reserves a managed virtual range backed by CPU frames and
	// inserts Managed PTEs with DeviceID=0. No GPU frames are allocated.
	AllocateManaged(pid vm.PID, byteSize uint64) ManagedAllocationResult
	// TryAllocatePhysicalPage allocates one physical frame on a device.
	TryAllocatePhysicalPage(deviceID int) (uint64, bool)
	// FreePhysicalPage returns a physical frame to a device.
	FreePhysicalPage(deviceID int, pAddr uint64)

	// sbin_claude_utopia: Utopia RestSeg allocator APIs.
	// SetUtopiaRegistry attaches the shared authoritative RestSeg state. When
	// set, allocations on devices that own a RestSeg try the hashed set first
	// and fall back to FlexSeg (utopia.md 4.8).
	SetUtopiaRegistry(registry *restseg.Registry)
	// ReserveRestSeg carves a contiguous RestSeg region out of a device's
	// frame pool so the normal allocator can never hand out RestSeg frames
	// (RestSeg XOR FlexSeg invariant, utopia.md 4.9).
	ReserveRestSeg(deviceID int, bytes uint64, associativity int) restseg.Config

	// sbin_claude_avatar: Avatar metadata/placement allocator APIs.
	// SetAvatarRegistry attaches the shared authoritative Avatar state.
	// When set, page mutations install/invalidate embedded page metadata;
	// with fragEnabled, GPU frame placement additionally goes through the
	// 2MB-region randomized allocator (avatar-plan.md 1.3, 1.4).
	SetAvatarRegistry(registry *meta.Registry, fragEnabled bool)
	// ReserveAvatarRegions hands a device's whole fresh frame pool to the
	// 2MB-region allocator so the default sequential pool can never hand
	// out the same frame again.
	ReserveAvatarRegions(deviceID int)
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

// ManagedAllocationResult describes a managed allocation created by
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

	// sbin_claude_utopia: authoritative RestSeg state shared with the GPU-side
	// RestSeg walker. Nil when Utopia is disabled.
	utopiaRegistry *restseg.Registry

	// sbin_claude_avatar: authoritative Avatar metadata/placement state
	// shared with the GPU-side ASU. Nil when Avatar is disabled.
	avatarRegistry    *meta.Registry
	avatarFragEnabled bool
}

// SetAvatarRegistry attaches the shared Avatar state. // sbin_claude_avatar
func (a *memoryAllocatorImpl) SetAvatarRegistry(
	registry *meta.Registry,
	fragEnabled bool,
) {
	a.Lock()
	defer a.Unlock()

	a.avatarRegistry = registry
	a.avatarFragEnabled = fragEnabled
}

// ReserveAvatarRegions drains a device's fresh frame pool into the Avatar
// 2MB-region allocator. It must run right after RegisterDevice, before any
// allocation on the device, so region placement and the default sequential
// pool can never hand out the same frame twice. // sbin_claude_avatar
func (a *memoryAllocatorImpl) ReserveAvatarRegions(deviceID int) {
	a.Lock()
	defer a.Unlock()

	if a.avatarRegistry == nil {
		panic("avatar: ReserveAvatarRegions requires a registry")
	}
	if MemoryAllocatorType != AllocatorTypeDefault {
		panic("avatar: region reservation supports the default allocator only")
	}

	device, found := a.devices[deviceID]
	if !found {
		panic("avatar: device not registered")
	}

	pageSize := uint64(1) << a.log2PageSize
	base := device.MemState.getInitialAddress()
	size := device.MemState.getStorageSize()

	for !device.MemState.noAvailablePAddrs() {
		device.MemState.popNextAvailablePAddrs()
	}

	a.avatarRegistry.RegisterDevice(deviceID, base, size, pageSize)
}

// tryAvatarAllocate places a page through the 2MB-region randomized
// allocator when the Avatar fragmentation model owns the device's frames.
// sbin_claude_avatar
func (a *memoryAllocatorImpl) tryAvatarAllocate(
	deviceID int,
	pid vm.PID,
	vAddr uint64,
) (pAddr uint64, ok bool) {
	if a.avatarRegistry == nil || !a.avatarFragEnabled {
		return 0, false
	}
	if !a.avatarRegistry.HasDevice(deviceID) {
		return 0, false
	}

	pAddr, ok = a.avatarRegistry.AllocateFrame(deviceID, pid, vAddr)
	if !ok {
		panic("avatar: 2MB region pool exhausted; " +
			"disable -avatar-frag or shrink the working set")
	}

	return pAddr, true
}

// avatarInstall records embedded page metadata for a fresh mapping.
// sbin_claude_avatar
func (a *memoryAllocatorImpl) avatarInstall(page vm.Page) {
	if a.avatarRegistry == nil {
		return
	}

	a.avatarRegistry.Install(page.PAddr, page.PID, page.VAddr)
}

// avatarOnUpdate keeps embedded page metadata coherent across a mapping
// update: the old physical location of a moved page must never validate a
// future mis-speculation (refs/avatar.md 5.11). // sbin_claude_avatar
func (a *memoryAllocatorImpl) avatarOnUpdate(
	old vm.Page,
	hadOld bool,
	page vm.Page,
) {
	if a.avatarRegistry == nil {
		return
	}

	if hadOld && old.PAddr != page.PAddr {
		a.avatarRegistry.Invalidate(old.PAddr)
	}

	if page.Valid {
		a.avatarRegistry.Install(page.PAddr, page.PID, page.VAddr)
	} else {
		a.avatarRegistry.Invalidate(page.PAddr)
	}
}

// SetUtopiaRegistry attaches the shared RestSeg registry. // sbin_claude_utopia
func (a *memoryAllocatorImpl) SetUtopiaRegistry(registry *restseg.Registry) {
	a.Lock()
	defer a.Unlock()

	a.utopiaRegistry = registry
}

// ReserveRestSeg pops the leading contiguous frames of a device out of the
// normal frame pool and registers them as a RestSeg. It must run right after
// RegisterDevice, before any allocation on the device. // sbin_claude_utopia
func (a *memoryAllocatorImpl) ReserveRestSeg(
	deviceID int,
	bytes uint64,
	associativity int,
) restseg.Config {
	a.Lock()
	defer a.Unlock()

	if a.utopiaRegistry == nil {
		panic("utopia: ReserveRestSeg requires a registry (SetUtopiaRegistry)")
	}
	if MemoryAllocatorType != AllocatorTypeDefault {
		panic("utopia: RestSeg reservation supports the default allocator only")
	}

	device, found := a.devices[deviceID]
	if !found {
		panic("utopia: device not registered")
	}

	pageSize := uint64(1) << a.log2PageSize
	base := device.MemState.getInitialAddress()
	cfg := restseg.MakeConfig(deviceID, base, bytes, pageSize, associativity)

	for i := 0; i < cfg.NumFrames(); i++ {
		pAddr := device.MemState.popNextAvailablePAddrs()
		if pAddr != base+uint64(i)*pageSize {
			panic("utopia: RestSeg reservation requires contiguous leading frames")
		}
	}

	a.utopiaRegistry.AddSegment(cfg)

	return cfg
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
	a.avatarInstall(page) // sbin_claude_avatar: embed page metadata.
	// sbin_claude_utopia: RestSeg pages are mapped by the TAR only. They must
	// never enter a GPU page table, or the FlexSeg walk would resolve them and
	// break the RestSeg XOR FlexSeg invariant (utopia.md 4.9). The CPU table
	// keeps the mapping for driver-functional paths (memcopy, CPU MMU).
	if a.isRestSegFrame(page.PAddr) {
		return
	}
	for _, gpuPageTable := range a.gpuPageTables {
		gpuPageTable.Insert(page)
	}
}

// updatePage updates the same mapping in the CPU table and every GPU table.
// sbin_codex
func (a *memoryAllocatorImpl) updatePage(page vm.Page) {
	// Pre-edit code (commented per project convention):
	// a.pageTable.Update(page)
	// for _, gpuPageTable := range a.gpuPageTables {
	// 	gpuPageTable.Update(page)
	// }
	//
	// sbin_claude_utopia: a page may change mapping domain on update. The GPU
	// tables only ever hold FlexSeg mappings, so domain transitions insert or
	// remove the GPU-side entry while the CPU table is always updated.
	a.pageTable.Update(page)

	wasRestSeg := a.utopiaRegistry != nil &&
		a.utopiaRegistry.IsResident(page.PID, page.VAddr)
	isRestSeg := a.isRestSegFrame(page.PAddr)

	switch {
	case wasRestSeg && isRestSeg:
		// Stays TAR-mapped; GPU tables never held it.
	case wasRestSeg && !isRestSeg:
		// RestSeg -> FlexSeg: release the TAR way and surface the mapping to
		// the GPU tables for the first time.
		a.utopiaRegistry.Release(page.PID, page.VAddr)
		for _, gpuPageTable := range a.gpuPageTables {
			gpuPageTable.Insert(page)
		}
	case !wasRestSeg && isRestSeg:
		// FlexSeg -> RestSeg (explicit migration): the GPU tables must forget
		// the FlexSeg mapping. The caller updates the TAR via the registry.
		for _, gpuPageTable := range a.gpuPageTables {
			gpuPageTable.Remove(page.PID, page.VAddr)
		}
	default:
		for _, gpuPageTable := range a.gpuPageTables {
			gpuPageTable.Update(page)
		}
	}
}

// removePageFromTables removes a mapping from the CPU and every GPU table.
// sbin_codex
func (a *memoryAllocatorImpl) removePageFromTables(page vm.Page) {
	a.pageTable.Remove(page.PID, page.VAddr)
	// sbin_claude_utopia: RestSeg pages never entered the GPU tables.
	if a.isRestSegFrame(page.PAddr) {
		return
	}
	for _, gpuPageTable := range a.gpuPageTables {
		gpuPageTable.Remove(page.PID, page.VAddr)
	}
}

// isRestSegFrame reports whether a physical address belongs to a reserved
// RestSeg region. // sbin_claude_utopia
func (a *memoryAllocatorImpl) isRestSegFrame(pAddr uint64) bool {
	return a.utopiaRegistry != nil && a.utopiaRegistry.Contains(pAddr)
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
		vAddr := nextVAddr + uint64(i)*pageSize

		// Pre-edit code (commented per project convention):
		// pAddr := device.allocatePage()
		//
		// sbin_claude_utopia: try the RestSeg first. The hashed set may have a
		// free way (allocation-time placement plays the role of the paper's
		// fault-based Mode A); a full set falls back to FlexSeg (utopia.md 4.8).
		pAddr, inRestSeg := a.tryRestSegAllocate(deviceID, pid, vAddr)
		if !inRestSeg {
			// Pre-edit code (commented per project convention):
			// pAddr = device.allocatePage()
			//
			// sbin_claude_avatar: with the fragmentation model on, GPU
			// frames come from the 2MB-region randomized allocator.
			if avatarPAddr, ok := a.tryAvatarAllocate(
				deviceID, pid, vAddr); ok {
				pAddr = avatarPAddr
			} else {
				pAddr = device.allocatePage()
			}
		}

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

// tryRestSegAllocate places a page into the device's RestSeg when Utopia is
// enabled and the hashed set has a free way. // sbin_claude_utopia
func (a *memoryAllocatorImpl) tryRestSegAllocate(
	deviceID int,
	pid vm.PID,
	vAddr uint64,
) (pAddr uint64, ok bool) {
	if a.utopiaRegistry == nil {
		return 0, false
	}

	return a.utopiaRegistry.Allocate(deviceID, pid, vAddr)
}

func (a *memoryAllocatorImpl) removePage(vAddr uint64) {
	page, ok := a.vAddrToPageMapping[vAddr]

	if !ok {
		panic("page not found")
	}

	// Pre-edit code (commented per project convention):
	// deviceID := a.deviceIDByPAddr(page.PAddr)
	// dState := a.devices[deviceID].MemState
	// dState.addSinglePAddr(page.PAddr)
	//
	// sbin_claude_utopia: a freed RestSeg frame goes back to the TAR/SF state,
	// never to the FlexSeg frame pool; freed FlexSeg frames behave as before.
	//
	// sbin_claude_avatar: a region-owned frame returns to its 2MB region
	// (the region rejoins the pool when its last page leaves); its embedded
	// metadata is dropped so the stale location can never validate a future
	// mis-speculation (refs/avatar.md 5.11).
	if a.avatarRegistry != nil {
		a.avatarRegistry.Invalidate(page.PAddr)
	}
	switch {
	case a.isRestSegFrame(page.PAddr):
		a.utopiaRegistry.Release(page.PID, page.VAddr)
	case a.avatarRegistry != nil &&
		a.avatarRegistry.FreeFrame(a.deviceIDByPAddr(page.PAddr), page.PAddr):
		// Returned to the Avatar region pool.
	default:
		deviceID := a.deviceIDByPAddr(page.PAddr)
		dState := a.devices[deviceID].MemState
		dState.addSinglePAddr(page.PAddr)
	}
	a.recordFree()                      // sbin_codex: update physical footprint counters.
	delete(a.vAddrToPageMapping, vAddr) // sbin_codex: keep live-page accounting accurate.

	a.removePageFromTables(page) // sbin_codex
}

// UpdatePage updates a page's runtime state in every driver-managed table.
// sbin_codex
func (a *memoryAllocatorImpl) UpdatePage(page vm.Page) {
	a.Lock()
	defer a.Unlock()

	// sbin_claude_avatar: capture the old mapping before it is overwritten
	// so metadata on the old physical location can be invalidated.
	old, hadOld := a.vAddrToPageMapping[page.VAddr]

	a.vAddrToPageMapping[page.VAddr] = page
	a.updatePage(page)

	a.avatarOnUpdate(old, hadOld, page) // sbin_claude_avatar
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
	// Pre-edit code (commented per project convention):
	// pAddr := device.allocatePage()
	//
	// sbin_claude_avatar: route through the region allocator when active.
	pAddr, inAvatar := a.tryAvatarAllocate(deviceID, pid, vAddr)
	if !inAvatar {
		pAddr = device.allocatePage()
	}

	page := vm.Page{
		PID:      pid,
		VAddr:    vAddr,
		PAddr:    pAddr,
		PageSize: pageSize,
		Valid:    true,
		DeviceID: uint64(deviceID),
		Unified:  isUnified,
	}
	old, hadOld := a.vAddrToPageMapping[page.VAddr] // sbin_claude_avatar
	a.vAddrToPageMapping[page.VAddr] = page
	a.updatePage(page)                  // sbin_codex
	a.avatarOnUpdate(old, hadOld, page) // sbin_claude_avatar
	a.recordAllocation(1)               // sbin_codex: migration allocation contributes to footprint.

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
	// Pre-edit code (commented per project convention):
	// pAddrs := device.allocateMultiplePages(len(vAddrs))
	//
	// sbin_claude_avatar: allocate per-vAddr so the region allocator can
	// keep each page at its position inside its 2MB region.
	pAddrs := make([]uint64, 0, len(vAddrs))
	for _, vAddr := range vAddrs {
		if pAddr, ok := a.tryAvatarAllocate(deviceID, pid, vAddr); ok {
			pAddrs = append(pAddrs, pAddr)
		} else {
			pAddrs = append(pAddrs, device.allocatePage())
		}
	}

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
		old, hadOld := a.vAddrToPageMapping[page.VAddr] // sbin_claude_avatar
		a.vAddrToPageMapping[page.VAddr] = page
		a.updatePage(page)                  // sbin_codex
		a.avatarOnUpdate(old, hadOld, page) // sbin_claude_avatar
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

// sbin_codex: UVM demand-paging allocator implementations.
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
	numPages := int((byteSize-1)/pageSize + 1)

	pState, found := a.processMemoryStates[pid]
	if !found {
		a.processMemoryStates[pid] = &processMemoryState{
			pid:       pid,
			nextVAddr: uint64(1 << a.log2PageSize),
		}
		pState = a.processMemoryStates[pid]
	}
	nextVAddr := pState.nextVAddr

	result := ManagedAllocationResult{
		Base:            nextVAddr,
		Size:            byteSize,
		PageCount:       uint64(numPages),
		PageSize:        pageSize,
		CPUBackingPages: make([]uint64, 0, numPages),
		PIDs:            make([]vm.PID, 0, numPages),
	}

	cpuDevice := a.devices[0]
	for i := 0; i < numPages; i++ {
		vAddr := nextVAddr + uint64(i)*pageSize
		cpuPAddr := cpuDevice.allocatePage()

		page := vm.Page{
			PID:         pid,
			VAddr:       vAddr,
			PAddr:       cpuPAddr,
			PageSize:    pageSize,
			Valid:       true,
			DeviceID:    0,
			Unified:     false,
			Managed:     true,
			IsPinned:    false,
			IsMigrating: false,
		}
		a.insertPage(page)
		a.vAddrToPageMapping[page.VAddr] = page
		result.CPUBackingPages = append(result.CPUBackingPages, cpuPAddr)
		result.PIDs = append(result.PIDs, pid)
	}
	a.recordAllocation(numPages)

	pState.nextVAddr += pageSize * uint64(numPages)

	return result
}

func (a *memoryAllocatorImpl) TryAllocatePhysicalPage(
	deviceID int,
) (uint64, bool) {
	a.Lock()
	defer a.Unlock()

	device, found := a.devices[deviceID]
	if !found {
		return 0, false
	}
	if device.MemState.noAvailablePAddrs() {
		return 0, false
	}
	return device.allocatePage(), true
}

func (a *memoryAllocatorImpl) FreePhysicalPage(
	deviceID int,
	pAddr uint64,
) {
	a.Lock()
	defer a.Unlock()

	device, found := a.devices[deviceID]
	if !found {
		panic("device not found")
	}
	device.MemState.addSinglePAddr(pAddr)
	a.recordFree()
}
