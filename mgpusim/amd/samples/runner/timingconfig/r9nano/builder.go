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
	"github.com/sarchlab/mgpusim/v4/amd/timing/accesscounter" // sbin_codex
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/asu"    // sbin_claude_avatar: speculation unit component.
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
	"github.com/sarchlab/mgpusim/v4/amd/timing/pagemigrationcontroller"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rdma"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/rsw"         // sbin_claude_utopia: RestSeg walker component.
	"github.com/sarchlab/mgpusim/v4/amd/timing/virtualcaching/fbt" // sbin_claude_fbt: Forward-Backward Table component.
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
	dataPathTopology               DataPathTopology    // sbin_codex
	memoryTopology                 MemoryTopology      // sbin_codex
	translationTopology            TranslationTopology // sbin_claude_utopia: walker chain below the L2 TLB.
	speculationTopology            SpeculationTopology // sbin_claude_avatar: interposer above the L2 TLB.
	hptSettings                    HPTSettings         // sbin_claude_hpt: hashed-page-table walk mode.
	softWalkerSettings             SoftWalkerSettings  // sbin_claude_softwalker: software page-walk mode.
	gmmuMaxInflight                int                 // sbin_claude_softwalker: baseline PTW sweep (0 = default).
	l2TLBNumMSHR                   int                 // sbin_claude_softwalker: L2 TLB MSHR sweep (0 = default).
	latpcSettings                  LATPCSettings       // sbin_claude_latpc: RD + LATC + LATP.
	l1TLBMSHREntries               int                 // sbin_claude_latpc: 0 keeps the 64-entry default.

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
	// sbin_codex: late-bound to RDMA request ingress.
	remoteMemoryProvider *mem.SinglePortMapper
	l1tlbFactory         func(name string, engine sim.Engine, freq sim.Freq, pageTable vm.PageTable, mapper mem.AddressToPortMapper, numReqPerCycle int) sim.Component //nolint:lll // sbin_codex: ideal-L1-TLB factory injection (todo 5).
	pmcAddressMapper     mem.AddressToPortMapper
	driverPort           sim.Port            // sbin_codex: driver destination for CP control responses.
	uvmServiceProvider   sim.RemotePort      // sbin_codex: driver UVM port.
	tlbCtrlPorts         []sim.RemotePort    // sbin_codex: UVM range-invalidation targets.
	accessCounter        *accesscounter.Comp // sbin_codex: GPU-side UVM counter.
	accessCounterThresh  uint64              // sbin_codex
	maxOutstandingRemote int                 // sbin_claude
	utu                  *rsw.Comp           // sbin_claude_utopia: RestSeg walker (nil on baseline).
	fbt                  *fbt.Comp           // sbin_claude_fbt: Forward-Backward Table (nil unless the FBT topology is used).
	asu                  *asu.Comp           // sbin_claude_avatar: speculation unit (nil on baseline).
}

// MakeBuilder creates a new builder.
func MakeBuilder() Builder {
	return Builder{
		freq:                1 * sim.GHz,
		numCUPerShaderArray: 4,
		numShaderArray:      16,
		// Pre-edit code (commented per project convention):
		// l2CacheSize: 2 * mem.MB,
		//
		// sbin_claude_fbt: the shared L2 is the second stage of the virtual
		// cache hierarchy's translation filter; a miss here is what forces a
		// translation.
		l2CacheSize:                    4 * mem.MB,
		numMemoryBank:                  16,
		log2CacheLineSize:              6,
		log2PageSize:                   12,
		log2MemoryBankInterleavingSize: 7,
		memAddrOffset:                  0,
		dramSize:                       4 * mem.GB,
		dataPathTopology:               NewBaselineDataPathTopology(),    // sbin_codex: baseline is the nil-free default.
		memoryTopology:                 NewBaselineMemoryTopology(),      // sbin_codex: baseline is the nil-free default.
		translationTopology:            NewBaselineTranslationTopology(), // sbin_claude_utopia: nil-free default.
		speculationTopology:            NewBaselineSpeculationTopology(), // sbin_claude_avatar: nil-free default.
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

// WithDriverPort sets the destination for command-processor control responses. // sbin_codex
func (b Builder) WithDriverPort(port sim.Port) gpubuilder.GPUBuilder {
	b.driverPort = port // sbin_codex
	return b
}

// WithUVMServiceProvider sets the driver UVM fault service provider for the
// GMMU. When empty, UVM demand-fault gating is disabled. // sbin_codex
func (b Builder) WithUVMServiceProvider(provider sim.RemotePort) gpubuilder.GPUBuilder {
	b.uvmServiceProvider = provider
	return b
}

// WithAccessCounterThreshold sets the GPU-side UVM remote-access counter
// threshold. // sbin_codex
func (b Builder) WithAccessCounterThreshold(
	thresh uint64,
) gpubuilder.GPUBuilder {
	b.accessCounterThresh = thresh
	return b
}

// WithMaxOutstandingRemote caps how many UVM remote accesses this GPU may
// have in flight over PCIe. Zero means unlimited. // sbin_claude
func (b Builder) WithMaxOutstandingRemote(n int) gpubuilder.GPUBuilder {
	b.maxOutstandingRemote = n
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

// WithTranslationTopology sets the walker chain below the shared L2 TLB
// (baseline GMMU-only, or Utopia UTU+GMMU). // sbin_claude_utopia
func (b Builder) WithTranslationTopology(topology TranslationTopology) Builder {
	b.translationTopology = topology
	return b
}

// WithSpeculationTopology sets what serves the L1 TLB bottom ports
// (baseline direct L2 TLB, or the Avatar ASU). // sbin_claude_avatar
func (b Builder) WithSpeculationTopology(topology SpeculationTopology) Builder {
	b.speculationTopology = topology
	return b
}

// HPTSettings selects the FS-HPT (PACT'24) walk mode in the GMMU. Unlike
// Utopia and Avatar, HPT needs no extra component and no rewiring: a hashed
// page table changes only how many memory references one walk costs and
// removes the intermediate levels a page-walk cache would hold, so it is a
// mode of the existing GPU-side walker. // sbin_claude_hpt
type HPTSettings struct {
	// Enabled turns the hashed walk on. When false the GMMU walks the radix
	// page table exactly as before.
	Enabled bool
	// AccessesPerWalk is how many memory references one walk costs. Ideal
	// HPT (no hash collision) is 1.
	AccessesPerWalk int
}

// WithHPTSettings selects the hashed-page-table walk mode for this GPU's
// GMMU. // sbin_claude_hpt
func (b Builder) WithHPTSettings(settings HPTSettings) Builder {
	b.hptSettings = settings
	return b
}

// SoftWalkerSettings selects the SoftWalker (MICRO'25) software page-walk
// mode. Like HPT it needs no extra component and no rewiring: the GMMU's
// admission becomes the paper's Request Distributor over per-CU PW-warp
// slots, each walk pays communication and instruction latency on top of the
// unchanged radix+PWC traversal, and the shared L2 TLB gains the In-TLB
// MSHR so the added walk concurrency is actually reachable.
// sbin_claude_softwalker
type SoftWalkerSettings struct {
	// Enabled turns the software walk on. When false everything stays the
	// baseline.
	Enabled bool
	// SlotsPerCU is the SoftPWB depth per compute unit (32 in the paper).
	SlotsPerCU int
	// CommCycles is the one-way L2TLB<->CU communication latency, charged
	// twice per walk.
	CommCycles int
	// SetupCycles is the PW Warp's per-walk setup cost.
	SetupCycles int
	// PerLevelCycles is the non-memory instruction cost per traversed
	// page-table level.
	PerLevelCycles int
	// InTLBMSHRMax caps how many L2 TLB ways may serve as In-TLB MSHR
	// slots. 0 disables In-TLB MSHR (the paper's "SW w/o In-TLB MSHR"
	// ablation).
	InTLBMSHRMax int
}

// WithSoftWalkerSettings selects the software page-walk mode for this GPU.
// sbin_claude_softwalker
func (b Builder) WithSoftWalkerSettings(settings SoftWalkerSettings) Builder {
	b.softWalkerSettings = settings
	return b
}

// WithGMMUMaxInflight overrides how many page walks the baseline GMMU keeps
// in flight (the hardware PTW count analog). 0 keeps the GMMU default.
// sbin_claude_softwalker
func (b Builder) WithGMMUMaxInflight(n int) Builder {
	b.gmmuMaxInflight = n
	return b
}

// WithL2TLBNumMSHR overrides the shared L2 TLB's dedicated MSHR entry
// count. 0 keeps the default. sbin_claude_softwalker
func (b Builder) WithL2TLBNumMSHR(n int) Builder {
	b.l2TLBNumMSHR = n
	return b
}

// LATPCSettings selects the LATPC translation path (MICRO'25,
// refs/latpc-plan.md): the Regularity Detector on every CU's coalescer, the
// LATC compressed MSHR on every L1V TLB, and LATP batched walks in the GMMU.
// Like HPT it needs no extra component and no rewiring - the translation
// topology stays the baseline. Non-UVM only. // sbin_claude_latpc
type LATPCSettings struct {
	// Enabled turns all three LATPC mechanisms on.
	Enabled bool
	// L4RowHitLatency is the cycles one batched member's L4 PTE load costs
	// (a DRAM row-buffer hit). The GMMU default is 20.
	L4RowHitLatency int
}

// WithLATPCSettings selects the LATPC translation path for this GPU.
// sbin_claude_latpc
func (b Builder) WithLATPCSettings(settings LATPCSettings) Builder {
	b.latpcSettings = settings
	return b
}

// WithL1TLBMSHREntries overrides the per-CU L1V TLB MSHR entry count
// (default 64) for any configuration, so the paper's 8-entry contention
// regime is measurable on the baseline and LATPC symmetrically.
// sbin_claude_latpc
func (b Builder) WithL1TLBMSHREntries(n int) Builder {
	b.l1TLBMSHREntries = n
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

	b.remoteMemoryProvider = &mem.SinglePortMapper{} // sbin_codex: SAs are built before RDMA.
	b.l1TLBAddressMapper = &mem.SinglePortMapper{}

	b.buildSAs()
	b.buildDRAMControllers()
	b.buildL2Caches()
	b.buildCP()
	b.buildGMMU() // sbin_codex: the L2 TLB must target the GPU-side walker.
	// sbin_claude_utopia: extra walkers (the Utopia UTU) are built between the
	// GMMU and the L2 TLB so the L2 TLB can target them as its provider.
	b.translationTopology.buildWalkers(&b)
	b.buildL2TLB()
	// sbin_claude_avatar: the ASU is built after the L2 TLB so it can
	// retarget the l1TLBAddressMapper and forward to the L2 TLB top.
	b.speculationTopology.buildSpeculationUnit(&b)
	// b.buildL2AddressTranslators()
	b.memoryTopology.buildBoundary(&b) // sbin_codex: boundary construction follows the shared L2 TLB.

	b.connectCP()
	b.connectL2AndDRAM()
	b.connectL1ToL2()
	b.connectL1TLBToL2TLB()
	// Pre-edit code (commented per project convention):
	// b.connectL2TLBToGMMU() // sbin_codex: complete the GPU translation path.
	//
	// sbin_claude_utopia: the translation topology owns the L2TLB-to-walker
	// wiring; the baseline delegates back to connectL2TLBToGMMU.
	b.translationTopology.connectWalkers(&b)
	// sbin_claude_avatar: wire the ASU bottom to the L2 TLB top (no-op on
	// the baseline).
	b.speculationTopology.connectSpeculation(&b)

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

	// Pre-edit code (commented per AGENTS.md convention). The GMMU used to be
	// the driver's direct peer:
	// if b.uvmServiceProvider != "" {
	// 	b.gpu.AddPort("UVM", b.gmmu.GetPortByName("UVM"))
	// }
	//
	// sbin_codex: the Command Processor is now the GPU-side UVM control
	// endpoint (spec 2.1); the GMMU only faces it internally.
	if b.uvmServiceProvider != "" {
		b.gpu.AddPort("UVM", b.cp.ToUVMDriver)
	}
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

	b.cp.RDMA = b.rdmaEngine.CtrlPort
	b.internalConn.PlugIn(b.cp.RDMA)

	b.cp.DMAEngine = b.dmaEngine.ToCP
	b.internalConn.PlugIn(b.dmaEngine.ToCP)

	pmcControlPort := b.pmc.GetPortByName("Control")
	b.cp.PMC = pmcControlPort
	b.internalConn.PlugIn(pmcControlPort)

	b.connectCPWithCUs()
	// b.connectCPWithAddressTranslators()
	// b.connectCPWithTLBs()
	b.dataPathTopology.connectCP(b) // sbin_codex
	b.memoryTopology.connectCP(b)   // sbin_codex
	b.connectCPWithCaches()
	b.connectUVMControlPlane() // sbin_codex: every TLB control port is known now.
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
	// sbin_codex: the GMMU coordinates UVM range invalidation, so it needs the
	// same control ports (spec 21.1).
	b.tlbCtrlPorts = append(b.tlbCtrlPorts, port.AsRemote())
	b.internalConn.PlugIn(port)
}

// registerUVMTranslators gives the Command Processor every address translator
// that must drain a UVM region before its cache lines are written back. They
// are kept out of the legacy shootdown groups so the non-UVM migration path is
// unchanged. // sbin_codex
func (b *Builder) registerUVMTranslators() {
	groups := [][]sim.Port{
		b.cp.PreCacheTranslators.Ports,
		b.cp.PostCacheTranslators.Ports,
	}

	for _, group := range groups {
		for _, port := range group {
			b.cp.UVMTranslators = append(
				b.cp.UVMTranslators, port.AsRemote())
		}
	}
}

// connectUVMControlPlane joins the Command Processor and the GMMU on the GPU's
// internal control network and tells each side about the other. // sbin_codex
func (b *Builder) connectUVMControlPlane() {
	if b.uvmServiceProvider == "" {
		return
	}

	gmmuUVMPort := b.gmmu.GetPortByName("UVM")
	gmmuTLBCtrlPort := b.gmmu.GetPortByName("TLBCtrl")

	b.internalConn.PlugIn(b.cp.ToUVMInternal)
	b.internalConn.PlugIn(gmmuUVMPort)
	b.internalConn.PlugIn(gmmuTLBCtrlPort)

	b.cp.GMMU = gmmuUVMPort.AsRemote()
	b.cp.UVMDriverPort = b.uvmServiceProvider

	b.gmmu.SetTLBs(b.tlbCtrlPorts)

	b.registerUVMTranslators()

	if b.accessCounter != nil {
		b.internalConn.PlugIn(b.accessCounter.Ctrl)
		b.accessCounter.SetCtrlDestination(b.cp.ToUVMInternal.AsRemote())
		b.cp.AccessCounter = b.accessCounter.Ctrl.AsRemote()
	}
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
		// WithRemoteMemoryProviderMapper(b.remoteMemoryProvider). // sbin_codex: topology config owns data-path policy.
		WithPageTable(b.pageTable).      // sbin_codex: pass page table for ideal-L1-TLB factory (todo 5).
		WithL1TLBFactory(b.l1tlbFactory) // sbin_codex: pass ideal-L1-TLB factory hook (todo 5).

	// sbin_claude_latpc: LATPC's per-SA pieces (RD on the CU coalescers,
	// LATC on the L1V TLBs) and the L1 TLB MSHR sizing knob.
	if b.latpcSettings.Enabled {
		saBuilder = saBuilder.WithLATPC(true)
	}
	if b.l1TLBMSHREntries > 0 {
		saBuilder = saBuilder.WithL1TLBMSHRSize(b.l1TLBMSHREntries)
	}

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
		// Pre-edit code (commented per project convention): the bank latency
		// was left at the writeback builder's 10-cycle default.
		//
		// sbin_claude: target spec - 20 cycle L2 lookup.
		WithBankLatency(20).
		WithNumReqPerCycle(16)

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
	// Pre-edit code (commented per AGENTS.md convention): remote traffic used
	// to reach the RDMA ingress directly.
	// b.remoteMemoryProvider.Port = b.rdmaEngine.RDMARequestInside.AsRemote()
	//
	// sbin_codex: the UVM access counter is now interposed on the remote
	// egress so every CPU-remote access is observed after translation.
	b.buildAccessCounter()

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

// buildAccessCounter interposes the GPU-side UVM counter between the address
// translators' remote egress and the RDMA ingress that carries it over PCIe.
// sbin_codex
func (b *Builder) buildAccessCounter() {
	if b.uvmServiceProvider == "" {
		b.remoteMemoryProvider.Port = b.rdmaEngine.RDMARequestInside.AsRemote()
		return
	}

	threshold := b.accessCounterThresh
	if threshold == 0 {
		threshold = 8
	}

	b.accessCounter = accesscounter.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithDeviceID(b.gpuID).
		WithThreshold(threshold).
		WithMaxOutstanding(b.maxOutstandingRemote). // sbin_claude
		WithBottomDestination(b.rdmaEngine.RDMARequestInside.AsRemote()).
		Build(fmt.Sprintf("%s.UVMAccessCounter", b.name))

	b.simulation.RegisterComponent(b.accessCounter)

	b.remoteMemoryProvider.Port = b.accessCounter.Top.AsRemote()
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
	b.cp.Driver = b.driverPort // sbin_codex: RDMA/shootdown ACKs target Driver.GPU.

	b.simulation.RegisterComponent(b.cp)

	b.buildDMAEngine()
	b.buildRDMAEngine()
	b.buildPageMigrationController()
}

// buildGMMU creates the GPU-side page-table walker. It resolves L2 TLB misses
// against this GPU's page table and keeps the CPU MMU as the downstream
// external translation endpoint for the v4 platform topology. // sbin_codex
func (b *Builder) buildGMMU() {
	gmmuBuilder := gmmu.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithLog2PageSize(b.log2PageSize).
		WithPageWalkingLatency(100).
		WithPageTable(b.pageTable).
		WithDeviceID(b.gpuID).
		WithMemAddrOffset(b.memAddrOffset).
		WithMemoryPerChiplet(b.dramSize)

	// sbin_claude_hpt: FS-HPT resolves a walk in one memory reference and has
	// no intermediate levels, so the GMMU skips the page-walk cache.
	if b.hptSettings.Enabled {
		gmmuBuilder = gmmuBuilder.
			WithHashedPageTable(true).
			WithHPTAccessesPerWalk(b.hptSettings.AccessesPerWalk)
	}

	// sbin_claude_softwalker: SoftWalker walks in software on the CUs, so
	// the concurrency cap becomes CUs x SoftPWB slots instead of the
	// hardware walker count.
	if b.softWalkerSettings.Enabled {
		gmmuBuilder = gmmuBuilder.WithSoftwareWalk(gmmu.SoftwareWalkConfig{
			NumCores:       b.numCUPerShaderArray * b.numShaderArray,
			SlotsPerCore:   b.softWalkerSettings.SlotsPerCU,
			CommCycles:     b.softWalkerSettings.CommCycles,
			SetupCycles:    b.softWalkerSettings.SetupCycles,
			PerLevelCycles: b.softWalkerSettings.PerLevelCycles,
		})
	}

	// sbin_claude_softwalker: baseline hardware-PTW sweep knob (paper
	// Figure 5 replication). 0 keeps the GMMU builder default.
	if b.gmmuMaxInflight > 0 {
		gmmuBuilder = gmmuBuilder.WithMaxNumReqInFlight(b.gmmuMaxInflight)
	}
	// sbin_claude_latpc: LATP batches same-group walks in the GMMU.
	if b.latpcSettings.Enabled {
		gmmuBuilder = gmmuBuilder.WithLATPBatching(true)
		if b.latpcSettings.L4RowHitLatency > 0 {
			gmmuBuilder = gmmuBuilder.
				WithLATPL4RowHitLatency(b.latpcSettings.L4RowHitLatency)
		}
	}

	if b.uvmServiceProvider != "" { // sbin_codex
		// Pre-edit code (commented per AGENTS.md convention). The GMMU used to
		// send faults straight to the driver:
		// gmmuBuilder = gmmuBuilder.WithUVMServiceProvider(b.uvmServiceProvider)
		//
		// sbin_codex: faults now leave through the Command Processor.
		gmmuBuilder = gmmuBuilder.WithUVMServiceProvider(
			b.cp.ToUVMInternal.AsRemote())
		// if b.accessCounterThresh > 0 { // sbin_codex
		// 	gmmuBuilder = gmmuBuilder.WithAccessCounterThreshold(b.accessCounterThresh)
		// }
	}

	b.gmmu = gmmuBuilder.Build(b.name + ".GMMU")

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
	// sbin_claude_avatar v2: the GMMU's out-of-band walk-cancel ingress
	// shares this connection so the L2 TLB bottom port can reach it. Inert
	// unless the Avatar speculation topology wires a cancel provider.
	conn.PlugIn(b.gmmu.GetPortByName("Cancel"))
	for _, l2TLB := range b.l2TLBs {
		conn.PlugIn(l2TLB.GetPortByName("Bottom"))
	}
}

func (b *Builder) buildL2TLB() {
	numEntries := 1024
	// Pre-edit code (commented per project convention):
	// numWays := 64
	//
	// sbin_claude_fbt: 64 ways over 512 entries left only 8 sets, so a
	// working set of a few thousand pages piled onto them and the same page
	// was evicted and re-walked repeatedly. 16 ways gives 32 sets.
	numWays := 16
	// numSets := int(numEntries/numWays)
	numSets := numEntries / numWays // sbin_codex: restore the baseline expression shape; both operands are int.

	// sbin_claude_softwalker: baseline MSHR sweep knob; 64 stays the default.
	numMSHREntry := 64
	if b.l2TLBNumMSHR > 0 {
		numMSHREntry = b.l2TLBNumMSHR
	}

	builder := tlb.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithNumWays(numWays).
		WithNumSets(numSets).
		// Pre-edit code (commented per project convention):
		// WithNumMSHREntry(64).
		WithNumMSHREntry(numMSHREntry). // sbin_claude_softwalker
		// Pre-edit code (commented per project convention):
		// WithNumReqPerCycle(1024).
		//
		// sbin_claude_fbt: 1024 requests per cycle made the shared L2 TLB an
		// infinite-bandwidth structure. The serialisation at a shared TLB is
		// the overhead virtual caching exists to filter, so modelling it away
		// left virtual caching with nothing to win. It also sized every L2
		// TLB port buffer at 1024 entries.
		WithNumReqPerCycle(16).
		// sbin_claude_fbt: a shared, second-level structure is not on the
		// critical path of an L1 hit, so it is not latency-critical.
		// WithLatency(10).
		//
		// sbin_claude: target spec - 20 cycle L2 TLB lookup.
		WithLatency(20).
		// sbin_claude_vc: the memory topology decides whether the L2 TLB
		// needs a second top channel for the memory-side translators.
		WithNumTopChannels(b.memoryTopology.l2TLBTopChannels()).
		WithLog2PageSize(b.log2PageSize).
		// WithLowModule(b.gmmu.GetPortByName("Top").AsRemote()). // sbin_codex: route L2 misses through GMMU.
		// Pre-edit code (commented per project convention):
		// WithTranslationProviderMapper(&mem.SinglePortMapper{
		// 	Port: b.gmmu.GetPortByName("Top").AsRemote(), // sbin_codex
		// })
		//
		// sbin_claude_utopia: the translation topology decides whether L2 TLB
		// misses go to the GMMU (baseline) or the UTU RestSeg walker (utopia).
		WithTranslationProviderMapper(&mem.SinglePortMapper{
			Port: b.translationTopology.l2TLBTranslationProvider(b),
		})

	// sbin_claude_softwalker: In-TLB MSHR - without it the dedicated MSHR
	// caps outstanding walks and the software walkers' concurrency is
	// unreachable (paper Figure 12).
	if b.softWalkerSettings.Enabled && b.softWalkerSettings.InTLBMSHRMax > 0 {
		builder = builder.WithInTLBMSHR(b.softWalkerSettings.InTLBMSHRMax)
	}

	l2TLB := builder.Build(fmt.Sprintf("%s.L2TLB", b.name))

	b.simulation.RegisterComponent(l2TLB)
	b.l2TLBs = append(b.l2TLBs, l2TLB)

	b.l1TLBAddressMapper.Port = l2TLB.GetPortByName("Top").AsRemote()
}

func (b *Builder) numCU() int {
	return b.numCUPerShaderArray * b.numShaderArray
}
