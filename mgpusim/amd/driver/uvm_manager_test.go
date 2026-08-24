package driver

import (
	"sync" // sbin_codex: coordinate simultaneous migration completions in the race regression.

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

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
		driver = MakeBuilder().
			WithEngine(engine).
			WithLog2PageSize(12).
			WithPageTable(pageTable).
			WithGlobalStorage(mem.NewStorage(1 << 30)).
			WithD2HCycles(1).
			WithH2DCycles(1).
			WithUVM(UVMConfig{
				Enabled:                true,
				Ideal:                  true,
				FaultLatencyUS:         20,
				AccessCounterThreshold: 64,
				TBNExpandRatio:         0.51,
				TBNMaxFetchSize:        2 * 1024 * 1024,
				GPUCapacityBytes:       128 * 1024 * 1024,
			}).
			Build("Driver")
		driver.RegisterGPU(
			sim.NewPort(driver, 8, 8, "TestGPU.CP"),
			DeviceProperties{CUCount: 4, DRAMSize: 4 * 1024 * 1024 * 1024},
		)
		// Wire the driver GPU port and the registered GPU through a direct
		// connection so eviction shootdown sends resolve without a platform.
		conn := directconnection.MakeBuilder().
			WithEngine(engine).
			WithFreq(1 * sim.GHz).
			Build("TestGPU.Conn")
		conn.PlugIn(driver.gpuPort)
		conn.PlugIn(driver.GPUs[0])
		ctx = driver.Init()
	})

	ginkgo.It("should register a managed allocation with CPU residency", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		gomega.Expect(uint64(ptr)).To(gomega.BeNumerically(">", 0))

		base := uint64(ptr)
		mp := driver.uvm.pages[PageKey{PID: ctx.pid, VAddr: base}]
		gomega.Expect(mp).NotTo(gomega.BeNil())
		gomega.Expect(mp.State).To(gomega.Equal(CPUResident))
		gomega.Expect(mp.GPUFrameValid).To(gomega.BeFalse())

		gomega.Expect(len(driver.uvm.pages)).To(gomega.Equal(32))
		gomega.Expect(len(driver.uvm.blocks)).To(gomega.Equal(1))
	})

	ginkgo.It("should align TBN region selection to 64KB", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		pageKey := PageKey{PID: ctx.pid, VAddr: base + 4096}
		block := driver.uvm.blocks[BlockKey{PID: ctx.pid, Base: (base + 4096) &^ (2*1024*1024 - 1)}]

		sel := driver.uvm.selectTBNRegion(pageKey, block)
		gomega.Expect(sel.regionBase % (64 * 1024)).To(gomega.Equal(uint64(0)))
		gomega.Expect(len(sel.pageKeys)).To(gomega.BeNumerically(">=", 1))
		gomega.Expect(sel.demandKey).To(gomega.Equal(pageKey))
	})

	ginkgo.It("should coalesce duplicate faults on one page", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		vAddr := base + 4096
		pid := ctx.pid

		driver.uvm.onManagedAccess(pid, vAddr, 1, "req1", "")
		driver.uvm.onManagedAccess(pid, vAddr, 1, "req2", "")
		driver.uvm.onManagedAccess(pid, vAddr, 1, "req3", "")

		gomega.Expect(driver.uvm.stats.PageFaultRequests).To(gomega.Equal(uint64(3)))
		gomega.Expect(driver.uvm.stats.UniquePageFaults).To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.CoalescedFaultReqs).To(gomega.Equal(uint64(2)))
	})

	// sbin_codex: old eager demand-migration expectations, replaced by the
	// first-touch spec below (a cold first touch must not migrate).
	// ginkgo.It("should run a demand fault through migration and replay", func() {
	// 	ptr := driver.AllocateManaged(ctx, 128*1024)
	// 	base := uint64(ptr)
	// 	vAddr := base + 4096
	// 	pid := ctx.pid
	//
	// 	driver.uvm.onManagedAccess(pid, vAddr, 1, "req1", "")
	// 	engine.Run()
	//
	// 	gomega.Expect(driver.uvm.stats.UniquePageFaults).To(gomega.Equal(uint64(1)))
	// 	gomega.Expect(driver.uvm.stats.CPUToGPUMigrations).To(gomega.Equal(uint64(1)))
	// 	// The demanded page's 64KB region is fetched; the region contains up to
	// 	// 16 pages but the allocation starts at 4KB so the first region holds 15.
	// 	gomega.Expect(driver.uvm.stats.MigratedBytes).To(gomega.BeNumerically(">=", 15*4096))
	//
	// 	mp := driver.uvm.pages[PageKey{PID: pid, VAddr: vAddr}]
	// 	gomega.Expect(mp.State).To(gomega.Equal(GPUResident))
	// 	gomega.Expect(mp.GPUFrameValid).To(gomega.BeTrue())
	// })

	// sbin_codex: a cold page's first touch must NOT migrate. The GPU reads it
	// remotely, so the page stays CPU-resident with a DeviceID=0/CPU-backed/
	// RemoteAccessible PTE and no migration stats move. Migration is deferred to
	// the access-counter path.
	ginkgo.It("should leave a cold page CPU-resident on its first-touch", func() {
		// Given: a cold managed allocation whose first 4KB page was never
		// accessed, migrated, or evicted.
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		pid := ctx.pid
		mp := driver.uvm.pages[PageKey{PID: pid, VAddr: base}]
		gomega.Expect(mp).NotTo(gomega.BeNil())
		gomega.Expect(mp.TimesMigrated).To(gomega.Equal(uint64(0)))

		// When: the GPU first touches the page and the engine drains the fault.
		driver.uvm.onManagedAccess(pid, base, 1, "req1", "")
		engine.Run()

		// Then: the page remains CPU-resident and remotely accessible; no
		// migration was performed.
		gomega.Expect(mp.State).To(gomega.Equal(CPUResident))
		gomega.Expect(mp.GPUFrameValid).To(gomega.BeFalse())
		gomega.Expect(mp.TimesMigrated).To(gomega.Equal(uint64(0)))
		gomega.Expect(driver.uvm.stats.CPUToGPUMigrations).To(gomega.Equal(uint64(0)))
		gomega.Expect(driver.uvm.stats.MigratedPages).To(gomega.Equal(uint64(0)))

		pte, ok := pageTable.Find(pid, base)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(pte.DeviceID).To(gomega.Equal(uint64(0)))
		gomega.Expect(pte.PAddr).To(gomega.Equal(mp.CPUBackingPAddr))
		gomega.Expect(pte.RemoteAccessible).To(gomega.BeTrue())
	})

	// sbin_codex: the access-counter path must migrate a hot region even when
	// its pages were never evicted (TimesMigrated stays 0). The first touch
	// leaves the page CPU-resident; the counter notification then migrates it.
	ginkgo.It("should migrate a never-evicted region on access-counter notify", func() {
		// Given: a cold managed allocation; the first touch must not migrate.
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		pid := ctx.pid
		regionBase := base &^ (64*1024 - 1)

		driver.uvm.onManagedAccess(pid, base, 1, "req1", "")
		engine.Run()
		mp := driver.uvm.pages[PageKey{PID: pid, VAddr: base}]
		gomega.Expect(mp.TimesMigrated).To(gomega.Equal(uint64(0)))

		// When: the GPU GMMU reaches the access-counter threshold for the
		// page's 64KB region and notifies the driver.
		driver.uvm.onAccessCounterNotify(pid, regionBase, 1)
		engine.Run()
		drainUVMTestQuiescence(driver, engine) // sbin_codex

		// Then: the region migrates to the GPU without any prior eviction.
		gomega.Expect(driver.uvm.stats.AccessCounterMigr).To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.CPUToGPUMigrations).To(gomega.Equal(uint64(1)))
		gomega.Expect(mp.State).To(gomega.Equal(GPUResident))
		gomega.Expect(mp.TimesMigrated).To(gomega.Equal(uint64(0)))
	})

	// sbin_codex: completion must clear demand ownership even when overlap leaves
	// the demand page outside the migration's reserved page list.
	ginkgo.It("should clear demand ownership when a migration reserved no page", func() {
		// Given
		ptr := driver.AllocateManaged(ctx, 128*1024)
		demand := PageKey{PID: ctx.pid, VAddr: uint64(ptr) + 4096}
		mig := &Migration{ID: "overlapped-migration", DeviceID: 1}
		driver.uvm.migrations[mig.ID] = mig
		driver.uvm.pageToMig[demand] = mig.ID
		gomega.Expect(mig.Pages).To(gomega.BeEmpty())

		// When
		driver.uvm.completeMigration(mig.ID)

		// Then
		gomega.Expect(driver.uvm.pageToMig).To(gomega.BeEmpty())
	})

	// sbin_codex: parallel-engine migration events must serialize every mutation
	// of the shared UVM maps, LRU list, page state, and statistics.
	ginkgo.It("should complete migrations concurrently without racing", func() {
		const migrationCount = 64
		ptr := driver.AllocateManaged(ctx, migrationCount*4096)
		start := make(chan struct{})
		var wg sync.WaitGroup

		for i := 0; i < migrationCount; i++ {
			pk := PageKey{PID: ctx.pid, VAddr: uint64(ptr) + uint64(i)*4096}
			migID := "parallel-migration-" + itoa(uint64(i))
			driver.uvm.migrations[migID] = &Migration{
				ID:       migID,
				DeviceID: 1,
				Pages:    []PageKey{pk},
			}
			driver.uvm.pageToMig[pk] = migID

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				driver.uvm.completeMigration(migID)
			}()
		}

		close(start)
		wg.Wait()

		gomega.Expect(driver.uvm.migrations).To(gomega.BeEmpty())
		gomega.Expect(driver.uvm.pageToMig).To(gomega.BeEmpty())
	})

	ginkgo.It("should enforce GPU capacity with eviction", func() {
		// GPU capacity is 128MB; allocate 192MB and touch everything.
		ptr := driver.AllocateManaged(ctx, 192*1024*1024)
		base := uint64(ptr)
		pid := ctx.pid

		// Touch one page per 64KB region, letting each fault complete before
		// the next so eviction can find eligible (fault-free) victim regions.
		// sbin_codex: first touch remote-maps the page; the explicit counter
		// notification then migrates the region and exercises capacity.
		for off := uint64(0); off < 192*1024*1024; off += 64 * 1024 {
			driver.uvm.onManagedAccess(pid, base+off, 1, "req", "")
			engine.Run()
			driver.uvm.onAccessCounterNotify(pid, (base+off)&^(64*1024-1), 1) // sbin_codex
			engine.Run()                                                      // sbin_codex
			// sbin_codex: simulate the GPU TLB-shootdown ACK so the reserved
			// eviction finalizes and the pending migration resumes.
			// if driver.uvm.hasPendingEvictions() { // sbin_codex: all UVM shootdowns now require ACKs.
			// 	driver.uvm.finalizeEviction()
			// 	engine.Run()
			// }
			drainUVMTestQuiescence(driver, engine) // sbin_codex
		}

		gomega.Expect(driver.uvm.stats.Evictions).To(gomega.BeNumerically(">", 0))
		residentBytes := driver.uvm.stats.GPUResidentPages * driver.uvm.config.PageSize
		gomega.Expect(residentBytes).To(gomega.BeNumerically("<=", 128*1024*1024))
	})

	ginkgo.It("should migrate a region on access-counter notification", func() {
		ptr := driver.AllocateManaged(ctx, 128*1024)
		base := uint64(ptr)
		pid := ctx.pid
		regionBase := base &^ (64*1024 - 1)

		// Simulate the GPU GMMU reaching its counter threshold and notifying
		// the driver.
		driver.uvm.onAccessCounterNotify(pid, regionBase, 1)
		engine.Run()
		drainUVMTestQuiescence(driver, engine) // sbin_codex

		gomega.Expect(driver.uvm.stats.AccessCounterNotif).To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.AccessCounterMigr).To(gomega.Equal(uint64(1)))
		gomega.Expect(driver.uvm.stats.CPUToGPUMigrations).To(gomega.Equal(uint64(1)))

		mp := driver.uvm.pages[PageKey{PID: pid, VAddr: base}]
		gomega.Expect(mp.State).To(gomega.Equal(GPUResident))
	})

	ginkgo.It("should keep ideal-uvm timing zero", func() {
		gomega.Expect(driver.uvm.config.faultHandlingCycles()).To(gomega.Equal(0))
	})

	ginkgo.It("should queue capacity requests during a pending eviction", func() {
		// GPU capacity is 128MB; touching a 192MB working set forces
		// evictions. Two faults arriving while an eviction is pending must
		// queue and resume in order without stalling.
		ptr := driver.AllocateManaged(ctx, 192*1024*1024)
		base := uint64(ptr)
		pid := ctx.pid

		for off := uint64(0); off < 128*1024*1024; off += 64 * 1024 {
			driver.uvm.onManagedAccess(pid, base+off, 1, "req", "")
			engine.Run()
			driver.uvm.onAccessCounterNotify(pid, (base+off)&^(64*1024-1), 1) // sbin_codex
			engine.Run()                                                      // sbin_codex
			// if driver.uvm.hasPendingEvictions() { // sbin_codex: all UVM shootdowns now require ACKs.
			// 	driver.uvm.finalizeEviction()
			// 	engine.Run()
			// }
			drainUVMTestQuiescence(driver, engine) // sbin_codex
		}
		// Beyond capacity, consecutive faults must queue behind the pending
		// eviction instead of being dropped.
		for off := uint64(128 * 1024 * 1024); off < 192*1024*1024; off += 64 * 1024 {
			driver.uvm.onManagedAccess(pid, base+off, 1, "req", "")
			engine.Run()
			driver.uvm.onAccessCounterNotify(pid, (base+off)&^(64*1024-1), 1) // sbin_codex
			engine.Run()                                                      // sbin_codex
			// if driver.uvm.hasPendingEvictions() { // sbin_codex: all UVM shootdowns now require ACKs.
			// 	driver.uvm.finalizeEviction()
			// 	engine.Run()
			// }
			drainUVMTestQuiescence(driver, engine) // sbin_codex
		}

		gomega.Expect(driver.uvm.stats.Evictions).To(gomega.BeNumerically(">", 0))
		residentBytes := driver.uvm.stats.GPUResidentPages * driver.uvm.config.PageSize
		gomega.Expect(residentBytes).To(gomega.BeNumerically("<=", 128*1024*1024))
	})

	ginkgo.It("should count repeated migrations under thrashing", func() {
		// GPU capacity is 128MB; two 96MB working sets exceed it together.
		// Alternating accesses force evictions. Evicted pages are remotely
		// accessible, so re-migration is driven by the access counter: each
		// region is touched AccessCounterThreshold times per pass.
		ptr := driver.AllocateManaged(ctx, 192*1024*1024)
		base := uint64(ptr)
		pid := ctx.pid
		// thresh := driver.uvm.config.AccessCounterThreshold
		// sbin_codex: direct driver accesses do not model GPU-side counting.

		for pass := 0; pass < 2; pass++ {
			for off := uint64(0); off < 96*1024*1024; off += 64 * 1024 {
				// for i := uint64(0); i < thresh; i++ { // sbin_codex: replaced by first touch plus threshold notification.
				// 	driver.uvm.onManagedAccess(pid, base+off, 1, "req", "")
				// }
				mp := driver.uvm.pages[PageKey{PID: pid, VAddr: base + off}] // sbin_codex
				if !mp.RemoteMapped {                                        // sbin_codex: only cold pages need first-touch setup.
					driver.uvm.onManagedAccess(pid, base+off, 1, "req", "") // sbin_codex
					engine.Run()                                            // sbin_codex
				}
				driver.uvm.onAccessCounterNotify(pid, (base+off)&^(64*1024-1), 1) // sbin_codex
				engine.Run()                                                      // sbin_codex
				// if driver.uvm.hasPendingEvictions() { // sbin_codex: all UVM shootdowns now require ACKs.
				// 	driver.uvm.finalizeEviction()
				// 	engine.Run()
				// }
				drainUVMTestQuiescence(driver, engine) // sbin_codex
			}
			for off := uint64(96 * 1024 * 1024); off < 192*1024*1024; off += 64 * 1024 {
				// for i := uint64(0); i < thresh; i++ { // sbin_codex: replaced by first touch plus threshold notification.
				// 	driver.uvm.onManagedAccess(pid, base+off, 1, "req", "")
				// }
				mp := driver.uvm.pages[PageKey{PID: pid, VAddr: base + off}] // sbin_codex
				if !mp.RemoteMapped {                                        // sbin_codex: only cold pages need first-touch setup.
					driver.uvm.onManagedAccess(pid, base+off, 1, "req", "") // sbin_codex
					engine.Run()                                            // sbin_codex
				}
				driver.uvm.onAccessCounterNotify(pid, (base+off)&^(64*1024-1), 1) // sbin_codex
				engine.Run()                                                      // sbin_codex
				// if driver.uvm.hasPendingEvictions() { // sbin_codex: all UVM shootdowns now require ACKs.
				// 	driver.uvm.finalizeEviction()
				// 	engine.Run()
				// }
				drainUVMTestQuiescence(driver, engine) // sbin_codex
			}
		}

		gomega.Expect(driver.uvm.stats.Evictions).To(gomega.BeNumerically(">", 0))
		gomega.Expect(driver.uvm.stats.RepeatedMigrations).To(gomega.BeNumerically(">", 0))
	})
})
