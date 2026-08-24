// Package r9nano contains the configuration of GPUs similar to AMD Radeon R9
// Nano.
package r9nano

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/writeback"
	"github.com/sarchlab/akita/v4/mem/dram"
	"github.com/sarchlab/akita/v4/mem/idealmemcontroller"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"                   // sbin_codex: per-GPU page-table ownership.
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator" // sbin_codex: L2-boundary translation for virtual caching.
	"github.com/sarchlab/akita/v4/mem/vm/gmmu"              // sbin_codex: GPU-side page-table walker.
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/shaderarray"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
	"github.com/sarchlab/mgpusim/v4/amd/timing/pagemigrationcontroller"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rdma"
)

// Builder builds a hardware platform for timing simulation.
type Builder struct {
	simulation *simulation.Simulation

	gpuID                          uint64
	name                           string
	freq                           sim.Freq
	numCUPerShaderArray            int
	numShaderArray                 int
	l2CacheSize                    uint64
	numMemoryBank                  int
	log2CacheLineSize              uint64
	log2PageSize                   uint64
	log2MemoryBankInterleavingSize uint64
	memAddrOffset                  uint64
	dramSize                       uint64
	globalStorage                  *mem.Storage
	mmu                            *mmu.Comp
	gmmu                           *gmmu.Comp   // sbin_codex: GPU-side translation endpoint.
	pageTable                      vm.PageTable // sbin_codex: this GPU's private page table.
	rdmaAddressMapper              mem.AddressToPortMapper
	dataPathTopology               DataPathTopology // sbin_codex
	memoryTopology                 MemoryTopology   // sbin_codex

	gpu                  *sim.Domain
	cp                   *cp.CommandProcessor
	rdmaEngine           *rdma.Comp
	pmc                  *pagemigrationcontroller.PageMigrationController
	dmaEngine            *cp.DMAEngine
	sas                  []*sim.Domain
	l2Caches             []*writeback.Comp
	l2AddressTranslators []*addresstranslator.Comp // sbin_codex: one translator per L2/DRAM slice.
	l2TLBs               []*tlb.Comp
	drams                []sim.Component
	internalConn         *directconnection.Comp
	l2ToDramConnection   *directconnection.Comp
	l1AddressMapper      *mem.InterleavedAddressPortMapper
	l1DataAddressMapper  *mem.InterleavedAddressPortMapper // sbin_codex: selected by the injected data-path topology.
	l1TLBAddressMapper   *mem.SinglePortMapper
	l1tlbFactory         func(name string, engine sim.Engine, freq sim.Freq, pageTable vm.PageTable, mapper mem.AddressToPortMapper, numReqPerCycle int) sim.Component //nolint:lll // sbin_codex: ideal-L1-TLB factory injection (todo 5).
	pmcAddressMapper     mem.AddressToPortMapper
	// sbin_codex: UVM generation source for the virtual access gates (plan
	// todo 10). A nil provider keeps the gates at generation zero.
	generationProvider shaderarray.GenerationProvider
}

// MakeBuilder creates a new builder.
func MakeBuilder() Builder {
	return Builder{
		freq:                           1 * sim.GHz,
		numCUPerShaderArray:            4,
		numShaderArray:                 16,
		l2CacheSize:                    2 * mem.MB,
		numMemoryBank:                  16,
		log2CacheLineSize:              6,
		log2PageSize:                   12,
		log2MemoryBankInterleavingSize: 7,
		memAddrOffset:                  0,
		dramSize:                       4 * mem.GB,
		dataPathTopology:               NewBaselineDataPathTopology(), // sbin_codex: baseline is the nil-free default.
		memoryTopology:                 NewBaselineMemoryTopology(),   // sbin_codex: baseline is the nil-free default.
	}
}

// WithSimulation sets the simulation to use.
func (b Builder) WithSimulation(sim *simulation.Simulation) Builder {
	b.simulation = sim
	return b
}

// WithGPUID sets the GPU ID to use.
func (b Builder) WithGPUID(id uint64) gpubuilder.GPUBuilder {
	b.gpuID = id
	return b
}

// WithFreq sets the frequency that the GPU works at.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithLog2MemoryBankInterleavingSize sets the log2 memory bank interleaving
// size.
func (b Builder) WithLog2MemoryBankInterleavingSize(size uint64) Builder {
	b.log2MemoryBankInterleavingSize = size
	return b
}

// WithLog2CacheLineSize sets the log2 cache line size.
func (b Builder) WithLog2CacheLineSize(size uint64) Builder {
	b.log2CacheLineSize = size
	return b
}

// WithLog2PageSize sets the log2 page size.
func (b Builder) WithLog2PageSize(size uint64) Builder {
	b.log2PageSize = size
	return b
}

// WithMemAddrOffset sets the memory address offset.
func (b Builder) WithMemAddrOffset(offset uint64) gpubuilder.GPUBuilder {
	b.memAddrOffset = offset
	return b
}

// WithNumCUPerShaderArray sets the number of CUs per shader array.
func (b Builder) WithNumCUPerShaderArray(numCUPerShaderArray int) Builder {
	b.numCUPerShaderArray = numCUPerShaderArray
	return b
}

// WithNumShaderArray sets the number of shader arrays.
func (b Builder) WithNumShaderArray(numShaderArray int) Builder {
	b.numShaderArray = numShaderArray
	return b
}

// WithL2CacheSize sets the size of the L2 cache.
func (b Builder) WithL2CacheSize(size uint64) Builder {
	b.l2CacheSize = size
	return b
}

// WithNumMemoryBank sets the number of memory banks.
func (b Builder) WithNumMemoryBank(numMemoryBank int) Builder {
	b.numMemoryBank = numMemoryBank
	return b
}

// WithDramSize sets the size of the DRAM.
func (b Builder) WithDramSize(size uint64) Builder {
	b.dramSize = size
	return b
}

// WithMMU sets the MMU that can provide the ultimate address translation.
func (b Builder) WithMMU(mmu *mmu.Comp) Builder {
	b.mmu = mmu
	return b
}

// WithPageTable binds the GPU builder to its per-GPU page table. // sbin_codex
func (b Builder) WithPageTable(pageTable vm.PageTable) gpubuilder.GPUBuilder {
	b.pageTable = pageTable
	return b
}

// WithL1TLBFactory sets the factory that builds L1 TLBs for this GPU.
// When nil, the default tlb.MakeBuilder is used. // sbin_codex
func (b Builder) WithL1TLBFactory(
	f func(
		name string,
		engine sim.Engine,
		freq sim.Freq,
		pageTable vm.PageTable,
		mapper mem.AddressToPortMapper,
		numReqPerCycle int,
	) sim.Component,
) Builder {
	b.l1tlbFactory = f
	return b
}

// WithGenerationProvider sets the UVM generation source the virtual access
// gates stamp into annotations and compare for stale retries. A nil provider
// keeps the gates at generation zero. // sbin_codex
func (b Builder) WithGenerationProvider(
	provider shaderarray.GenerationProvider,
) Builder {
	b.generationProvider = provider
	return b
}

// WithGlobalStorage sets the global storage that can provide the ultimate address translation.
func (b Builder) WithGlobalStorage(
	globalStorage *mem.Storage,
) Builder {
	b.globalStorage = globalStorage
	return b
}

// WithRDMAAddressMapper sets the RDMA address mapper.
func (b Builder) WithRDMAAddressMapper(
	mapper mem.AddressToPortMapper,
) gpubuilder.GPUBuilder {
	b.rdmaAddressMapper = mapper
	return b
}

// WithDataPathTopology sets L1 mapper and control wiring behavior. // sbin_codex
func (b Builder) WithDataPathTopology(topology DataPathTopology) Builder {
	b.dataPathTopology = topology // sbin_codex
	return b
}

// WithMemoryTopology sets L2 physical-boundary behavior. // sbin_codex
func (b Builder) WithMemoryTopology(topology MemoryTopology) Builder {
	b.memoryTopology = topology // sbin_codex
	return b
}

// Build builds the hardware platform.
func (b Builder) Build(name string) *sim.Domain {
	b.validateTopologyPair() // sbin_codex: reject invalid composition before domain or mapper construction.
	b.name = name
	b.gpu = sim.NewDomain(name)

	b.l1AddressMapper = mem.NewInterleavedAddressPortMapper(
		1 << b.log2MemoryBankInterleavingSize,
	)
	b.l1AddressMapper.LowAddress = b.memAddrOffset
	b.l1AddressMapper.HighAddress = b.memAddrOffset + b.dramSize
	b.l1AddressMapper.UseAddressSpaceLimitation = true
	b.dataPathTopology.initializeMapper(&b) // sbin_codex

	b.l1TLBAddressMapper = &mem.SinglePortMapper{}

	b.buildSAs()
	b.buildDRAMControllers()
	b.buildL2Caches()
	b.buildCP()
	b.buildGMMU() // sbin_codex: the L2 TLB must target the GPU-side walker.
	b.buildL2TLB()
	// b.buildL2AddressTranslators()
	b.memoryTopology.buildBoundary(&b) // sbin_codex: boundary construction follows the shared L2 TLB.

	b.connectCP()
	b.connectL2AndDRAM()
	b.connectL1ToL2()
	b.connectL1TLBToL2TLB()
	b.connectL2TLBToGMMU() // sbin_codex: complete the GPU translation path.

	b.populateExternalPorts()

	return b.gpu
}

func (b *Builder) populateExternalPorts() {
	b.gpu.AddPort("CommandProcessor", b.cp.ToDriver)
	b.gpu.AddPort("RDMARequest", b.rdmaEngine.RDMARequestOutside)
	b.gpu.AddPort("RDMAData", b.rdmaEngine.RDMADataOutside)

	b.gpu.AddPort("PageMigrationController",
		b.pmc.GetPortByName("Remote"))

	// The GMMU bottom port replaces the L2 TLB bottom port as the GPU's
	// external translation endpoint. // sbin_codex
	b.gpu.AddPort("Translation", b.gmmu.GetPortByName("Bottom"))
}

func (b *Builder) connectCP() {
	b.internalConn = directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".InternalConn")
	b.simulation.RegisterComponent(b.internalConn)

	b.internalConn.PlugIn(b.cp.ToDMA)
	b.internalConn.PlugIn(b.cp.ToCaches)
	b.internalConn.PlugIn(b.cp.ToCUs)
	b.internalConn.PlugIn(b.cp.ToTLBs)
	b.internalConn.PlugIn(b.cp.ToAddressTranslators)
	b.internalConn.PlugIn(b.cp.ToRDMA)
	b.internalConn.PlugIn(b.cp.ToPMC)
	b.internalConn.PlugIn(b.cp.ToGMMU)                              // sbin_codex: UVM GMMU control seam.
	b.internalConn.PlugIn(b.gmmu.GetPortByName("Control"))          // sbin_codex
	b.internalConn.PlugIn(b.gmmu.GetPortByName("CommandProcessor")) // sbin_codex
	// sbin_codex (todo 24 of mgpusim-uvm-manager): the CP's ToAccessCounter
	// port is the shared AccessCounter seam. The timingconfig builder sets the
	// counter's ToCP field to this port; loop-back messages flow through the
	// internal connection.
	b.internalConn.PlugIn(b.cp.ToAccessCounter)

	b.cp.RDMA = b.rdmaEngine.CtrlPort
	b.internalConn.PlugIn(b.cp.RDMA)

	b.cp.DMAEngine = b.dmaEngine.ToCP
	b.internalConn.PlugIn(b.dmaEngine.ToCP)

	pmcControlPort := b.pmc.GetPortByName("Control")
	b.cp.PMC = pmcControlPort
	b.internalConn.PlugIn(pmcControlPort)

	// sbin_codex: the CP pre-registers the GMMU translation gate for UVM
	// block/unblock commands and the GMMU targets the CP for typed faults and
	// replay commands (todo 8 of mgpusim-uvm-manager).
	b.gmmu.SetCommandProcessor(b.cp.ToGMMU)
	b.cp.UVMGateIDs = append(b.cp.UVMGateIDs, gmmu.TranslationGateID)
	// sbin_codex (todo 24): the CP routes block/unblock commands to the GMMU
	// control port through the shared ToGMMU seam.
	b.cp.GMMUControl = b.gmmu.GetPortByName("Control").AsRemote()

	b.connectCPWithCUs()
	// b.connectCPWithAddressTranslators()
	// b.connectCPWithTLBs()
	b.dataPathTopology.connectCP(b) // sbin_codex
	b.memoryTopology.connectCP(b)   // sbin_codex
	b.connectCPWithCaches()
}

func (b *Builder) connectL1ToL2() {
	b.dataPathTopology.connectL1ToL2(b) // sbin_codex
}

func (b *Builder) connectL2AndDRAM() {
	b.memoryTopology.connectL2AndDRAM(b) // sbin_codex
}

func (b *Builder) connectL1TLBToL2TLB() {
	b.dataPathTopology.connectTranslation(b) // sbin_codex
}

type cuInterfaceForCP struct {
	ctrlPort        sim.RemotePort
	dispatchingPort sim.RemotePort
	wfPoolSizes     []int
	vRegCounts      []int
	sRegCount       int
	ldsBytes        int
}

func (cu cuInterfaceForCP) ControlPort() sim.RemotePort {
	return cu.ctrlPort
}

func (cu cuInterfaceForCP) DispatchingPort() sim.RemotePort {
	return cu.dispatchingPort
}

func (cu cuInterfaceForCP) WfPoolSizes() []int {
	return cu.wfPoolSizes
}

func (cu cuInterfaceForCP) VRegCounts() []int {
	return cu.vRegCounts
}

func (cu cuInterfaceForCP) SRegCount() int {
	return cu.sRegCount
}

func (cu cuInterfaceForCP) LDSBytes() int {
	return cu.ldsBytes
}

func (b *Builder) connectCPWithCUs() {
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			cuDispatchingPort := sa.GetPortByName(
				fmt.Sprintf("CU[%d]", i))
			cuCtrlPort := sa.GetPortByName(
				fmt.Sprintf("CUCtrl[%d]", i))
			cu := cuInterfaceForCP{
				ctrlPort:        cuCtrlPort.AsRemote(),
				dispatchingPort: cuDispatchingPort.AsRemote(),
				wfPoolSizes:     []int{10, 10, 10, 10},
				vRegCounts:      []int{16384, 16384, 16384, 16384},
				sRegCount:       3200,
				ldsBytes:        64 * 1024,
			}

			b.cp.RegisterCU(cu)

			b.internalConn.PlugIn(cuDispatchingPort)
			b.internalConn.PlugIn(cuCtrlPort)
		}
	}
}

func (b *Builder) addSharedL2TLBs() { // sbin_codex
	for _, tlb := range b.l2TLBs {
		b.addTLB(tlb.GetPortByName("Control"))
	}
}

func (b *Builder) addTLB(port sim.Port) { // sbin_codex
	b.cp.TLBs = append(b.cp.TLBs, port)
	b.internalConn.PlugIn(port)
}

// sbin_codex: registerGMMUTLBEndpoints registers the topology-present TLB
// endpoint set with the GMMU invalidation coordinator (plan todo 14 of
// mgpusim-uvm-manager, uvm-manager.md §21.1). The set is exactly the CP's
// TLB endpoints: baseline = private L1V/L1S/L1I + shared L2; virtual-caching
// = private L1I + shared L2 only.
func (b *Builder) registerGMMUTLBEndpoints() { // sbin_codex
	b.gmmu.SetTLBEndpoints(b.cp.TLBs)
}

func (b *Builder) addPreCacheTranslator(port sim.Port) { // sbin_codex
	b.cp.PreCacheTranslators.Ports = append(b.cp.PreCacheTranslators.Ports, port)
	b.internalConn.PlugIn(port)
}

func (b *Builder) addPostCacheTranslator(port sim.Port) { // sbin_codex
	b.cp.PostCacheTranslators.Ports = append(b.cp.PostCacheTranslators.Ports, port)
	b.internalConn.PlugIn(port)
}

func (b *Builder) connectCPWithCaches() {
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			cache := sa.GetPortByName(fmt.Sprintf("L1VCacheCtrl[%d]", i))
			b.cp.L1VCaches = append(b.cp.L1VCaches, cache)
			b.internalConn.PlugIn(cache)
		}

		l1sCache := sa.GetPortByName("L1SCacheCtrl")
		b.cp.L1SCaches = append(b.cp.L1SCaches, l1sCache)
		b.internalConn.PlugIn(l1sCache)

		l1iCache := sa.GetPortByName("L1ICacheCtrl")
		b.cp.L1ICaches = append(b.cp.L1ICaches, l1iCache)
		b.internalConn.PlugIn(l1iCache)
	}

	for _, c := range b.l2Caches {
		ctrlPort := c.GetPortByName("Control")
		b.cp.L2Caches = append(b.cp.L2Caches, ctrlPort)
		b.internalConn.PlugIn(ctrlPort)
	}
}

func (b *Builder) buildSAs() {
	// Original chain (commented per AGENTS.md convention):
	// saBuilder := shaderarray.MakeBuilder().
	// 	WithSimulation(b.simulation).
	// 	WithFreq(b.freq).
	// 	WithGPUID(b.gpuID).
	// 	WithNumCUs(b.numCUPerShaderArray).
	// 	WithLog2CacheLineSize(b.log2CacheLineSize).
	// 	WithLog2PageSize(b.log2PageSize).
	// 	WithL1AddressMapper(b.l1AddressMapper).
	// 	WithL1TLBAddressMapper(b.l1TLBAddressMapper)

	saBuilder := shaderarray.MakeBuilder(). // sbin_codex
						WithSimulation(b.simulation).
						WithFreq(b.freq).
						WithGPUID(b.gpuID).
						WithNumCUs(b.numCUPerShaderArray).
						WithLog2CacheLineSize(b.log2CacheLineSize).
						WithLog2PageSize(b.log2PageSize).
						WithL1TLBAddressMapper(b.l1TLBAddressMapper).
						WithPageTable(b.pageTable).      // sbin_codex: pass page table for ideal-L1-TLB factory (todo 5).
						WithL1TLBFactory(b.l1tlbFactory) // sbin_codex: pass ideal-L1-TLB factory hook (todo 5).
	saBuilder = b.dataPathTopology.configureShaderArray(b, saBuilder) // sbin_codex

	// if b.enableISADebugging {
	// 	saBuilder = saBuilder.withIsaDebugging()
	// }

	// if b.enableMemTracing {
	// 	saBuilder = saBuilder.withMemTracer(b.memTracer)
	// }

	for i := 0; i < b.numShaderArray; i++ {
		saName := fmt.Sprintf("%s.SA[%d]", b.name, i)
		sa := saBuilder.Build(saName)

		b.sas = append(b.sas, sa)
	}
}

func (b *Builder) buildL2Caches() {
	byteSize := b.l2CacheSize / uint64(b.numMemoryBank)
	l2Builder := writeback.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithByteSize(byteSize).
		WithNumMSHREntry(64).
		WithNumReqPerCycle(16).
		WithUVMRangeVirtual(b.memoryTopology.uvmRangeVirtual()) // sbin_codex: UVM range-operation mode (plan todo 13).

	for i := 0; i < b.numMemoryBank; i++ {
		cacheName := fmt.Sprintf("%s.L2Cache[%d]", b.name, i)
		l2 := l2Builder.WithInterleaving(
			1<<(b.log2MemoryBankInterleavingSize-b.log2CacheLineSize),
			b.numMemoryBank,
			i).
			WithAddressMapperType("single").
			WithRemotePorts(b.drams[i].GetPortByName("Top").AsRemote()).
			Build(cacheName)

		b.simulation.RegisterComponent(l2)
		b.l2Caches = append(b.l2Caches, l2)

		b.l1AddressMapper.LowModules = append(
			b.l1AddressMapper.LowModules,
			l2.GetPortByName("Top").AsRemote(),
		)

		// if b.enableMemTracing {
		// 	tracing.CollectTrace(l2, b.memTracer)
		// }
	}
}

func (b *Builder) buildDRAMControllers() {
	// memCtrlBuilder := b.createDramControllerBuilder()

	for i := 0; i < b.numMemoryBank; i++ {
		dramName := fmt.Sprintf("%s.DRAM[%d]", b.name, i)
		dram := idealmemcontroller.MakeBuilder().
			WithEngine(b.simulation.GetEngine()).
			WithFreq(b.freq).
			WithLatency(100).
			WithStorage(b.globalStorage).
			Build(dramName)
		b.simulation.RegisterComponent(dram)
		b.drams = append(b.drams, dram)

		// if b.enableMemTracing {
		// 	tracing.CollectTrace(dram, b.memTracer)
		// }
	}
}

func (b *Builder) createDramControllerBuilder() dram.Builder {
	memBankSize := 4 * mem.GB / uint64(b.numMemoryBank)
	if 4*mem.GB%uint64(b.numMemoryBank) != 0 {
		panic("GPU memory size is not a multiple of the number of memory banks")
	}

	dramCol := 64
	dramRow := 16384
	dramDeviceWidth := 128
	dramBankSize := dramCol * dramRow * dramDeviceWidth
	dramBank := 4
	dramBankGroup := 4
	dramBusWidth := 256
	dramDevicePerRank := dramBusWidth / dramDeviceWidth
	dramRankSize := dramBankSize * dramDevicePerRank * dramBank
	dramRank := int(memBankSize * 8 / uint64(dramRankSize))

	memCtrlBuilder := dram.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(500 * sim.MHz).
		WithProtocol(dram.HBM).
		WithBurstLength(4).
		WithDeviceWidth(dramDeviceWidth).
		WithBusWidth(dramBusWidth).
		WithNumChannel(1).
		WithNumRank(dramRank).
		WithNumBankGroup(dramBankGroup).
		WithNumBank(dramBank).
		WithNumCol(dramCol).
		WithNumRow(dramRow).
		WithCommandQueueSize(8).
		WithTransactionQueueSize(32).
		WithTCL(7).
		WithTCWL(2).
		WithTRCDRD(7).
		WithTRCDWR(7).
		WithTRP(7).
		WithTRAS(17).
		WithTREFI(1950).
		WithTRRDS(2).
		WithTRRDL(3).
		WithTWTRS(3).
		WithTWTRL(4).
		WithTWR(8).
		WithTCCDS(1).
		WithTCCDL(1).
		WithTRTRS(0).
		WithTRTP(3).
		WithTPPD(2)

	if b.globalStorage != nil {
		memCtrlBuilder = memCtrlBuilder.WithGlobalStorage(b.globalStorage)
	}

	return memCtrlBuilder
}

func (b *Builder) buildRDMAEngine() {
	name := fmt.Sprintf("%s.RDMA", b.name)
	b.rdmaEngine = rdma.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(1 * sim.GHz).
		WithLocalModules(b.l1AddressMapper).
		Build(name)

	b.rdmaEngine.RemoteRDMAAddressTable = b.rdmaAddressMapper

	b.simulation.RegisterComponent(b.rdmaEngine)
}

func (b *Builder) buildPageMigrationController() {
	b.pmc = pagemigrationcontroller.NewPageMigrationController(
		fmt.Sprintf("%s.PMC", b.name),
		b.simulation.GetEngine(),
		b.pmcAddressMapper,
		nil)

	b.simulation.RegisterComponent(b.pmc)
}

func (b *Builder) buildDMAEngine() {
	b.dmaEngine = cp.NewDMAEngine(
		fmt.Sprintf("%s.DMA", b.name),
		b.simulation.GetEngine(),
		nil)

	b.simulation.RegisterComponent(b.dmaEngine)
}

func (b *Builder) buildCP() {
	b.cp = cp.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithVisTracer(b.simulation.GetVisTracer()).
		WithFreq(b.freq).
		WithMonitor(b.simulation.GetMonitor()).
		Build(b.name + ".CommandProcessor")

	b.simulation.RegisterComponent(b.cp)

	b.buildDMAEngine()
	b.buildRDMAEngine()
	b.buildPageMigrationController()
}

// buildGMMU creates the GPU-side page-table walker. It resolves L2 TLB misses
// against this GPU's page table and keeps the CPU MMU as the downstream
// external translation endpoint for the v4 platform topology. // sbin_codex
func (b *Builder) buildGMMU() {
	b.gmmu = gmmu.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithLog2PageSize(b.log2PageSize).
		WithPageWalkingLatency(100).
		WithPageTable(b.pageTable).
		WithDeviceID(b.gpuID).
		WithMemAddrOffset(b.memAddrOffset).
		WithMemoryPerChiplet(b.dramSize).
		Build(b.name + ".GMMU")

	b.simulation.RegisterComponent(b.gmmu)
}

// connectL2TLBToGMMU connects every L2 TLB bottom port to the GMMU top port.
// Translation requests therefore walk the GPU page table before returning to
// the TLB hierarchy. // sbin_codex
func (b *Builder) connectL2TLBToGMMU() {
	conn := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".L2TLBToGMMU")
	b.simulation.RegisterComponent(conn)

	conn.PlugIn(b.gmmu.GetPortByName("Top"))
	for _, l2TLB := range b.l2TLBs {
		conn.PlugIn(l2TLB.GetPortByName("Bottom"))
	}
}

func (b *Builder) buildL2TLB() {
	numEntries := 512
	numWays := 64
	// numSets := int(numEntries/numWays)
	numSets := numEntries / numWays // sbin_codex: restore the baseline expression shape; both operands are int.

	builder := tlb.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithNumWays(numWays).
		WithNumSets(numSets).
		WithNumMSHREntry(64).
		WithNumReqPerCycle(1024).
		WithLog2PageSize(b.log2PageSize).
		// WithLowModule(b.gmmu.GetPortByName("Top").AsRemote()). // sbin_codex: route L2 misses through GMMU.
		WithTranslationProviderMapper(&mem.SinglePortMapper{
			Port: b.gmmu.GetPortByName("Top").AsRemote(), // sbin_codex
		})

	l2TLB := builder.Build(fmt.Sprintf("%s.L2TLB", b.name))

	b.simulation.RegisterComponent(l2TLB)
	b.l2TLBs = append(b.l2TLBs, l2TLB)

	b.l1TLBAddressMapper.Port = l2TLB.GetPortByName("Top").AsRemote()
}

func (b *Builder) numCU() int {
	return b.numCUPerShaderArray * b.numShaderArray
}
