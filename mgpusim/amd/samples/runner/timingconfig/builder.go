// Package timingconfig contains the configuration for the timing simulation.
package timingconfig

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/noc/networking/pcie"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	ideall1tlb "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/ideal-l1tlb" // sbin_codex: ideal-L1-TLB GPU builder
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
	virtualcaching "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/virtual-caching" // sbin_codex: virtual-caching GPU builder
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"  // sbin_codex (todo 24): UVM CP seam wiring.
	"github.com/sarchlab/mgpusim/v4/amd/timing/rdma" // sbin_codex (todo 24): remote endpoint CPU-memory seam.
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"  // sbin_codex: GPU-wide AccessCounter + CPU-remote endpoint (todo 11).
)

// Builder builds a hardware platform for timing simulation.
type Builder struct {
	simulation *simulation.Simulation

	numGPUs            int
	numCUPerSA         int
	numSAPerGPU        int
	cpuMemSize         uint64
	gpuMemSize         uint64
	log2PageSize       uint64
	useMagicMemoryCopy bool
	gpuType            string
	switchLatency      int // PCIe/interconnect switch latency in cycles
	d2hCycles          int
	h2dCycles          int

	platform          *sim.Domain
	globalStorage     *mem.Storage
	rdmaAddressMapper *mem.BankedAddressPortMapper
	cpuPageTable      vm.PageTable     // sbin_codex: authoritative CPU-side page table.
	gpuPageTables     []vm.PageTable   // sbin_codex: one isolated page table per GPU GMMU.
	uvmConfig         driver.UVMConfig // sbin_codex: validated UVM config (disabled zero value by default).

	// sbin_codex (todo 11): per-GPU UVM remote-access components (GPU-wide
	// AccessCounter + CPU-remote endpoint), registered behind the access gates.
	accessCounters  []*uvm.AccessCounter
	remoteEndpoints []*uvm.RemoteEndpoint

	// sbin_codex (todo 24): the UVM scheduling adapter (uvm-manager.md §1.2).
	// ModeNormal for -uvm, ModeIdeal for -uvm -uvm-ideal; the driver builder
	// constructs the Todo-21 coordinator with this mode. The platform wiring
	// itself is mode-neutral: both modes wire the same control and remote
	// data paths (ideal changes only timing, not behavior).
	uvmMode uvm.Mode
	// gpuDomains collects the built GPU domains so the UVM remote components
	// can be wired to their CP and RDMA engine seams. // sbin_codex
	gpuDomains []*sim.Domain
}

// MakeBuilder creates a new Builder with default parameters.
func MakeBuilder() Builder {
	return Builder{
		numGPUs:            1,
		numCUPerSA:         4,
		numSAPerGPU:        16,
		cpuMemSize:         4 * mem.GB,
		gpuMemSize:         4 * mem.GB,
		log2PageSize:       12,
		useMagicMemoryCopy: false,
		gpuType:            "r9nano",
		switchLatency:      140, // default PCIe Gen4
		d2hCycles:          300,
		h2dCycles:          500,
	}
}

// WithSimulation sets the simulation to use.
func (b Builder) WithSimulation(sim *simulation.Simulation) Builder {
	b.simulation = sim
	return b
}

// WithNumGPUs sets the number of GPUs to simulate.
func (b Builder) WithNumGPUs(numGPUs int) Builder {
	b.numGPUs = numGPUs
	return b
}

// WithMagicMemoryCopy sets whether to use the magic memory copy middleware.
func (b Builder) WithMagicMemoryCopy() Builder {
	b.useMagicMemoryCopy = true
	return b
}

// WithGPUType sets the GPU type for timing simulation (r9nano, mi300a,
// ideal-l1tlb, or virtual-caching). // sbin_codex
func (b Builder) WithGPUType(gpuType string) Builder {
	b.gpuType = gpuType
	return b
}

// WithUVMConfig sets the validated UVM configuration handed to the driver
// builder. A disabled (zero-value) config leaves the driver's UVM manager
// nil. sbin_codex
func (b Builder) WithUVMConfig(cfg driver.UVMConfig) Builder {
	b.uvmConfig = cfg
	return b
}

// UVMMode returns the chosen UVM scheduling adapter (uvm-manager.md §1.2):
// ModeIdeal for -uvm -uvm-ideal, ModeNormal otherwise (including a disabled
// config). The driver builder constructs the Todo-21 coordinator with this
// mode; the platform wiring is mode-neutral. // sbin_codex
func (b Builder) UVMMode() uvm.Mode {
	if b.uvmConfig.Enabled && b.uvmConfig.Ideal {
		return uvm.ModeIdeal
	}
	return uvm.ModeNormal
}

// Build builds the hardware platform.
func (b Builder) Build() *sim.Domain {
	b.adjustConfigForGPUType()
	b.cpuGPUMemSizeMustEqual()

	b.platform = &sim.Domain{}

	b.globalStorage = mem.NewStorage(
		uint64(b.numGPUs)*b.gpuMemSize + b.cpuMemSize)

	b.createPageTables()                   // sbin_codex: construct distinct CPU/GPU page-table instances.
	mmuComp := b.createMMU(b.cpuPageTable) // sbin_codex: CPU MMU uses only the CPU table.
	gpuDriver := b.buildGPUDriver(
		b.cpuPageTable, b.gpuPageTables) // sbin_codex: driver owns CPU/GPU table synchronization.

	gpuBuilder := b.createGPUBuilder(mmuComp)
	pcieConnector, rootComplexID :=
		b.createConnection(gpuDriver, mmuComp)

	mmuComp.MigrationServiceProvider = gpuDriver.GetPortByName("MMU").AsRemote()

	b.createRDMAAddrTable()
	pmcAddressTable := b.createPMCPageTable()

	b.createGPUs(
		rootComplexID, pcieConnector,
		gpuBuilder, gpuDriver,
		pmcAddressTable)

	// sbin_codex (todo 11): register the per-GPU UVM remote-access components
	// behind the access gates when UVM is enabled.
	if b.uvmConfig.Enabled {
		// sbin_codex (todo 24): choose the normal/ideal scheduling adapter
		// and complete the UVM wiring (CP seam, RDMA/CPU-memory seam).
		b.uvmMode = b.UVMMode()
		b.createUVMRemoteComponents()
		b.wireUVMRemoteComponents()
	}

	pcieConnector.EstablishRoute()

	return b.platform
}

func (b *Builder) cpuGPUMemSizeMustEqual() {
	if b.cpuMemSize != b.gpuMemSize {
		panic("currently only support cpuMemSize == gpuMemSize")
	}
}

func (b *Builder) adjustConfigForGPUType() {
	switch b.gpuType {
	default:
		// Keep defaults for r9nano
	}
}

// createPageTables allocates one CPU page table and one page table per GPU.
// No CPU MMU or GPU GMMU shares the same table instance. // sbin_codex
func (b *Builder) createPageTables() {
	b.cpuPageTable = vm.NewPageTable(b.log2PageSize)
	b.gpuPageTables = make([]vm.PageTable, b.numGPUs)
	for i := range b.gpuPageTables {
		b.gpuPageTables[i] = vm.NewPageTable(b.log2PageSize)
	}
}

func (b *Builder) createMMU(pageTable vm.PageTable) *mmu.Comp { // sbin_codex: accept the CPU table explicitly.
	mmuBuilder := mmu.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(1 * sim.GHz).
		WithPageWalkingLatency(100).
		WithLog2PageSize(b.log2PageSize).
		WithPageTable(pageTable)

	mmuComponent := mmuBuilder.Build("MMU")

	b.simulation.RegisterComponent(mmuComponent)

	return mmuComponent
}

func (b *Builder) buildGPUDriver(
	cpuPageTable vm.PageTable,
	gpuPageTables []vm.PageTable, // sbin_codex: supply all page tables to the driver.
) *driver.Driver {
	gpuDriverBuilder := driver.MakeBuilder()

	if b.useMagicMemoryCopy {
		gpuDriverBuilder = gpuDriverBuilder.WithMagicMemoryCopyMiddleware()
	}

	gpuDriver := gpuDriverBuilder.
		WithEngine(b.simulation.GetEngine()).
		WithPageTable(cpuPageTable).
		WithGPUPageTables(gpuPageTables). // sbin_codex: driver registers GPU tables with its allocator.
		WithLog2PageSize(b.log2PageSize).
		WithGlobalStorage(b.globalStorage).
		WithD2HCycles(b.d2hCycles).
		WithH2DCycles(b.h2dCycles).
		WithUVMConfig(b.uvmConfig).                             // sbin_codex: validated UVM config (disabled => nil manager).
		WithUVMGPUMemorySize(uint64(b.numGPUs) * b.gpuMemSize). // sbin_codex: total GPU DRAM for capacity validation.
		Build("Driver")

	b.simulation.RegisterComponent(gpuDriver)

	return gpuDriver
}

func (b *Builder) createGPUBuilder(
	mmuComponent *mmu.Comp,
) gpubuilder.GPUBuilder {
	b.createRDMAAddressMapper()

	switch b.gpuType {
	case "ideal-l1tlb": // sbin_codex: ideal-L1TLB GPU config (todo 7).
		return ideall1tlb.MakeBuilder().
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	case "virtual-caching": // sbin_codex: virtual-caching GPU config.
		return virtualcaching.MakeBuilder().
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	default:
		return r9nano.MakeBuilder().
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	}
}

func (b *Builder) createGPUs(
	rootComplexID int,
	pcieConnector *pcie.Connector,
	gpuBuilder gpubuilder.GPUBuilder,
	gpuDriver *driver.Driver,
	pmcAddressTable *mem.BankedAddressPortMapper,
) {
	lastSwitchID := rootComplexID
	for i := 1; i < b.numGPUs+1; i++ {
		if i%2 == 1 {
			lastSwitchID = pcieConnector.AddSwitch(rootComplexID)
		}

		gpu := b.createGPU(i, gpuBuilder, gpuDriver, pmcAddressTable,
			pcieConnector, lastSwitchID)
		b.gpuDomains = append(b.gpuDomains, gpu) // sbin_codex (todo 24): keep the domains for UVM seam wiring.
	}
}

func (b *Builder) createPMCPageTable() *mem.BankedAddressPortMapper {
	pmcAddressTable := new(mem.BankedAddressPortMapper)
	pmcAddressTable.BankSize = b.gpuMemSize
	pmcAddressTable.LowModules = append(pmcAddressTable.LowModules, "")
	return pmcAddressTable
}

func (b *Builder) createRDMAAddrTable() *mem.BankedAddressPortMapper {
	rdmaAddressTable := new(mem.BankedAddressPortMapper)
	rdmaAddressTable.BankSize = b.gpuMemSize
	rdmaAddressTable.LowModules = append(rdmaAddressTable.LowModules, "")
	return rdmaAddressTable
}

// createUVMRemoteComponents builds and registers one GPU-wide AccessCounter
// and one CPU-remote endpoint per GPU (uvm-manager.md §14, §16, §31.1). The
// components sit behind the access gates; the topology builders complete the
// CP-seam and RDMA wiring. // sbin_codex
func (b *Builder) createUVMRemoteComponents() {
	for i := 1; i < b.numGPUs+1; i++ {
		counter := uvm.MakeAccessCounterBuilder().
			WithEngine(b.simulation.GetEngine()).
			WithFreq(1 * sim.GHz).
			Build(fmt.Sprintf("GPU[%d].AccessCounter", i))
		b.simulation.RegisterComponent(counter)

		endpoint := uvm.MakeRemoteEndpointBuilder().
			WithEngine(b.simulation.GetEngine()).
			WithFreq(1 * sim.GHz).
			WithGPU(i).
			WithAccessCounter(counter).
			Build(fmt.Sprintf("GPU[%d].RemoteEndpoint", i))
		b.simulation.RegisterComponent(endpoint)

		b.accessCounters = append(b.accessCounters, counter)
		b.remoteEndpoints = append(b.remoteEndpoints, endpoint)
	}
}

// wireUVMRemoteComponents completes the UVM platform wiring (plan todo 24 of
// mgpusim-uvm-manager):
//
//   - counter.ToCP = CP.ToAccessCounter (shared port): the acknowledged
//     kernel-launch reset barrier and the threshold notifications flow
//     loop-back through the GPU internal connection (uvm-manager.md §14.2),
//   - endpoint.ToRDMA = RDMA.RDMARequestInside (shared port): CPU-remote
//     reads are forwarded over modeled PCIe to CPU memory, bypassing the GPU
//     data caches (uvm-manager.md §13, §23.1).
//
// The wiring is mode-neutral: normal and ideal modes wire the same paths.
// sbin_codex
func (b *Builder) wireUVMRemoteComponents() {
	for i := 1; i < b.numGPUs+1; i++ {
		counter := b.accessCounters[i-1]
		endpoint := b.remoteEndpoints[i-1]
		gpu := b.gpuDomains[i-1]

		commandProcessor := gpu.GetPortByName("CommandProcessor").
			Component().(*cp.CommandProcessor)
		counter.ToCP = commandProcessor.ToAccessCounter

		rdmaEngine := gpu.GetPortByName("RDMAData").
			Component().(*rdma.Comp)
		endpoint.ToRDMA = rdmaEngine.RDMARequestInside
	}
}

func (b *Builder) createConnection(
	gpuDriver *driver.Driver,
	mmuComponent *mmu.Comp,
) (*pcie.Connector, int) {
	// connection := sim.NewDirectConnection(engine)
	// connection := noc.NewFixedBandwidthConnection(32, engine, 1*sim.GHz)
	// connection.SrcBufferCapacity = 40960000
	pcieConnector := pcie.NewConnector().
		WithEngine(b.simulation.GetEngine()).
		WithVersion(4, 16).
		WithSwitchLatency(b.switchLatency)

	pcieConnector.CreateNetwork("PCIe")
	rootComplexID := pcieConnector.AddRootComplex(
		[]sim.Port{
			gpuDriver.GetPortByName("GPU"),
			gpuDriver.GetPortByName("MMU"),
			mmuComponent.GetPortByName("Migration"),
			mmuComponent.GetPortByName("Top"),
		})

	return pcieConnector, rootComplexID
}

func (b *Builder) createRDMAAddressMapper() {
	b.rdmaAddressMapper = new(mem.BankedAddressPortMapper)
	b.rdmaAddressMapper.BankSize = b.gpuMemSize
	b.rdmaAddressMapper.LowModules = append(b.rdmaAddressMapper.LowModules,
		sim.RemotePort("CPU"))
}

func (b *Builder) createGPU(
	index int,
	gpuBuilder gpubuilder.GPUBuilder,
	gpuDriver *driver.Driver,
	pmcAddressTable *mem.BankedAddressPortMapper,
	pcieConnector *pcie.Connector,
	pcieSwitchID int,
) *sim.Domain {
	name := fmt.Sprintf("GPU[%d]", index)
	memAddrOffset := uint64(index) * b.gpuMemSize
	gpu := gpuBuilder.
		WithGPUID(uint64(index)).
		WithMemAddrOffset(memAddrOffset).
		WithRDMAAddressMapper(b.rdmaAddressMapper).
		WithPageTable(b.gpuPageTables[index-1]). // sbin_codex: bind GPU N to its driver-managed table.
		Build(name)

	gpuDriver.RegisterGPU(
		gpu.GetPortByName("CommandProcessor"),
		driver.DeviceProperties{
			CUCount:  b.numCUPerSA * b.numSAPerGPU,
			DRAMSize: b.gpuMemSize,
		},
	)
	// gpu.CommandProcessor.Driver = gpuDriver.GetPortByName("GPU")

	b.configRDMAEngine(gpu)
	// b.configPMC(gpu, gpuDriver, pmcAddressTable)

	pcieConnector.PlugInDevice(pcieSwitchID, gpu.Ports())

	// b.gpus = append(b.gpus, gpu)

	return gpu
}

func (b *Builder) configRDMAEngine(
	gpu *sim.Domain,
) {
	b.rdmaAddressMapper.LowModules = append(
		b.rdmaAddressMapper.LowModules,
		gpu.GetPortByName("RDMAData").AsRemote())
}

// func (b *Builder) configPMC(
// 	gpu *GPU,
// 	gpuDriver *driver.Driver,
// 	addrTable *mem.BankedAddressPortMapper,
// ) {
// 	gpu.PMC.RemotePMCAddressTable = addrTable
// 	addrTable.LowModules = append(
// 		addrTable.LowModules,
// 		gpu.PMC.GetPortByName("Remote").AsRemote())
// 	gpuDriver.RemotePMCPorts = append(
// 		gpuDriver.RemotePMCPorts, gpu.PMC.GetPortByName("Remote"))
// }
