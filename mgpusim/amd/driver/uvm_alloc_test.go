package driver

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
	"go.uber.org/mock/gomock"
)

// sbin_codex: managed-allocation registration contract tests (todo 3 of plan
// mgpusim-uvm-manager). The QA regex
// 'TestUVMManagedAllocation(OneByte|Page|Region|VABlockEdge|RegistrationRollback|PTERollback)'
// runs the end-to-end registration-mask fixtures and the injected
// registration-failure rollback in this file; the internal package owns the
// PTE/frame fixtures and the injected-PTE-failure rollback.

// buildManagedDriver builds a real driver (real allocator, real CPU + GPU
// page tables, real UVM manager) so allocation flows through
// AllocateManagedMemory end to end.
func buildManagedDriver(t *testing.T, accessCounter bool) (
	*Driver, vm.PageTable, []vm.PageTable,
) {
	t.Helper()

	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)
	gpuTables := []vm.PageTable{vm.NewPageTable(12), vm.NewPageTable(12)}

	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	cfg.AccessCounter = accessCounter

	d := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGPUPageTables(gpuTables).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	return d, pageTable, gpuTables
}

// expectMask asserts a registration mask array equals the expected words.
func expectMask(t *testing.T, name string, got, want []uint64) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s length = %d, want %d", name, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %#x, want %#x", name, i, got[i], want[i])
		}
	}
}

// expectRegistration asserts the exact boundaries and masks of a managed
// allocation registration.
func expectRegistration(
	t *testing.T,
	reg *ManagedAllocationRegistration,
	pid vm.PID,
	base, size, pageCount uint64,
	valid, resident, inFlight, dirty []uint64,
) {
	t.Helper()

	if reg.PID != pid {
		t.Errorf("registration PID = %d, want %d", reg.PID, pid)
	}
	if reg.Base != base {
		t.Errorf("registration Base = %x, want %x", reg.Base, base)
	}
	if reg.Size != size {
		t.Errorf("registration Size = %d, want %d", reg.Size, size)
	}
	if reg.PageCount != pageCount {
		t.Errorf("registration PageCount = %d, want %d", reg.PageCount, pageCount)
	}
	if reg.PageSize != 4096 {
		t.Errorf("registration PageSize = %d, want 4096", reg.PageSize)
	}
	expectMask(t, "ValidMask", reg.ValidMask, valid)
	expectMask(t, "ResidentMask", reg.ResidentMask, resident)
	expectMask(t, "InFlightMask", reg.InFlightMask, inFlight)
	expectMask(t, "DirtyMask", reg.DirtyMask, dirty)
}

// mustFind fetches a page from a table, failing the test when absent.
func mustFind(t *testing.T, table vm.PageTable, pid vm.PID, vAddr uint64) vm.Page {
	t.Helper()

	page, found := table.Find(pid, vAddr)
	if !found {
		t.Fatalf("no page table entry for %x", vAddr)
	}
	return page
}

func TestUVMManagedAllocationOneByte(t *testing.T) {
	d, cpuTable, gpuTables := buildManagedDriver(t, false) // Access Counter off
	ctx := d.Init()
	pid := ctx.pid

	ptr := d.AllocateManagedMemory(ctx, 1)
	if ptr != Ptr(4096) {
		t.Errorf("AllocateManagedMemory(1) = %d, want 4096", ptr)
	}
	if got := d.uvm.RegistrationCount(); got != 1 {
		t.Fatalf("RegistrationCount = %d, want 1", got)
	}

	expectRegistration(t, d.uvm.registrations[0], pid, 4096, 1, 1,
		[]uint64{0x1}, []uint64{0x0}, []uint64{0x0}, []uint64{0x0})

	// CPU truth: authoritative CPU backing mapping.
	truth := mustFind(t, cpuTable, pid, 4096)
	if !truth.Valid || !truth.Managed || truth.PAddr == 0 {
		t.Errorf("CPU truth = %+v, want valid managed backing page", truth)
	}
	if truth.Location != vm.MemoryLocationCPU_REMOTE {
		t.Errorf("CPU truth Location = %s, want CPU_REMOTE", truth.Location)
	}

	// Access Counter off: every GPU PTE is INVALID with no consumable address.
	for i, gpuTable := range gpuTables {
		pte := mustFind(t, gpuTable, pid, 4096)
		if pte.Valid || pte.Location != vm.MemoryLocationINVALID || pte.PAddr != 0 {
			t.Errorf("GPU %d PTE = %+v, want INVALID/Valid=false/PAddr=0", i+1, pte)
		}
		if !pte.Managed {
			t.Errorf("GPU %d PTE: Managed = false, want true", i+1)
		}
	}
}

func TestUVMManagedAllocationPage(t *testing.T) {
	d, cpuTable, gpuTables := buildManagedDriver(t, true) // Access Counter on
	ctx := d.Init()
	pid := ctx.pid

	ptr := d.AllocateManagedMemory(ctx, 4096)
	if ptr != Ptr(4096) {
		t.Errorf("AllocateManagedMemory(4096) = %d, want 4096", ptr)
	}
	if got := d.uvm.RegistrationCount(); got != 1 {
		t.Fatalf("RegistrationCount = %d, want 1", got)
	}

	expectRegistration(t, d.uvm.registrations[0], pid, 4096, 4096, 1,
		[]uint64{0x1}, []uint64{0x0}, []uint64{0x0}, []uint64{0x0})

	truth := mustFind(t, cpuTable, pid, 4096)
	if !truth.Valid || !truth.Managed || truth.PAddr == 0 {
		t.Errorf("CPU truth = %+v, want valid managed backing page", truth)
	}

	// Access Counter on: every GPU PTE maps the page remotely to the
	// authoritative CPU backing PA.
	for i, gpuTable := range gpuTables {
		pte := mustFind(t, gpuTable, pid, 4096)
		if !pte.Valid || pte.Location != vm.MemoryLocationCPU_REMOTE {
			t.Errorf("GPU %d PTE = %+v, want CPU_REMOTE/Valid=true", i+1, pte)
		}
		if pte.PAddr != truth.PAddr {
			t.Errorf("GPU %d PTE PAddr = %x, want CPU backing %x",
				i+1, pte.PAddr, truth.PAddr)
		}
		if !pte.Managed {
			t.Errorf("GPU %d PTE: Managed = false, want true", i+1)
		}
	}
}

func TestUVMManagedAllocationRegion(t *testing.T) {
	d, cpuTable, gpuTables := buildManagedDriver(t, false) // Access Counter off
	ctx := d.Init()
	pid := ctx.pid

	// 64 KB = 16 base pages.
	d.AllocateManagedMemory(ctx, 64*mem.KB)
	if got := d.uvm.RegistrationCount(); got != 1 {
		t.Fatalf("RegistrationCount = %d, want 1", got)
	}

	expectRegistration(t, d.uvm.registrations[0], pid, 4096, 64*mem.KB, 16,
		[]uint64{0xFFFF}, []uint64{0x0}, []uint64{0x0}, []uint64{0x0})

	for i := uint64(0); i < 16; i++ {
		vAddr := 4096 + i*4096
		truth := mustFind(t, cpuTable, pid, vAddr)
		if !truth.Valid || !truth.Managed || truth.PAddr == 0 {
			t.Errorf("CPU truth %x = %+v, want valid managed backing page", vAddr, truth)
		}
		for j, gpuTable := range gpuTables {
			pte := mustFind(t, gpuTable, pid, vAddr)
			if pte.Valid || pte.Location != vm.MemoryLocationINVALID || pte.PAddr != 0 {
				t.Errorf("GPU %d PTE %x = %+v, want INVALID/Valid=false/PAddr=0",
					j+1, vAddr, pte)
			}
		}
	}
}

func TestUVMManagedAllocationVABlockEdge(t *testing.T) {
	d, cpuTable, gpuTables := buildManagedDriver(t, false) // Access Counter off
	ctx := d.Init()
	pid := ctx.pid

	// 2 MB + 4 KB = 513 base pages: the 513th page crosses into the next
	// 2 MB VA block, so the valid mask spans 9 words with a partial last word.
	size := 2*mem.MB + 4096
	d.AllocateManagedMemory(ctx, size)
	if got := d.uvm.RegistrationCount(); got != 1 {
		t.Fatalf("RegistrationCount = %d, want 1", got)
	}

	valid := make([]uint64, 9)
	for i := 0; i < 8; i++ {
		valid[i] = ^uint64(0)
	}
	valid[8] = 0x1
	expectRegistration(t, d.uvm.registrations[0], pid, 4096, size, 513,
		valid, make([]uint64, 9), make([]uint64, 9), make([]uint64, 9))

	// Spot-check the PTE state at the VA-block boundary pages.
	for _, i := range []uint64{0, 511, 512} {
		vAddr := 4096 + i*4096
		truth := mustFind(t, cpuTable, pid, vAddr)
		if !truth.Valid || !truth.Managed || truth.PAddr == 0 {
			t.Errorf("CPU truth %x = %+v, want valid managed backing page", vAddr, truth)
		}
		for j, gpuTable := range gpuTables {
			pte := mustFind(t, gpuTable, pid, vAddr)
			if pte.Valid || pte.Location != vm.MemoryLocationINVALID || pte.PAddr != 0 {
				t.Errorf("GPU %d PTE %x = %+v, want INVALID/Valid=false/PAddr=0",
					j+1, vAddr, pte)
			}
		}
	}
}

func TestUVMManagedAllocationRegistrationRollback(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	alloc := NewMockMemoryAllocator(ctrl)

	cfg := DefaultUVMConfig()
	cfg.Enabled = true
	d := &Driver{
		memAllocator: alloc,
		uvm:          NewUVMManager(cfg, 4*mem.GB),
	}

	ctx := d.Init()

	// The allocator returns an inconsistent result (PageCount 2 does not
	// match ceil(100/4096) = 1), so registration must fail and the driver
	// must roll the allocation back page by page.
	bad := internal.ManagedAllocationResult{
		Base:            4096,
		Size:            100,
		PageCount:       2,
		PageSize:        4096,
		CPUBackingPages: []uint64{0x1_0000_1000, 0x1_0000_2000},
		PIDs:            []vm.PID{ctx.pid, ctx.pid},
	}
	alloc.EXPECT().AllocateManaged(ctx.pid, uint64(100)).Return(bad)
	alloc.EXPECT().Free(uint64(4096))
	alloc.EXPECT().Free(uint64(8192))

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("AllocateManagedMemory with failing registration: no panic, want panic")
			}
		}()
		d.AllocateManagedMemory(ctx, 100)
	}()

	// Rollback leaves no registration behind.
	if got := d.uvm.RegistrationCount(); got != 0 {
		t.Errorf("RegistrationCount after rollback = %d, want 0", got)
	}
}
