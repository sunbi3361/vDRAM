package driver

// sbin_codex: unit coverage for the UVM state machine described by
// uvm-manager.md. Ideal mode is used throughout so the functional sequence
// runs without a GPU platform: with no GPU control endpoint registered, the
// region-scoped invalidations complete immediately and migrations move their
// bytes through globalStorage.

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

const testRegionSize = 64 * 1024

const testVABlockSize = 2 * 1024 * 1024

// allocateFullVABlock reserves enough managed memory that at least one 2MB VA
// block is entirely backed, and returns that block's base. TBN uses the valid
// allocation mask as its denominator, so a partially backed block would change
// the occupancy arithmetic under test. // sbin_codex
func allocateFullVABlock(d *Driver, ctx *Context) uint64 {
	ptr := uint64(d.AllocateManaged(ctx, 4*testVABlockSize))

	return (ptr + testVABlockSize - 1) / testVABlockSize * testVABlockSize
}

func buildUVMDriver(
	engine *sim.SerialEngine,
	pageTable vm.PageTable,
	config UVMConfig,
) *Driver {
	d := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGlobalStorage(mem.NewStorage(1 << 30)).
		WithD2HCycles(1).
		WithH2DCycles(1).
		WithUVM(config).
		Build("Driver")

	d.RegisterGPU(
		sim.NewPort(d, 8, 8, "TestGPU.CP"),
		DeviceProperties{CUCount: 4, DRAMSize: 4 * 1024 * 1024 * 1024},
	)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("TestGPU.Conn")
	conn.PlugIn(d.gpuPort)
	conn.PlugIn(d.GPUs[0])

	return d
}

func defaultUVMConfig() UVMConfig {
	return UVMConfig{
		Enabled:                true,
		Ideal:                  true,
		FaultLatencyUS:         20,
		AccessCounterEnabled:   true,
		AccessCounterThreshold: 8,
		TBNExpandRatio:         0.51,
		TBNMaxFetchSize:        2 * 1024 * 1024,
		GPUCapacityBytes:       128 * 1024 * 1024,
	}
}

// lazyRemotePTEConfig is defaultUVMConfig with the REMOTE mapping deferred to
// the first access. // sbin_claude_uvm
func lazyRemotePTEConfig() UVMConfig {
	config := defaultUVMConfig()
	config.LazyRemotePTE = true

	return config
}

var _ = ginkgo.Describe("UVMManager", func() {
	var (
		driver    *Driver
		engine    *sim.SerialEngine
		pageTable vm.PageTable
		ctx       *Context
	)

	ginkgo.BeforeEach(func() {
		engine = sim.NewSerialEngine()
		pageTable = vm.NewPageTable(12)
		driver = buildUVMDriver(engine, pageTable, defaultUVMConfig())
		ctx = driver.Init()
	})

	ginkgo.It("registers a managed allocation as CPU resident", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		gomega.Expect(uint64(ptr)).To(gomega.BeNumerically(">", 0))

		base := uint64(ptr)
		managedPage := driver.uvm.pages[PageKey{PID: ctx.pid, VAddr: base}]
		gomega.Expect(managedPage).NotTo(gomega.BeNil())
		gomega.Expect(managedPage.State).To(gomega.Equal(CPUResident))
		gomega.Expect(managedPage.GPUFrameValid).To(gomega.BeFalse())

		gomega.Expect(len(driver.uvm.pages)).To(gomega.Equal(32))
		gomega.Expect(len(driver.uvm.blocks)).To(gomega.Equal(1))
	})

	// Spec 7.1: with access-counter mode on, a cold managed page is REMOTE, so
	// the first GPU read is a counted remote access instead of a demand fault.
	ginkgo.It("maps a cold page remotely when access counting is on", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)

		pte, found := pageTable.Find(ctx.pid, base)
		gomega.Expect(found).To(gomega.BeTrue())
		gomega.Expect(pte.DeviceID).To(gomega.Equal(uint64(0)))
		gomega.Expect(pte.RemoteAccessible).To(gomega.BeTrue())
		gomega.Expect(pte.Managed).To(gomega.BeTrue())
	})

	// Spec 7.1: with access-counter mode off, the same page is INVALID, so the
	// first access is a demand fault.
	ginkgo.It("maps a cold page invalid when access counting is off", func() {
		localEngine := sim.NewSerialEngine()
		localTable := vm.NewPageTable(12)
		config := defaultUVMConfig()
		config.AccessCounterEnabled = false
		localDriver := buildUVMDriver(localEngine, localTable, config)
		localCtx := localDriver.Init()

		base := uint64(localDriver.AllocateManaged(localCtx, 128*1024))

		pte, found := localTable.Find(localCtx.pid, base)
		gomega.Expect(found).To(gomega.BeTrue())
		gomega.Expect(pte.RemoteAccessible).To(gomega.BeFalse())
	})

	// sbin_claude_uvm: -uvm-lazy-remote-pte defers the REMOTE mapping, so a
	// cold page is INVALID even though access counting is on.
	ginkgo.It("maps a cold page invalid when the remote PTE is lazy", func() {
		localEngine := sim.NewSerialEngine()
		localTable := vm.NewPageTable(12)
		localDriver := buildUVMDriver(
			localEngine, localTable, lazyRemotePTEConfig())
		localCtx := localDriver.Init()

		base := uint64(localDriver.AllocateManaged(localCtx, 128*1024))

		pte, found := localTable.Find(localCtx.pid, base)
		gomega.Expect(found).To(gomega.BeTrue())
		gomega.Expect(pte.RemoteAccessible).To(gomega.BeFalse())
		gomega.Expect(localDriver.uvm.stats.RemotePTEInstalls).
			To(gomega.Equal(uint64(0)))
	})

	// sbin_claude_uvm: the first fault publishes the whole 64KB region as
	// REMOTE and migrates nothing. Residency stays the access counter's call.
	ginkgo.It("publishes a region remotely on the first fault", func() {
		localEngine := sim.NewSerialEngine()
		localTable := vm.NewPageTable(12)
		localDriver := buildUVMDriver(
			localEngine, localTable, lazyRemotePTEConfig())
		localCtx := localDriver.Init()

		// A 2MB-aligned block keeps the 64KB region fully backed, so all 16 of
		// its pages are registered and the counts are exact.
		regionBase := allocateFullVABlock(localDriver, localCtx)

		localDriver.uvm.onPageFault(localCtx.pid, regionBase, 1, false)
		localEngine.Run()

		uvm := localDriver.uvm
		gomega.Expect(uvm.stats.LazyRemoteMaps).To(gomega.Equal(uint64(1)))
		gomega.Expect(uvm.stats.UniqueFaultServices).To(gomega.Equal(uint64(0)))
		gomega.Expect(uvm.stats.CPUToGPUMigrations).To(gomega.Equal(uint64(0)))
		gomega.Expect(uvm.stats.RemotePTEInstalls).
			To(gomega.Equal(uint64(testRegionSize / 4096)))

		// Every page of the region is now remotely accessible and still on the
		// host: the fault changed the mapping, not the residency.
		for offset := uint64(0); offset < testRegionSize; offset += 4096 {
			pte, found := localTable.Find(localCtx.pid, regionBase+offset)
			gomega.Expect(found).To(gomega.BeTrue())
			gomega.Expect(pte.RemoteAccessible).To(gomega.BeTrue())
			gomega.Expect(pte.DeviceID).To(gomega.Equal(uint64(0)))

			managedPage := uvm.pages[PageKey{
				PID: localCtx.pid, VAddr: regionBase + offset}]
			gomega.Expect(managedPage.State).To(gomega.Equal(CPUResident))
			gomega.Expect(managedPage.GPUFrameValid).To(gomega.BeFalse())
		}
	})

	// sbin_claude_uvm: a remote write is never performed, so a region whose
	// first touch is a write is migrated on demand rather than mapped REMOTE.
	ginkgo.It("migrates on demand when the first touch is a write", func() {
		localEngine := sim.NewSerialEngine()
		localTable := vm.NewPageTable(12)
		localDriver := buildUVMDriver(
			localEngine, localTable, lazyRemotePTEConfig())
		localCtx := localDriver.Init()

		regionBase := allocateFullVABlock(localDriver, localCtx)

		localDriver.uvm.onPageFault(localCtx.pid, regionBase, 1, true)
		localEngine.Run()

		uvm := localDriver.uvm
		gomega.Expect(uvm.stats.LazyRemoteMaps).To(gomega.Equal(uint64(0)))
		gomega.Expect(uvm.stats.RemotePTEInstalls).To(gomega.Equal(uint64(0)))
		gomega.Expect(uvm.stats.UniqueFaultServices).To(gomega.Equal(uint64(1)))
		gomega.Expect(uvm.stats.DemandMigrations).To(gomega.Equal(uint64(1)))

		managedPage := uvm.pages[PageKey{PID: localCtx.pid, VAddr: regionBase}]
		gomega.Expect(managedPage.State).To(gomega.Equal(GPUResident))
		gomega.Expect(managedPage.RemoteMapped).To(gomega.BeFalse())

		// INVALID -> GPU_LOCAL needs no TLB invalidation: no REMOTE
		// translation was ever published, so none can be cached.
		gomega.Expect(uvm.stats.TLBRangeInvalidations).
			To(gomega.Equal(uint64(0)))
	})

	// sbin_claude_uvm: only the first fault of a region decides. A write
	// arriving behind a pending install rides its replay instead of racing it
	// with a migration.
	ginkgo.It("lets a write ride a pending lazy remote map", func() {
		localEngine := sim.NewSerialEngine()
		localTable := vm.NewPageTable(12)
		localDriver := buildUVMDriver(
			localEngine, localTable, lazyRemotePTEConfig())
		localCtx := localDriver.Init()

		regionBase := allocateFullVABlock(localDriver, localCtx)

		localDriver.uvm.onPageFault(localCtx.pid, regionBase, 1, false)
		localDriver.uvm.onPageFault(localCtx.pid, regionBase+4096, 1, true)

		uvm := localDriver.uvm
		gomega.Expect(uvm.pendingRemoteMaps).To(gomega.HaveLen(1))
		gomega.Expect(uvm.stats.CoalescedFaults).To(gomega.Equal(uint64(1)))
		gomega.Expect(uvm.stats.UniqueFaultServices).To(gomega.Equal(uint64(0)))

		localEngine.Run()

		gomega.Expect(uvm.stats.LazyRemoteMaps).To(gomega.Equal(uint64(1)))
		gomega.Expect(uvm.stats.DemandMigrations).To(gomega.Equal(uint64(0)))
	})

	// sbin_claude_uvm: one install per region. Further faults arriving before
	// it publishes ride its replay instead of scheduling another.
	ginkgo.It("coalesces faults onto a pending lazy remote map", func() {
		localEngine := sim.NewSerialEngine()
		localTable := vm.NewPageTable(12)
		localDriver := buildUVMDriver(
			localEngine, localTable, lazyRemotePTEConfig())
		localCtx := localDriver.Init()

		regionBase := allocateFullVABlock(localDriver, localCtx)

		localDriver.uvm.onPageFault(localCtx.pid, regionBase, 1, false)
		localDriver.uvm.onPageFault(localCtx.pid, regionBase+4096, 1, false)
		localDriver.uvm.onPageFault(localCtx.pid, regionBase+8192, 1, true)
		localEngine.Run()

		uvm := localDriver.uvm
		gomega.Expect(uvm.stats.RawPageFaults).To(gomega.Equal(uint64(3)))
		gomega.Expect(uvm.stats.CoalescedFaults).To(gomega.Equal(uint64(2)))
		gomega.Expect(uvm.stats.LazyRemoteMaps).To(gomega.Equal(uint64(1)))
		gomega.Expect(uvm.pendingRemoteMaps).To(gomega.BeEmpty())
	})

	// sbin_claude_uvm: lazy mapping must not disturb the migration policy. The
	// access counter still opens the service that moves the region.
	ginkgo.It("still migrates on the access counter threshold when lazy",
		func() {
			localEngine := sim.NewSerialEngine()
			localTable := vm.NewPageTable(12)
			localDriver := buildUVMDriver(
				localEngine, localTable, lazyRemotePTEConfig())
			localCtx := localDriver.Init()

			regionBase := allocateFullVABlock(localDriver, localCtx)

			localDriver.uvm.onPageFault(localCtx.pid, regionBase, 1, false)
			localEngine.Run()

			localDriver.uvm.onAccessCounterNotify(localCtx.pid, regionBase, 1)
			localEngine.Run()

			uvm := localDriver.uvm
			gomega.Expect(uvm.stats.AccessCounterServices).
				To(gomega.Equal(uint64(1)))
			gomega.Expect(uvm.stats.CPUToGPUMigrations).
				To(gomega.BeNumerically(">", 0))

			managedPage := uvm.pages[PageKey{
				PID: localCtx.pid, VAddr: regionBase}]
			gomega.Expect(managedPage.State).To(gomega.Equal(GPUResident))
			gomega.Expect(managedPage.RemoteMapped).To(gomega.BeFalse())
		})

	// Spec 8.3 and 10.1: duplicate 4KB faults inside one 64KB region join the
	// same transaction and are charged the fixed latency only once.
	ginkgo.It("coalesces 4KB faults into one 64KB fault service", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		regionBase := base &^ (testRegionSize - 1)

		driver.uvm.onPageFault(ctx.pid, regionBase, 1, false)
		driver.uvm.onPageFault(ctx.pid, regionBase+4096, 1, false)
		driver.uvm.onPageFault(ctx.pid, regionBase+8192, 1, true)

		gomega.Expect(driver.uvm.stats.RawPageFaults).To(gomega.Equal(uint64(3)))
		gomega.Expect(driver.uvm.stats.UniqueFaultServices).
			To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.CoalescedFaults).
			To(gomega.Equal(uint64(2)))
	})

	// Spec 8.4: exactly one 64KB fault-service transaction is active at a time.
	ginkgo.It("services one 64KB region at a time, FIFO", func() {
		ptr := driver.AllocateManaged(ctx, 512*1024)
		base := uint64(ptr) &^ (testRegionSize - 1)

		driver.uvm.onPageFault(ctx.pid, base+testRegionSize, 1, false)
		driver.uvm.onPageFault(ctx.pid, base+2*testRegionSize, 1, false)
		driver.uvm.onPageFault(ctx.pid, base+3*testRegionSize, 1, false)

		gomega.Expect(driver.uvm.stats.UniqueFaultServices).
			To(gomega.Equal(uint64(3)))
		gomega.Expect(driver.uvm.activeFaultID).NotTo(gomega.BeEmpty())
		gomega.Expect(len(driver.uvm.faultServiceCue)).To(gomega.Equal(2))

		engine.Run()

		gomega.Expect(driver.uvm.activeFaultID).To(gomega.BeEmpty())
		gomega.Expect(driver.uvm.faults).To(gomega.BeEmpty())
	})

	// Spec 11.5: a lone 64KB demand leaf fills exactly 50% of its 128KB
	// parent, and 50 > 51 is false, so TBN does not expand.
	ginkgo.It("does not expand TBN at 50 percent occupancy", func() {
		base := allocateFullVABlock(driver, ctx)

		key := RegionKey{PID: ctx.pid, Base: base, DeviceID: uvmDeviceID}
		driver.uvm.onPageFault(ctx.pid, base, 1, false)

		block := driver.uvm.blocks[BlockKey{PID: ctx.pid, Base: base}]
		gomega.Expect(block).NotTo(gomega.BeNil())

		sel := driver.uvm.selectTBNRegion(key, block)
		gomega.Expect(sel.selectedSize).To(gomega.Equal(uint64(testRegionSize)))
		gomega.Expect(sel.regionBase % testRegionSize).To(gomega.Equal(uint64(0)))
	})

	// Spec 11.4 and 11.6: the ancestor walk is bottom-up and stops at the
	// first candidate that fails, so a 256KB node is only reached through a
	// 128KB node that already passed. Here leaves 1 and 2 are GPU resident and
	// the fault lands on leaf 0: the 128KB parent is fully occupied, the 256KB
	// node reaches 192KB of 256KB, and the 512KB node fails.
	ginkgo.It("expands TBN when resident neighbors exceed the threshold", func() {
		base := allocateFullVABlock(driver, ctx)

		for _, leaf := range []uint64{base + testRegionSize, base + 2*testRegionSize} {
			for off := uint64(0); off < testRegionSize; off += 4096 {
				managedPage := driver.uvm.pages[PageKey{
					PID: ctx.pid, VAddr: leaf + off,
				}]
				managedPage.State = GPUResident
				managedPage.GPUFrameValid = true
			}
		}

		key := RegionKey{PID: ctx.pid, Base: base, DeviceID: uvmDeviceID}
		driver.uvm.onPageFault(ctx.pid, base, 1, false)

		block := driver.uvm.blocks[BlockKey{PID: ctx.pid, Base: base}]
		sel := driver.uvm.selectTBNRegion(key, block)

		gomega.Expect(sel.selectedSize).To(gomega.Equal(uint64(256 * 1024)))
		gomega.Expect(sel.regionBase).To(gomega.Equal(base))
		// Already-resident pages are never transferred again (spec 11.9), so
		// only the fault leaf and the empty fourth leaf move.
		gomega.Expect(len(sel.pageKeys)).To(gomega.Equal(32))
	})

	// Spec 16: a threshold crossing migrates the whole 64KB region.
	ginkgo.It("migrates a region on an access-counter notification", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		regionBase := base &^ (testRegionSize - 1)

		driver.uvm.onAccessCounterNotify(ctx.pid, regionBase, uvmDeviceID)
		engine.Run()

		gomega.Expect(driver.uvm.stats.AccessCounterNotify).
			To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.AccessCounterMigrations).
			To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.CPUToGPUMigrations).
			To(gomega.Equal(uint64(1)))

		managedPage := driver.uvm.pages[PageKey{PID: ctx.pid, VAddr: base}]
		gomega.Expect(managedPage.State).To(gomega.Equal(GPUResident))

		pte, found := pageTable.Find(ctx.pid, base)
		gomega.Expect(found).To(gomega.BeTrue())
		gomega.Expect(pte.DeviceID).To(gomega.Equal(uvmDeviceID))
		gomega.Expect(pte.RemoteAccessible).To(gomega.BeFalse())
	})

	// Spec 16: a region already owned by a transaction swallows the
	// notification instead of creating a duplicate migration.
	ginkgo.It("ignores an access-counter notification while migrating", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		regionBase := uint64(ptr) &^ (testRegionSize - 1)

		region := driver.uvm.regions[RegionKey{
			PID: ctx.pid, Base: regionBase, DeviceID: uvmDeviceID,
		}]
		region.Phase = RegionMigratingToGPU

		driver.uvm.onAccessCounterNotify(ctx.pid, regionBase, uvmDeviceID)

		gomega.Expect(driver.uvm.stats.AccessCounterSuppressed).
			To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.CPUToGPUMigrations).
			To(gomega.Equal(uint64(0)))
	})

	// Spec 17: managed allocations may exceed GPU capacity, and residency must
	// stay inside the budget.
	ginkgo.It("enforces GPU capacity with eviction", func() {
		const capacityBytes = 128 * 1024 * 1024

		ptr := driver.AllocateManaged(ctx, 192*1024*1024)
		base := uint64(ptr)

		for off := uint64(0); off < 192*1024*1024; off += testRegionSize {
			driver.uvm.onAccessCounterNotify(
				ctx.pid, (base+off)&^(testRegionSize-1), uvmDeviceID)
			engine.Run()
		}

		gomega.Expect(driver.uvm.stats.Evictions).
			To(gomega.BeNumerically(">", 0))

		residentBytes := driver.uvm.stats.GPUResidentPages *
			driver.uvm.config.PageSize
		gomega.Expect(residentBytes).
			To(gomega.BeNumerically("<=", uint64(capacityBytes)))
	})

	// Spec 1.2 and 10.4: ideal mode zeroes the timing but keeps the counters.
	ginkgo.It("keeps ideal-uvm timing at zero", func() {
		gomega.Expect(driver.uvm.config.faultHandlingCycles()).
			To(gomega.Equal(0))

		ptr := driver.AllocateManaged(ctx, 128*1024)
		driver.uvm.onAccessCounterNotify(
			ctx.pid, uint64(ptr)&^(testRegionSize-1), uvmDeviceID)
		engine.Run()

		gomega.Expect(driver.uvm.stats.FaultHandlingTime).
			To(gomega.Equal(sim.VTimeInSec(0)))
		gomega.Expect(driver.uvm.stats.MigrationTime).
			To(gomega.Equal(sim.VTimeInSec(0)))
		gomega.Expect(driver.uvm.stats.MigratedBytes).
			To(gomega.BeNumerically(">", 0))
	})
})
