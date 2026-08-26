package driver

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

// A Builder can build a driver.
type Builder struct {
	engine              sim.Engine
	freq                sim.Freq
	log2PageSize        uint64
	pageTable           vm.PageTable
	gpuPageTables       []vm.PageTable // sbin_codex: per-GPU tables managed by the driver allocator.
	globalStorage       *mem.Storage
	useMagicMemoryCopy  bool
	middlewareD2HCycles int
	middlewareH2DCycles int

	// sbin_codex: UVM demand-paging configuration.
	uvmConfig UVMConfig

	// sbin_claude_utopia: Utopia RestSeg configuration.
	utopiaConfig UtopiaConfig

	// sbin_claude_avatar: Avatar metadata/placement configuration.
	avatarConfig AvatarConfig
}

// MakeBuilder creates a driver builder with some default configuration
// parameters.
func MakeBuilder() Builder {
	return Builder{
		freq: 1 * sim.GHz,
	}
}

// WithEngine sets the engine to use.
func (b Builder) WithEngine(e sim.Engine) Builder {
	b.engine = e
	return b
}

// WithFreq sets the frequency to use.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithPageTable sets the global page table.
func (b Builder) WithPageTable(pt vm.PageTable) Builder {
	b.pageTable = pt
	return b
}

// WithGPUPageTables sets the per-GPU page tables managed by the driver. // sbin_codex
func (b Builder) WithGPUPageTables(pageTables []vm.PageTable) Builder {
	b.gpuPageTables = pageTables
	return b
}

// WithLog2PageSize sets the page size used by all the devices in the system
// as a power of 2.
func (b Builder) WithLog2PageSize(log2PageSize uint64) Builder {
	b.log2PageSize = log2PageSize
	return b
}

// WithGlobalStorage sets the global storage that the driver uses.
func (b Builder) WithGlobalStorage(storage *mem.Storage) Builder {
	b.globalStorage = storage
	return b
}

// WithMagicMemoryCopyMiddleware uses global storage as memory components
func (b Builder) WithMagicMemoryCopyMiddleware() Builder {
	b.useMagicMemoryCopy = true
	return b
}

func (b Builder) WithD2HCycles(d2hCycles int) Builder {
	b.middlewareD2HCycles = d2hCycles
	return b
}

func (b Builder) WithH2DCycles(h2dCycles int) Builder {
	b.middlewareH2DCycles = h2dCycles
	return b
}

// WithUVM enables UVM demand-paged managed memory on the built driver.
func (b Builder) WithUVM(config UVMConfig) Builder {
	b.uvmConfig = config
	return b
}

// WithUtopia enables Utopia RestSeg reservation and RestSeg-first allocation
// on the built driver. // sbin_claude_utopia
func (b Builder) WithUtopia(config UtopiaConfig) Builder {
	b.utopiaConfig = config
	return b
}

// WithAvatar enables Avatar embedded-metadata bookkeeping and (optionally)
// 2MB-region randomized physical placement on the built driver.
// sbin_claude_avatar
func (b Builder) WithAvatar(config AvatarConfig) Builder {
	b.avatarConfig = config
	return b
}

// Build creates a driver.
func (b Builder) Build(name string) *Driver {
	driver := new(Driver)
	driver.TickingComponent = sim.NewTickingComponent(
		"Driver", b.engine, b.freq, driver)

	driver.Log2PageSize = b.log2PageSize

	// Pre-edit code (commented per project convention):
	// memAllocatorImpl := internal.NewMemoryAllocator(b.pageTable, b.log2PageSize)
	// for i, pageTable := range b.gpuPageTables { // sbin_codex: GPU IDs are 1-based.
	// 	memAllocatorImpl.RegisterPageTable(i+1, pageTable)
	// }
	// driver.memAllocator = memAllocatorImpl
	//
	// sbin_claude_utopia: allocator construction moved into a helper so the
	// Utopia registry hookup lives next to the page-table registration.
	b.createMemAllocator(driver)

	// Pre-edit code (commented per project convention):
	// distributorImpl := newDistributorImpl(memAllocatorImpl)
	distributorImpl := newDistributorImpl(driver.memAllocator) // sbin_claude_utopia
	distributorImpl.pageSizeAsPowerOf2 = b.log2PageSize
	driver.distributor = distributorImpl

	driver.pageTable = b.pageTable
	driver.globalStorage = b.globalStorage

	if b.useMagicMemoryCopy {
		globalStorageMemoryCopyMiddleware := &globalStorageMemoryCopyMiddleware{
			driver: driver,
		}
		driver.middlewares = append(driver.middlewares, globalStorageMemoryCopyMiddleware)
	} else {
		defaultMemoryCopyMiddleware := &defaultMemoryCopyMiddleware{
			driver:       driver,
			cyclesPerD2H: b.middlewareD2HCycles,
			cyclesPerH2D: b.middlewareH2DCycles,
		}
		driver.middlewares = append(driver.middlewares, defaultMemoryCopyMiddleware)
	}

	driver.gpuPort = sim.NewPort(driver, 40960000, 40960000, "Driver.ToGPUs")
	driver.AddPort("GPU", driver.gpuPort)
	driver.mmuPort = sim.NewPort(driver, 1, 1, "Driver.ToMMU")
	driver.AddPort("MMU", driver.mmuPort)

	// sbin_codex: construct the UVM manager and its fault port when enabled.
	if b.uvmConfig.Enabled {
		config := b.uvmConfig
		config.Log2PageSize = b.log2PageSize
		config.GPUCoreFrequency = b.freq
		config.PageSize = uint64(1) << b.log2PageSize
		if config.RegionSize == 0 {
			config.RegionSize = 64 * 1024
		}
		if config.VABlockSize == 0 {
			config.VABlockSize = 2 * 1024 * 1024
		}
		if config.TBNMaxFetchSize == 0 {
			config.TBNMaxFetchSize = config.VABlockSize
		}
		driver.uvm = newUVMManager(driver, config)
		driver.uvmPort = sim.NewPort(driver, 4096, 4096, "Driver.UVM")
		driver.AddPort("UVM", driver.uvmPort)
	}

	driver.enqueueSignal = make(chan bool)
	driver.driverStopped = make(chan bool)
	driver.codeObjGPUAddrs = make(map[*insts.KernelCodeObject]Ptr)

	b.createCPU(driver)

	return driver
}

// createMemAllocator builds the driver allocator, registers every GPU page
// table, and attaches the shared RestSeg state when Utopia is enabled so
// allocations can try the RestSeg first; RegisterGPU performs the per-GPU
// reservation. // sbin_claude_utopia
func (b Builder) createMemAllocator(driver *Driver) {
	memAllocatorImpl := internal.NewMemoryAllocator(b.pageTable, b.log2PageSize)
	for i, pageTable := range b.gpuPageTables { // sbin_codex: GPU IDs are 1-based.
		memAllocatorImpl.RegisterPageTable(i+1, pageTable)
	}
	driver.memAllocator = memAllocatorImpl

	// sbin_claude_avatar: attach the shared Avatar metadata registry so the
	// allocator installs/invalidates embedded page metadata and (with the
	// fragmentation model) places frames at 2MB-region granularity.
	if b.avatarConfig.Enabled {
		if b.avatarConfig.Registry == nil {
			panic("avatar: driver requires a metadata registry")
		}

		driver.avatarConfig = b.avatarConfig
		memAllocatorImpl.SetAvatarRegistry(
			b.avatarConfig.Registry, b.avatarConfig.FragEnabled)
	}

	if !b.utopiaConfig.Enabled {
		return
	}
	if b.utopiaConfig.Registry == nil {
		panic("utopia: driver requires a RestSeg registry")
	}

	driver.utopiaConfig = b.utopiaConfig
	memAllocatorImpl.SetUtopiaRegistry(b.utopiaConfig.Registry)
}

func (b *Builder) createCPU(d *Driver) {
	cpu := &internal.Device{
		ID:       0,
		Type:     internal.DeviceTypeCPU,
		MemState: internal.NewDeviceMemoryState(d.Log2PageSize),
	}
	cpu.SetTotalMemSize(4 * mem.GB)

	d.memAllocator.RegisterDevice(cpu)
	d.devices = append(d.devices, cpu)
}
