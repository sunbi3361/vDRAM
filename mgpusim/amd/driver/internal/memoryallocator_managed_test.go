package internal

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_codex: managed-allocation contract tests (todo 3 of plan
// mgpusim-uvm-manager). The QA regex
// 'TestUVMManagedAllocation(OneByte|Page|Region|VABlockEdge|RegistrationRollback|PTERollback)'
// runs the PTE/frame fixtures and the injected-PTE-failure rollback in this
// file; the driver package owns the registration-mask fixtures and the
// injected-registration-failure rollback.

// freeFrameCount returns the number of free frames a device memory state
// currently holds, or -1 when the state is not the regular implementation.
func freeFrameCount(state DeviceMemoryState) int {
	if impl, ok := state.(*deviceMemoryStateImpl); ok {
		return len(impl.availablePAddrs)
	}
	return -1
}

// newManagedTestAllocator builds an allocator with one CPU and four GPU
// devices, each with a real page table, and returns the CPU table and the
// per-GPU tables for PTE assertions.
func newManagedTestAllocator(t *testing.T) (
	*memoryAllocatorImpl, vm.PageTable, []vm.PageTable,
) {
	t.Helper()

	cpuTable := vm.NewPageTable(12)
	allocator := NewMemoryAllocator(cpuTable, 12).(*memoryAllocatorImpl)
	configAFourGPUSystem(allocator)

	gpuTables := make([]vm.PageTable, 4)
	for i := range gpuTables {
		gpuTables[i] = vm.NewPageTable(12)
		allocator.RegisterPageTable(i+1, gpuTables[i])
	}

	return allocator, cpuTable, gpuTables
}

// gpuFreeFrameCounts returns the free-frame count of every GPU device.
func gpuFreeFrameCounts(allocator *memoryAllocatorImpl) []int {
	counts := make([]int, 4)
	for i := 0; i < 4; i++ {
		counts[i] = freeFrameCount(allocator.devices[i+1].MemState)
	}
	return counts
}

// expectManagedTruth asserts the authoritative CPU-table entry for a managed
// page: valid, managed, CPU_REMOTE, and carrying the CPU backing PA.
func expectManagedTruth(t *testing.T, table vm.PageTable, pid vm.PID, vAddr uint64) vm.Page {
	t.Helper()

	page, found := table.Find(pid, vAddr)
	if !found {
		t.Fatalf("CPU table: no entry for managed page %x", vAddr)
	}
	if !page.Valid {
		t.Errorf("CPU table page %x: Valid = false, want true", vAddr)
	}
	if !page.Managed {
		t.Errorf("CPU table page %x: Managed = false, want true", vAddr)
	}
	if page.Location != vm.MemoryLocationCPU_REMOTE {
		t.Errorf("CPU table page %x: Location = %s, want CPU_REMOTE",
			vAddr, page.Location)
	}
	if page.PAddr == 0 {
		t.Errorf("CPU table page %x: PAddr = 0, want CPU backing PA", vAddr)
	}
	return page
}

// expectManagedGPUPTE asserts the per-GPU PTE state for a managed page:
// INVALID/Valid=false/PAddr=0 when the Access Counter is off, otherwise
// CPU_REMOTE/Valid=true/PAddr=<CPU backing PA>.
func expectManagedGPUPTE(
	t *testing.T,
	gpuTables []vm.PageTable,
	pid vm.PID,
	vAddr uint64,
	accessCounter bool,
	cpuPAddr uint64,
) {
	t.Helper()

	for i, gpuTable := range gpuTables {
		pte, found := gpuTable.Find(pid, vAddr)
		if !found {
			t.Fatalf("GPU %d table: no entry for managed page %x", i+1, vAddr)
		}
		if !pte.Managed {
			t.Errorf("GPU %d PTE %x: Managed = false, want true", i+1, vAddr)
		}
		if pte.Location == vm.MemoryLocationGPU_LOCAL {
			t.Errorf("GPU %d PTE %x: GPU_LOCAL before any migration", i+1, vAddr)
		}
		if accessCounter {
			if pte.Location != vm.MemoryLocationCPU_REMOTE {
				t.Errorf("GPU %d PTE %x: Location = %s, want CPU_REMOTE",
					i+1, vAddr, pte.Location)
			}
			if !pte.Valid {
				t.Errorf("GPU %d PTE %x: Valid = false, want true", i+1, vAddr)
			}
			if pte.PAddr != cpuPAddr {
				t.Errorf("GPU %d PTE %x: PAddr = %x, want CPU backing %x",
					i+1, vAddr, pte.PAddr, cpuPAddr)
			}
		} else {
			if pte.Location != vm.MemoryLocationINVALID {
				t.Errorf("GPU %d PTE %x: Location = %s, want INVALID",
					i+1, vAddr, pte.Location)
			}
			if pte.Valid {
				t.Errorf("GPU %d PTE %x: Valid = true, want false", i+1, vAddr)
			}
			if pte.PAddr != 0 {
				t.Errorf("GPU %d PTE %x: PAddr = %x, want 0", i+1, vAddr, pte.PAddr)
			}
		}
	}
}

func TestUVMManagedAllocationOneByte(t *testing.T) {
	allocator, cpuTable, gpuTables := newManagedTestAllocator(t)

	cpuFramesBefore := freeFrameCount(allocator.devices[0].MemState)
	gpuFramesBefore := gpuFreeFrameCounts(allocator)

	// Access Counter off: the initial GPU PTE is INVALID.
	res := allocator.AllocateManaged(1, 1)

	if res.Base != 4096 || res.Size != 1 || res.PageCount != 1 || res.PageSize != 4096 {
		t.Errorf("result = (%x, %d, %d, %d), want (4096, 1, 1, 4096)",
			res.Base, res.Size, res.PageCount, res.PageSize)
	}
	if len(res.CPUBackingPages) != 1 || res.CPUBackingPages[0] == 0 {
		t.Fatalf("CPUBackingPages = %v, want one non-zero CPU backing page", res.CPUBackingPages)
	}
	if len(res.PIDs) != 1 || res.PIDs[0] != 1 {
		t.Errorf("PIDs = %v, want [1]", res.PIDs)
	}

	truth := expectManagedTruth(t, cpuTable, 1, 4096)
	if truth.PAddr != res.CPUBackingPages[0] {
		t.Errorf("CPU truth PAddr = %x, want backing %x",
			truth.PAddr, res.CPUBackingPages[0])
	}
	expectManagedGPUPTE(t, gpuTables, 1, 4096, false, res.CPUBackingPages[0])

	// CPU backing consumed exactly one frame; no GPU frame was allocated.
	if got := freeFrameCount(allocator.devices[0].MemState); got != cpuFramesBefore-1 {
		t.Errorf("CPU frames = %d, want %d (one consumed)", got, cpuFramesBefore-1)
	}
	for i, got := range gpuFreeFrameCounts(allocator) {
		if got != gpuFramesBefore[i] {
			t.Errorf("GPU %d frames = %d, want %d (zero consumed)",
				i+1, got, gpuFramesBefore[i])
		}
	}
}

func TestUVMManagedAllocationPage(t *testing.T) {
	allocator, cpuTable, gpuTables := newManagedTestAllocator(t)

	cpuFramesBefore := freeFrameCount(allocator.devices[0].MemState)
	gpuFramesBefore := gpuFreeFrameCounts(allocator)

	// Access Counter on: the initial GPU PTE maps the page remotely to the
	// authoritative CPU backing PA.
	allocator.SetManagedAccessCounter(true)
	res := allocator.AllocateManaged(1, 4096)

	if res.Base != 4096 || res.PageCount != 1 || res.PageSize != 4096 {
		t.Errorf("result = (%x, %d, %d), want (4096, 1, 4096)",
			res.Base, res.PageCount, res.PageSize)
	}
	truth := expectManagedTruth(t, cpuTable, 1, 4096)
	if truth.PAddr != res.CPUBackingPages[0] {
		t.Errorf("CPU truth PAddr = %x, want backing %x",
			truth.PAddr, res.CPUBackingPages[0])
	}
	expectManagedGPUPTE(t, gpuTables, 1, 4096, true, truth.PAddr)

	if got := freeFrameCount(allocator.devices[0].MemState); got != cpuFramesBefore-1 {
		t.Errorf("CPU frames = %d, want %d (one consumed)", got, cpuFramesBefore-1)
	}
	for i, got := range gpuFreeFrameCounts(allocator) {
		if got != gpuFramesBefore[i] {
			t.Errorf("GPU %d frames = %d, want %d (zero consumed)",
				i+1, got, gpuFramesBefore[i])
		}
	}
}

func TestUVMManagedAllocationRegion(t *testing.T) {
	allocator, cpuTable, gpuTables := newManagedTestAllocator(t)

	cpuFramesBefore := freeFrameCount(allocator.devices[0].MemState)
	gpuFramesBefore := gpuFreeFrameCounts(allocator)

	// 64 KB = 16 base pages, Access Counter off.
	res := allocator.AllocateManaged(1, 64*mem.KB)

	if res.PageCount != 16 {
		t.Fatalf("PageCount = %d, want 16", res.PageCount)
	}
	if len(res.CPUBackingPages) != 16 {
		t.Fatalf("CPUBackingPages = %d entries, want 16", len(res.CPUBackingPages))
	}
	for i := uint64(0); i < 16; i++ {
		vAddr := res.Base + i*4096
		truth := expectManagedTruth(t, cpuTable, 1, vAddr)
		if truth.PAddr != res.CPUBackingPages[i] {
			t.Errorf("page %d truth PAddr = %x, want backing %x",
				i, truth.PAddr, res.CPUBackingPages[i])
		}
		expectManagedGPUPTE(t, gpuTables, 1, vAddr, false, res.CPUBackingPages[i])
	}

	if got := freeFrameCount(allocator.devices[0].MemState); got != cpuFramesBefore-16 {
		t.Errorf("CPU frames = %d, want %d (16 consumed)", got, cpuFramesBefore-16)
	}
	for i, got := range gpuFreeFrameCounts(allocator) {
		if got != gpuFramesBefore[i] {
			t.Errorf("GPU %d frames = %d, want %d (zero consumed)",
				i+1, got, gpuFramesBefore[i])
		}
	}
}

func TestUVMManagedAllocationVABlockEdge(t *testing.T) {
	allocator, cpuTable, gpuTables := newManagedTestAllocator(t)

	cpuFramesBefore := freeFrameCount(allocator.devices[0].MemState)
	gpuFramesBefore := gpuFreeFrameCounts(allocator)

	// 2 MB + 4 KB = 513 base pages: the 513th page crosses into the next
	// 2 MB VA block. Access Counter off.
	res := allocator.AllocateManaged(1, 2*mem.MB+4096)

	if res.PageCount != 513 {
		t.Fatalf("PageCount = %d, want 513", res.PageCount)
	}
	if len(res.CPUBackingPages) != 513 {
		t.Fatalf("CPUBackingPages = %d entries, want 513", len(res.CPUBackingPages))
	}
	for i := uint64(0); i < 513; i++ {
		vAddr := res.Base + i*4096
		truth := expectManagedTruth(t, cpuTable, 1, vAddr)
		if truth.PAddr != res.CPUBackingPages[i] {
			t.Errorf("page %d truth PAddr = %x, want backing %x",
				i, truth.PAddr, res.CPUBackingPages[i])
		}
		expectManagedGPUPTE(t, gpuTables, 1, vAddr, false, res.CPUBackingPages[i])
	}

	if got := freeFrameCount(allocator.devices[0].MemState); got != cpuFramesBefore-513 {
		t.Errorf("CPU frames = %d, want %d (513 consumed)", got, cpuFramesBefore-513)
	}
	for i, got := range gpuFreeFrameCounts(allocator) {
		if got != gpuFramesBefore[i] {
			t.Errorf("GPU %d frames = %d, want %d (zero consumed)",
				i+1, got, gpuFramesBefore[i])
		}
	}
}

func TestUVMManagedAllocationPTERollback(t *testing.T) {
	allocator, cpuTable, gpuTables := newManagedTestAllocator(t)

	cpuFramesBefore := freeFrameCount(allocator.devices[0].MemState)
	gpuFramesBefore := gpuFreeFrameCounts(allocator)

	// Inject a PTE failure: GPU 2's table already owns the first VA the
	// allocator will assign (4096 for a fresh PID), so the per-GPU PTE
	// insertion for page 0 panics mid-allocation.
	gpuTables[1].Insert(vm.Page{PID: 1, VAddr: 4096, PageSize: 4096, Valid: true})

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("AllocateManaged with colliding GPU PTE: no panic, want panic")
			}
		}()
		allocator.AllocateManaged(1, 3*4096)
	}()

	// No partial pages survive in any table or the VA mapping.
	for _, vAddr := range []uint64{4096, 8192, 12288} {
		if _, found := cpuTable.Find(1, vAddr); found {
			t.Errorf("rollback left CPU table entry at %x", vAddr)
		}
		for i, gpuTable := range gpuTables {
			if _, found := gpuTable.Find(1, vAddr); found {
				t.Errorf("rollback left GPU %d table entry at %x", i+1, vAddr)
			}
		}
		if _, found := allocator.vAddrToPageMapping[vAddr]; found {
			t.Errorf("rollback left vAddrToPageMapping entry at %x", vAddr)
		}
	}

	// Every frame is returned: CPU and GPU free-frame counts are restored.
	if got := freeFrameCount(allocator.devices[0].MemState); got != cpuFramesBefore {
		t.Errorf("CPU frames after rollback = %d, want %d", got, cpuFramesBefore)
	}
	for i, got := range gpuFreeFrameCounts(allocator) {
		if got != gpuFramesBefore[i] {
			t.Errorf("GPU %d frames after rollback = %d, want %d",
				i+1, got, gpuFramesBefore[i])
		}
	}
}

func TestUVMManagedAllocationGPULocalPublication(t *testing.T) {
	allocator, cpuTable, gpuTables := newManagedTestAllocator(t)

	res := allocator.AllocateManaged(1, 4096)
	truth := expectManagedTruth(t, cpuTable, 1, res.Base)

	// Migration publication: the allocator expresses GPU_LOCAL through
	// UpdatePage; the GPU tables receive the HBM PA while the CPU table
	// keeps the authoritative CPU-backing truth.
	published := vm.Page{
		PID:      1,
		VAddr:    res.Base,
		PAddr:    0x4_0000_0000, // HBM PA
		PageSize: 4096,
		Valid:    true,
		Managed:  true,
		Location: vm.MemoryLocationGPU_LOCAL,
	}
	allocator.UpdatePage(published)

	for i, gpuTable := range gpuTables {
		pte, found := gpuTable.Find(1, res.Base)
		if !found {
			t.Fatalf("GPU %d table: no entry after GPU_LOCAL publication", i+1)
		}
		if pte.Location != vm.MemoryLocationGPU_LOCAL {
			t.Errorf("GPU %d PTE: Location = %s, want GPU_LOCAL", i+1, pte.Location)
		}
		if !pte.Valid {
			t.Errorf("GPU %d PTE: Valid = false, want true", i+1)
		}
		if pte.PAddr != 0x4_0000_0000 {
			t.Errorf("GPU %d PTE: PAddr = %x, want HBM PA", i+1, pte.PAddr)
		}
	}

	after := expectManagedTruth(t, cpuTable, 1, res.Base)
	if after.PAddr != truth.PAddr {
		t.Errorf("CPU truth PAddr changed to %x after GPU_LOCAL publication, want %x",
			after.PAddr, truth.PAddr)
	}

	mapped := allocator.vAddrToPageMapping[res.Base]
	if mapped.Location != vm.MemoryLocationCPU_REMOTE || mapped.PAddr != truth.PAddr {
		t.Errorf("vAddrToPageMapping lost the CPU truth: %+v", mapped)
	}
}
