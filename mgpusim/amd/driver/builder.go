package driver

import (
	"log"

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
	uvmConfig           *UVMConfig // sbin_codex: UVM config; nil means UVM disabled.
	uvmGPUMemorySize    uint64     // sbin_codex: total GPU DRAM available to UVM capacity validation.
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

// WithUVMConfig sets the validated UVM configuration. A config with
// Enabled == false (or no call at all) leaves the driver's UVM manager nil.
// sbin_codex
func (b Builder) WithUVMConfig(cfg UVMConfig) Builder {
	b.uvmConfig = &cfg
	return b
}

// WithUVMGPUMemorySize sets the total GPU DRAM (in bytes) available to UVM
// capacity validation. The timingconfig builder supplies this from the DRAM
// sizes it owns. sbin_codex
func (b Builder) WithUVMGPUMemorySize(size uint64) Builder {
	b.uvmGPUMemorySize = size
	return b
}

// Build creates a driver.
func (b Builder) Build(name string) *Driver {
	driver := new(Driver)
	driver.TickingComponent = sim.NewTickingComponent(
		"Driver", b.engine, b.freq, driver)

	driver.Log2PageSize = b.log2PageSize

	memAllocatorImpl := internal.NewMemoryAllocator(b.pageTable, b.log2PageSize)
	for i, pageTable := range b.gpuPageTables { // sbin_codex: GPU IDs are 1-based.
		memAllocatorImpl.RegisterPageTable(i+1, pageTable)
	}
	driver.memAllocator = memAllocatorImpl

	distributorImpl := newDistributorImpl(memAllocatorImpl)
	distributorImpl.pageSizeAsPowerOf2 = b.log2PageSize
	driver.distributor = distributorImpl

	driver.pageTable = b.pageTable
	driver.globalStorage = b.globalStorage

	// sbin_codex (todo 15): wire the FIFO fault service before the copy
	// middleware so it consumes its own DMA/replay responses first.
	if b.uvmConfig != nil && b.uvmConfig.Enabled {
		driver.uvmFault = &faultServiceMiddleware{driver: driver}
		driver.middlewares = append(driver.middlewares, driver.uvmFault)
	}

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
		// sbin_codex (todo 5): wire the managed copy handler when UVM is
		// enabled so copies branch by allocation.
		if b.uvmConfig != nil && b.uvmConfig.Enabled {
			defaultMemoryCopyMiddleware.managed = &managedMemoryCopyMiddleware{
				driver: driver,
			}
		}
		driver.middlewares = append(driver.middlewares, defaultMemoryCopyMiddleware)
	}

	driver.gpuPort = sim.NewPort(driver, 40960000, 40960000, "Driver.ToGPUs")
	driver.AddPort("GPU", driver.gpuPort)
	driver.mmuPort = sim.NewPort(driver, 1, 1, "Driver.ToMMU")
	driver.AddPort("MMU", driver.mmuPort)

	driver.enqueueSignal = make(chan bool)
	driver.driverStopped = make(chan bool)
	driver.codeObjGPUAddrs = make(map[*insts.KernelCodeObject]Ptr)

	b.createCPU(driver)

	// sbin_codex: construct the driver-owned UVM manager only when the UVM
	// config is enabled; otherwise driver.uvm stays nil (disabled mode, where
	// AllocateManagedMemory is the only rejection path).
	if b.uvmConfig != nil && b.uvmConfig.Enabled {
		if err := b.uvmConfig.Validate(); err != nil {
			log.Panic(err)
		}
		if err := b.uvmConfig.ValidateCapacity(b.uvmGPUMemorySize); err != nil {
			log.Panic(err)
		}
		driver.uvm = NewUVMManager(*b.uvmConfig, b.uvmGPUMemorySize)
		// sbin_codex (todo 16): the driver allocates migration destination
		// frames from its registered GPU devices.
		driver.uvm.SetFrameAllocator(driver)
	}

	return driver
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
