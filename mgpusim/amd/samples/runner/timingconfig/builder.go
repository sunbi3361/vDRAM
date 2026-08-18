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
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
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
	cpuPageTable      vm.PageTable   // sbin_codex: authoritative CPU-side page table.
	gpuPageTables     []vm.PageTable // sbin_codex: one isolated page table per GPU GMMU.
}

// splitPageTable is the driver-facing page-table view. The v4 driver accepts
// one page table, so this view mirrors allocator mutations into the separate
// CPU and GPU tables without changing code outside runner. // sbin_codex
type splitPageTable struct {
	cpu  vm.PageTable
	gpus []vm.PageTable
}

// Insert adds a page to the CPU table and every GPU table that can access it.
// Unified pages are visible to every GPU; private pages are visible only to
// their owning GPU. // sbin_codex
func (pt *splitPageTable) Insert(page vm.Page) {
	pt.cpu.Insert(page)
	for gpuIndex, gpuPageTable := range pt.gpus {
		if pt.pageBelongsToGPU(page, gpuIndex) {
			gpuPageTable.Insert(page)
		}
	}
}

// Remove removes a page from the CPU table and from the GPU tables that held
// the previous mapping. // sbin_codex
func (pt *splitPageTable) Remove(pid vm.PID, vAddr uint64) {
	page, found := pt.cpu.Find(pid, vAddr)
	if !found {
		pt.cpu.Remove(pid, vAddr)
		return
	}

	pt.cpu.Remove(pid, vAddr)
	for gpuIndex, gpuPageTable := range pt.gpus {
		if pt.pageBelongsToGPU(page, gpuIndex) {
			gpuPageTable.Remove(pid, page.VAddr)
		}
	}
}

// Find reads the authoritative CPU-side mapping. // sbin_codex
func (pt *splitPageTable) Find(pid vm.PID, addr uint64) (vm.Page, bool) {
	return pt.cpu.Find(pid, addr)
}

// Update keeps membership correct when migration changes a page's owner.
// Existing memberships are updated, departed memberships are removed, and
// newly reachable GPU tables receive an insert. // sbin_codex
func (pt *splitPageTable) Update(page vm.Page) {
	oldPage, found := pt.cpu.Find(page.PID, page.VAddr)
	if !found {
		pt.cpu.Update(page)
		return
	}

	pt.cpu.Update(page)
	for gpuIndex, gpuPageTable := range pt.gpus {
		hadPage := pt.pageBelongsToGPU(oldPage, gpuIndex)
		hasPage := pt.pageBelongsToGPU(page, gpuIndex)
		switch {
		case hadPage && hasPage:
			gpuPageTable.Update(page)
		case hadPage:
			gpuPageTable.Remove(oldPage.PID, oldPage.VAddr)
		case hasPage:
			gpuPageTable.Insert(page)
		}
	}
}

// ReverseLookup reads the authoritative CPU-side mapping. // sbin_codex
func (pt *splitPageTable) ReverseLookup(pAddr uint64) (vm.Page, bool) {
	return pt.cpu.ReverseLookup(pAddr)
}

// pageBelongsToGPU applies the v2 membership rule: unified pages are shared,
// while a private GPU page appears only in its 1-based DeviceID table.
// CPU-private pages do not appear in a GPU table. // sbin_codex
func (pt *splitPageTable) pageBelongsToGPU(page vm.Page, gpuIndex int) bool {
	return page.Unified || page.DeviceID == uint64(gpuIndex+1)
}

// gpuPageTableView preserves a GPU's isolated page table while providing the
// CPU-table fallback that the v2 GMMU performs through its Bottom port. The v4
// GMMU resolves PageTable.Find directly, so the fallback belongs at this
// runner-only compatibility boundary. // sbin_codex
type gpuPageTableView struct {
	local vm.PageTable
	cpu   vm.PageTable
}

// Insert mutates only the GPU-local table. // sbin_codex
func (pt *gpuPageTableView) Insert(page vm.Page) {
	pt.local.Insert(page)
}

// Remove mutates only the GPU-local table. // sbin_codex
func (pt *gpuPageTableView) Remove(pid vm.PID, vAddr uint64) {
	pt.local.Remove(pid, vAddr)
}

// Find resolves local mappings first and falls back to the authoritative CPU
// table for remote GPU/CPU pages. // sbin_codex
func (pt *gpuPageTableView) Find(pid vm.PID, addr uint64) (vm.Page, bool) {
	if page, found := pt.local.Find(pid, addr); found {
		return page, true
	}
	return pt.cpu.Find(pid, addr)
}

// Update mutates only the GPU-local table. // sbin_codex
func (pt *gpuPageTableView) Update(page vm.Page) {
	pt.local.Update(page)
}

// ReverseLookup resolves local mappings first and then remote CPU mappings.
// sbin_codex
func (pt *gpuPageTableView) ReverseLookup(pAddr uint64) (vm.Page, bool) {
	if page, found := pt.local.ReverseLookup(pAddr); found {
		return page, true
	}
	return pt.cpu.ReverseLookup(pAddr)
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

// WithGPUType sets the GPU type for timing simulation (r9nano or mi300a).
func (b Builder) WithGPUType(gpuType string) Builder {
	b.gpuType = gpuType
	return b
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
	driverPageTable := &splitPageTable{    // sbin_codex: mirror driver mutations across the split tables.
		cpu:  b.cpuPageTable,
		gpus: b.gpuPageTables,
	}
	gpuDriver := b.buildGPUDriver(driverPageTable) // sbin_codex

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
	pageTable vm.PageTable,
) *driver.Driver {
	gpuDriverBuilder := driver.MakeBuilder()

	if b.useMagicMemoryCopy {
		gpuDriverBuilder = gpuDriverBuilder.WithMagicMemoryCopyMiddleware()
	}

	gpuDriver := gpuDriverBuilder.
		WithEngine(b.simulation.GetEngine()).
		WithPageTable(pageTable).
		WithLog2PageSize(b.log2PageSize).
		WithGlobalStorage(b.globalStorage).
		WithD2HCycles(b.d2hCycles).
		WithH2DCycles(b.h2dCycles).
		Build("Driver")

	b.simulation.RegisterComponent(gpuDriver)

	return gpuDriver
}

func (b *Builder) createGPUBuilder(
	mmuComponent *mmu.Comp,
) gpubuilder.GPUBuilder {
	b.createRDMAAddressMapper()

	switch b.gpuType {
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

		b.createGPU(i, gpuBuilder, gpuDriver, pmcAddressTable,
			pcieConnector, lastSwitchID)
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
		WithPageTable(&gpuPageTableView{ // sbin_codex: bind GPU N locally with CPU fallback for remote pages.
			local: b.gpuPageTables[index-1],
			cpu:   b.cpuPageTable,
		}).
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
