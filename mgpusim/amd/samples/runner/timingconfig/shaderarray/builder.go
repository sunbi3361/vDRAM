// Package shaderarray provides a builder for a shader array.
package shaderarray

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/writearound"
	"github.com/sarchlab/akita/v4/mem/cache/writethrough"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/emu"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cu"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rob"
)

// Builder builds a shader array.
type Builder struct {
	simulation *simulation.Simulation

	gpuID                     uint64
	name                      string
	numCUs                    int
	freq                      sim.Freq
	log2CacheLineSize         uint64
	log2PageSize              uint64
	wfPoolSize                int
	vgprCount                 []int
	numSinglePrecisionUnits   int
	vecMemInstPipelineStages  int
	vecMemTransPipelineStages int
	vecMemTransPipelineWidth  int
	cuMemPipelineBufferSize   int
	l1vCacheSize              uint64
	l1vBankLatency            int
	memPipelineBufferSize     int
	maxCoalescingPenalty      int
	registerScoreboard        bool
	// sbin_codex: injected strategy owns vector/scalar data-path construction and wiring.
	dataPathTopology   DataPathTopology
	remoteDataPath     remoteDataPathConfig // sbin_codex
	l1AddressMapper    mem.AddressToPortMapper
	l1iAddressMapper   mem.AddressToPortMapper // sbin_codex: preserve an independent instruction path mapper.
	l1TLBAddressMapper mem.AddressToPortMapper
	l1tlbFactory       func(name string, engine sim.Engine, freq sim.Freq, pageTable vm.PageTable, mapper mem.AddressToPortMapper, numReqPerCycle int) sim.Component //nolint:lll // sbin_codex: ideal-L1-TLB factory hook (todo 4).
	// sbin_codex: page table passed to ideal-L1-TLB factory (todo 4).
	pageTable  vm.PageTable
	aluFactory emu.ALUFactory

	sa        *sim.Domain
	cus       []*cu.ComputeUnit
	l1vROBs   []*rob.ReorderBuffer
	l1sROB    *rob.ReorderBuffer
	l1iROB    *rob.ReorderBuffer
	l1vATs    []*addresstranslator.Comp
	l1sAT     *addresstranslator.Comp
	l1iAT     *addresstranslator.Comp
	l1vCaches []*writearound.Comp
	l1sCache  *writethrough.Comp
	l1iCache  *writethrough.Comp
	l1vTLBs   []sim.Component // sbin_codex: relax concrete type for ideal-L1-TLB injection (todo 4).
	l1sTLB    sim.Component   // sbin_codex: relax concrete type for ideal-L1-TLB injection (todo 4).
	l1iTLB    sim.Component   // sbin_codex: relax concrete type for ideal-L1-TLB injection (todo 4).

	// Mapper pointers to allow left-to-right component build order
	// Vector path: ROB -> AT -(mem)-> L1V Cache, AT -(xlate)-> L1V TLB
	l1vMemMappers   []*mem.SinglePortMapper
	l1vTransMappers []*mem.SinglePortMapper

	// Scalar path: ROB -> AT -(mem)-> L1S Cache, AT -(xlate)-> L1S TLB
	l1sMemMapper   *mem.SinglePortMapper
	l1sTransMapper *mem.SinglePortMapper

	// Instruction path: ROB -> L1I Cache -(mem)-> AT -(xlate)-> L1I TLB
	l1iCacheMapper *mem.SinglePortMapper
	l1iTransMapper *mem.SinglePortMapper

	connectionCount int
}

// MakeBuilder creates a new builder.
func MakeBuilder() Builder {
	return Builder{
		numCUs:            4,
		freq:              1 * sim.GHz,
		log2CacheLineSize: 6,
		log2PageSize:      12,
		dataPathTopology:  NewBaselineDataPathTopology(), // sbin_codex: baseline remains the nil-free default.
	}
}

// WithSimulation sets the simulation to use.
func (b Builder) WithSimulation(sim *simulation.Simulation) Builder {
	b.simulation = sim
	return b
}

// WithGPUID sets the GPU ID to use.
func (b Builder) WithGPUID(gpuID uint64) Builder {
	b.gpuID = gpuID
	return b
}

// WithNumCUs sets the number of CUs to use.
func (b Builder) WithNumCUs(numCUs int) Builder {
	b.numCUs = numCUs
	return b
}

// WithFreq sets the frequency to use.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithLog2CacheLineSize sets the log2 cache line size to use.
func (b Builder) WithLog2CacheLineSize(log2CacheLineSize uint64) Builder {
	b.log2CacheLineSize = log2CacheLineSize
	return b
}

// WithLog2PageSize sets the log2 page size to use.
func (b Builder) WithLog2PageSize(log2PageSize uint64) Builder {
	b.log2PageSize = log2PageSize
	return b
}

// WithL1AddressMapper sets the L1 address mapper to use.
func (b Builder) WithL1AddressMapper(
	l1AddressMapper mem.AddressToPortMapper,
) Builder {
	b.l1AddressMapper = l1AddressMapper
	return b
}

// WithL1IAddressMapper sets the L1 instruction address mapper.
// sbin_codex: when unset, L1I continues to use the shared L1 mapper.
func (b Builder) WithL1IAddressMapper(
	l1iAddressMapper mem.AddressToPortMapper,
) Builder {
	b.l1iAddressMapper = l1iAddressMapper
	return b
}

// WithL1TLBAddressMapper sets the L1 TLB address mapper to use.
func (b Builder) WithL1TLBAddressMapper(
	l1TLBAddressMapper mem.AddressToPortMapper,
) Builder {
	b.l1TLBAddressMapper = l1TLBAddressMapper
	return b
}

// WithL1TLBFactory sets a factory that creates an ideal L1 TLB component.
// sbin_codex: used by ideal-L1-TLB GPU configs (todo 4).
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

// WithPageTable sets the page table passed to the ideal L1 TLB factory.
// sbin_codex: used by ideal-L1-TLB GPU configs (todo 4).
func (b Builder) WithPageTable(pageTable vm.PageTable) Builder {
	b.pageTable = pageTable
	return b
}

// WithWfPoolSize sets the wavefront pool size for the CU builder.
func (b Builder) WithWfPoolSize(n int) Builder {
	b.wfPoolSize = n
	return b
}

// WithVGPRCount sets the VGPR counts for the CU builder.
func (b Builder) WithVGPRCount(counts []int) Builder {
	b.vgprCount = counts
	return b
}

// WithALUFactory sets the ALU factory for creating compute unit ALUs.
// This allows using different ALU implementations (e.g., GCN3 vs CDNA3).
func (b Builder) WithALUFactory(factory emu.ALUFactory) Builder {
	b.aluFactory = factory
	return b
}

// WithNumSinglePrecisionUnits sets the number of single-precision units per
// SIMD in each CU.
func (b Builder) WithNumSinglePrecisionUnits(n int) Builder {
	b.numSinglePrecisionUnits = n
	return b
}

// WithVecMemInstPipelineStages sets the vector memory instruction pipeline
// depth for each CU.
func (b Builder) WithVecMemInstPipelineStages(n int) Builder {
	b.vecMemInstPipelineStages = n
	return b
}

// WithVecMemTransPipelineStages sets the vector memory transaction pipeline
// depth for each CU.
func (b Builder) WithVecMemTransPipelineStages(n int) Builder {
	b.vecMemTransPipelineStages = n
	return b
}

// WithVecMemTransPipelineWidth sets the width (items per cycle) of the
// vector memory transaction pipeline for each CU. Default is 1.
func (b Builder) WithVecMemTransPipelineWidth(n int) Builder {
	b.vecMemTransPipelineWidth = n
	return b
}

// WithCUMemPipelineBufferSize sets the CU-internal post-pipeline buffer
// size for vector memory transactions. Default is 8.
func (b Builder) WithCUMemPipelineBufferSize(n int) Builder {
	b.cuMemPipelineBufferSize = n
	return b
}

// WithL1VCacheSize sets the L1V cache size per CU in bytes.
func (b Builder) WithL1VCacheSize(size uint64) Builder {
	b.l1vCacheSize = size
	return b
}

// WithL1VBankLatency sets the L1V cache bank latency in cycles.
func (b Builder) WithL1VBankLatency(latency int) Builder {
	b.l1vBankLatency = latency
	return b
}

// WithMemPipelineBufferSize sets the buffer size for memory pipeline
// connections (CU→ROB→AT→L1V). Larger values allow more concurrent
// memory transactions, improving throughput for bandwidth-limited workloads.
func (b Builder) WithMemPipelineBufferSize(size int) Builder {
	b.memPipelineBufferSize = size
	return b
}

// WithMaxCoalescingPenalty sets the maximum coalescing penalty in cycles
// for poorly-coalesced read transactions in each CU.
func (b Builder) WithMaxCoalescingPenalty(n int) Builder {
	b.maxCoalescingPenalty = n
	return b
}

// WithRegisterScoreboard enables or disables the register scoreboard and
// SIMD pipelining feature in each CU.
func (b Builder) WithRegisterScoreboard(enabled bool) Builder {
	b.registerScoreboard = enabled
	return b
}

// WithDataPathTopology sets the vector/scalar cache topology strategy. // sbin_codex
func (b Builder) WithDataPathTopology(topology DataPathTopology) Builder {
	b.dataPathTopology = topology // sbin_codex
	return b
}

// Build builds the shader array.
func (b Builder) Build(name string) *sim.Domain {
	b.name = name
	b.sa = sim.NewDomain(name)

	b.buildComponents()
	b.connectComponents()

	return b.sa
}

func (b *Builder) buildComponents() {
	b.buildCUs()

	// Build in dataflow order
	b.buildL1VReorderBuffers()
	b.buildL1SReorderBuffer()
	b.dataPathTopology.build(b) // sbin_codex

	b.buildL1IReorderBuffer()
	b.buildL1ICache()
	b.buildL1IAddressTranslator()
	b.buildL1ITLB()

	b.populateExternalPorts()
}

func (b *Builder) populateExternalPorts() {
	for i := range b.numCUs {
		cu := b.cus[i]

		b.sa.AddPort(fmt.Sprintf("CU[%d]", i), cu.GetPortByName("Top"))
		b.sa.AddPort(fmt.Sprintf("CUCtrl[%d]", i), cu.GetPortByName("Ctrl"))
		b.sa.AddPort(fmt.Sprintf("L1VROBCtrl[%d]", i), b.l1vROBs[i].
			GetPortByName("Control"))
	}

	b.sa.AddPort("L1SROBCtrl", b.l1sROB.GetPortByName("Control"))
	b.dataPathTopology.addExternalPorts(b) // sbin_codex

	b.sa.AddPort("L1IROBCtrl", b.l1iROB.GetPortByName("Control"))
	b.sa.AddPort("L1IAddrTransCtrl", b.l1iAT.GetPortByName("Control"))
	b.sa.AddPort("L1ITLBCtrl", b.l1iTLB.GetPortByName("Control"))
	b.sa.AddPort("L1ICacheCtrl", b.l1iCache.GetPortByName("Control"))
	// Expose instruction memory egress to L2 via AT bottom
	b.sa.AddPort("L1ICacheBottom", b.l1iAT.GetPortByName("Bottom"))
	b.sa.AddPort("L1ITLBBottom", b.l1iTLB.GetPortByName("Bottom"))
}

func (b *Builder) connectComponents() {
	// b.connectVectorMem()
	// b.connectScalarMem()
	b.dataPathTopology.connect(b) // sbin_codex: injected topology owns vector/scalar connections.
	b.connectInstMem()
}

func (b *Builder) connectInstMem() {
	rob := b.l1iROB
	at := b.l1iAT
	tlb := b.l1iTLB
	l1i := b.l1iCache

	// Set mapper targets now that AT/TLB are built
	if b.l1iCacheMapper != nil {
		b.l1iCacheMapper.Port = at.GetPortByName("Top").AsRemote()
	}
	if b.l1iTransMapper != nil {
		b.l1iTransMapper.Port = tlb.GetPortByName("Top").AsRemote()
	}

	l1iTopPort := l1i.GetPortByName("Top")
	rob.BottomUnit = l1iTopPort.AsRemote()
	b.connectWithDirectConnection(rob.GetPortByName("Bottom"), l1iTopPort, 8)

	atTopPort := at.GetPortByName("Top")
	b.connectWithDirectConnection(l1i.GetPortByName("Bottom"), atTopPort, 8)

	tlbTopPort := tlb.GetPortByName("Top")
	b.connectWithDirectConnection(
		at.GetPortByName("Translation"), tlbTopPort, 8)

	robTopPort := rob.GetPortByName("Top")
	conn := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".InstMemConn")
	b.simulation.RegisterComponent(conn)

	conn.PlugIn(robTopPort)
	for i := range b.numCUs {
		cu := b.cus[i]
		cu.InstMem = rob.GetPortByName("Top")
		conn.PlugIn(cu.ToInstMem)
	}
}

func (b *Builder) connectWithDirectConnection(
	port1, port2 sim.Port,
	bufferSize int,
) {
	name := fmt.Sprintf("%s.Conn[%d]", b.name, b.connectionCount)
	b.connectionCount++

	conn := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(name)

	b.simulation.RegisterComponent(conn)

	conn.PlugIn(port1)
	conn.PlugIn(port2)
}

func (b *Builder) buildCUs() {
	cuBuilder := cu.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithLog2CachelineSize(b.log2CacheLineSize)

	if b.aluFactory != nil {
		cuBuilder = cuBuilder.WithALUFactory(b.aluFactory)
	}

	if b.wfPoolSize > 0 {
		cuBuilder = cuBuilder.WithWfPoolSize(b.wfPoolSize)
	}

	if b.vgprCount != nil {
		cuBuilder = cuBuilder.WithVGPRCount(b.vgprCount)
	}

	if b.numSinglePrecisionUnits > 0 {
		cuBuilder = cuBuilder.WithNumSinglePrecisionUnits(b.numSinglePrecisionUnits)
	}

	if b.vecMemInstPipelineStages > 0 {
		cuBuilder = cuBuilder.WithVecMemInstPipelineStages(b.vecMemInstPipelineStages)
	}

	if b.vecMemTransPipelineStages > 0 {
		cuBuilder = cuBuilder.WithVecMemTransPipelineStages(b.vecMemTransPipelineStages)
	}

	if b.vecMemTransPipelineWidth > 0 {
		cuBuilder = cuBuilder.WithVecMemTransPipelineWidth(b.vecMemTransPipelineWidth)
	}

	if b.cuMemPipelineBufferSize > 0 {
		cuBuilder = cuBuilder.WithMemPipelineBufferSize(b.cuMemPipelineBufferSize)
	}

	if b.maxCoalescingPenalty > 0 {
		cuBuilder = cuBuilder.WithMaxCoalescingPenalty(b.maxCoalescingPenalty)
	}

	if b.registerScoreboard {
		cuBuilder = cuBuilder.WithRegisterScoreboard(true)
	}

	for i := 0; i < b.numCUs; i++ {
		cuName := fmt.Sprintf("%s.CU[%d]", b.name, i)
		computeUnit := cuBuilder.Build(cuName)
		b.cus = append(b.cus, computeUnit)
		b.simulation.RegisterComponent(computeUnit)
	}
}

func (b *Builder) buildL1VReorderBuffers() {
	builder := rob.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithBufferSize(512).
		WithNumReqPerCycle(32)

	for i := 0; i < b.numCUs; i++ {
		name := fmt.Sprintf("%s.L1VROB[%d]", b.name, i)
		rob := builder.Build(name)
		b.l1vROBs = append(b.l1vROBs, rob)
		b.simulation.RegisterComponent(rob)

		// if b.visTracer != nil {
		// 	tracing.CollectTrace(rob, b.visTracer)
		// }
	}
}

func (b *Builder) buildL1VAddressTranslators() {
	// Pre-edit code (commented per AGENTS.md convention):
	// base := addresstranslator.MakeBuilder().
	// 	WithEngine(b.simulation.GetEngine()).
	// 	WithFreq(b.freq).
	// 	WithDeviceID(b.gpuID).
	// 	WithLog2PageSize(b.log2PageSize).
	// 	WithNumReqPerCycle(32)
	base := b.configureDataAddressTranslator(addresstranslator.MakeBuilder(). // sbin_codex
											WithEngine(b.simulation.GetEngine()).
											WithFreq(b.freq).
											WithDeviceID(b.gpuID).
											WithLog2PageSize(b.log2PageSize).
											WithNumReqPerCycle(32))

	b.l1vMemMappers = make([]*mem.SinglePortMapper, 0, b.numCUs)
	b.l1vTransMappers = make([]*mem.SinglePortMapper, 0, b.numCUs)

	for i := 0; i < b.numCUs; i++ {
		name := fmt.Sprintf("%s.L1VAddrTrans[%d]", b.name, i)
		memMapper := &mem.SinglePortMapper{}
		xlateMapper := &mem.SinglePortMapper{}
		curr := base.
			WithMemoryProviderMapper(memMapper).
			WithTranslationProviderMapper(xlateMapper)
		at := curr.Build(name)
		b.l1vATs = append(b.l1vATs, at)
		b.l1vMemMappers = append(b.l1vMemMappers, memMapper)
		b.l1vTransMappers = append(b.l1vTransMappers, xlateMapper)
		b.simulation.RegisterComponent(at)
	}
}

func (b *Builder) buildL1VTLBs() {
	// sbin_codex: ideal-L1-TLB factory branch (todo 4).
	if b.l1tlbFactory != nil {
		for i := 0; i < b.numCUs; i++ {
			name := fmt.Sprintf("%s.L1VTLB[%d]", b.name, i)
			comp := b.l1tlbFactory(name, b.simulation.GetEngine(), b.freq,
				b.pageTable, b.l1TLBAddressMapper, 32)
			b.l1vTLBs = append(b.l1vTLBs, comp)
			b.simulation.RegisterComponent(comp)
		}
		return
	}

	builder := tlb.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithNumMSHREntry(64).
		// Pre-edit code (commented per project convention):
		// WithNumSets(4).
		// WithNumWays(64).
		//
		// sbin_claude_fbt: 4 sets x 64 ways is a 256-entry per-CU TLB, eight
		// times the 32-entry fully associative TLB the virtual caching paper
		// evaluates as its baseline (ASPLOS'18 Table 1). A filter that large
		// keeps the shared TLB nearly idle, which is the pressure virtual
		// caching exists to relieve.
		WithNumSets(1).
		WithNumWays(32).
		WithNumReqPerCycle(32).
		WithLatency(1).
		// WithTranslationProviderMapper(b.l1TLBAddressMapper) // sbin_codex: pre-edit chain ending.
		WithTranslationProviderMapper(b.l1TLBAddressMapper).
		WithPageAdmissionPredicate(admitLocalPage) // sbin_codex

	for i := 0; i < b.numCUs; i++ {
		name := fmt.Sprintf("%s.L1VTLB[%d]", b.name, i)
		tlb := builder.Build(name)
		b.l1vTLBs = append(b.l1vTLBs, tlb)
		b.simulation.RegisterComponent(tlb)
	}
}

func (b *Builder) buildL1VCaches() {
	// Pre-edit code (commented per project convention):
	// l1vSize := 16 * mem.KB
	//
	// sbin_claude_fbt: the per-CU L1 data cache is the first stage of the
	// virtual cache hierarchy's translation filter, so its capacity decides
	// how much translation traffic ever reaches the shared TLB.
	l1vSize := 128 * mem.KB
	if b.l1vCacheSize > 0 {
		l1vSize = b.l1vCacheSize
	}

	l1vBankLatency := 20
	if b.l1vBankLatency > 0 {
		l1vBankLatency = b.l1vBankLatency
	}

	builder := writearound.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithBankLatency(l1vBankLatency).
		WithNumBanks(4).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(4).
		WithNumMSHREntry(128).
		WithNumReqsPerCycle(8).
		WithMaxNumConcurrentTrans(128).
		WithTotalByteSize(l1vSize).
		WithAddressToPortMapper(b.l1AddressMapper)

	for i := 0; i < b.numCUs; i++ {
		name := fmt.Sprintf("%s.L1VCache[%d]", b.name, i)
		cache := builder.Build(name)
		b.l1vCaches = append(b.l1vCaches, cache)
		b.simulation.RegisterComponent(cache)

		// if b.memTracer != nil {
		// 	tracing.CollectTrace(cache, b.memTracer)
		// }
	}
}

func (b *Builder) buildL1SReorderBuffer() {
	builder := rob.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithBufferSize(512).
		WithNumReqPerCycle(32)

	name := fmt.Sprintf("%s.L1SROB", b.name)
	rob := builder.Build(name)
	b.l1sROB = rob
	b.simulation.RegisterComponent(rob)
}

func (b *Builder) buildL1SAddressTranslator() {
	// Prepare mappers and set ports later when cache/TLB are ready
	if b.l1sMemMapper == nil {
		b.l1sMemMapper = &mem.SinglePortMapper{}
	}
	if b.l1sTransMapper == nil {
		b.l1sTransMapper = &mem.SinglePortMapper{}
	}
	// Pre-edit code (commented per AGENTS.md convention):
	// builder := addresstranslator.MakeBuilder().
	// 	WithEngine(b.simulation.GetEngine()).
	// 	WithFreq(b.freq).
	// 	WithDeviceID(b.gpuID).
	// 	WithLog2PageSize(b.log2PageSize).
	// 	WithNumReqPerCycle(32).
	// 	WithMemoryProviderMapper(b.l1sMemMapper).
	// 	WithTranslationProviderMapper(b.l1sTransMapper)
	builder := b.configureDataAddressTranslator(addresstranslator.MakeBuilder(). // sbin_codex
											WithEngine(b.simulation.GetEngine()).
											WithFreq(b.freq).
											WithDeviceID(b.gpuID).
											WithLog2PageSize(b.log2PageSize).
											WithNumReqPerCycle(32).
											WithMemoryProviderMapper(b.l1sMemMapper).
											WithTranslationProviderMapper(b.l1sTransMapper))

	name := fmt.Sprintf("%s.L1SAddrTrans", b.name)
	at := builder.Build(name)
	b.l1sAT = at
	b.simulation.RegisterComponent(at)
}

func (b *Builder) buildL1STLB() {
	// sbin_codex: ideal-L1-TLB factory branch (todo 4).
	if b.l1tlbFactory != nil {
		name := fmt.Sprintf("%s.L1STLB", b.name)
		comp := b.l1tlbFactory(name, b.simulation.GetEngine(), b.freq,
			b.pageTable, b.l1TLBAddressMapper, 32)
		b.l1sTLB = comp
		b.simulation.RegisterComponent(comp)
		return
	}

	builder := tlb.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithNumMSHREntry(64).
		WithNumSets(1).
		WithNumWays(64).
		WithNumReqPerCycle(32).
		// WithTranslationProviderMapper(b.l1TLBAddressMapper) // sbin_codex: pre-edit chain ending.
		WithTranslationProviderMapper(b.l1TLBAddressMapper).
		WithPageAdmissionPredicate(admitLocalPage) // sbin_codex

	name := fmt.Sprintf("%s.L1STLB", b.name)
	tlb := builder.Build(name)
	b.l1sTLB = tlb
	b.simulation.RegisterComponent(tlb)
}

func (b *Builder) buildL1SCache() {
	builder := writethrough.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithBankLatency(1).
		WithNumBanks(1).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(4).
		WithNumMSHREntry(128).
		WithNumReqsPerCycle(32).
		WithTotalByteSize(16 * mem.KB).
		WithAddressToPortMapper(b.l1AddressMapper)

	name := fmt.Sprintf("%s.L1SCache", b.name)
	cache := builder.Build(name)
	b.l1sCache = cache
	b.simulation.RegisterComponent(cache)

	// if b.memTracer != nil {
	// 	tracing.CollectTrace(cache, b.memTracer)
	// }
}

func (b *Builder) buildL1IReorderBuffer() {
	builder := rob.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithBufferSize(128).
		WithNumReqPerCycle(4)

	name := fmt.Sprintf("%s.L1IROB", b.name)
	rob := builder.Build(name)
	b.l1iROB = rob
	b.simulation.RegisterComponent(rob)
}

func (b *Builder) buildL1IAddressTranslator() {
	if b.l1iTransMapper == nil {
		b.l1iTransMapper = &mem.SinglePortMapper{}
	}
	l1iAddressMapper := b.l1iAddressMapper // sbin_codex: explicit L1I mapper wins.
	if l1iAddressMapper == nil {
		l1iAddressMapper = b.l1AddressMapper // sbin_codex: preserve the builder's existing default.
	}
	builder := addresstranslator.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithDeviceID(b.gpuID).
		WithLog2PageSize(b.log2PageSize).
		WithNumReqPerCycle(16).
		// WithMemoryProviderMapper(b.l1AddressMapper).
		WithMemoryProviderMapper(l1iAddressMapper). // sbin_codex
		WithTranslationProviderMapper(b.l1iTransMapper)

	name := fmt.Sprintf("%s.L1IAddrTrans", b.name)
	at := builder.Build(name)
	b.l1iAT = at
	b.simulation.RegisterComponent(at)
}

func (b *Builder) buildL1ITLB() {
	// sbin_codex: ideal-L1-TLB factory branch (todo 4).
	if b.l1tlbFactory != nil {
		name := fmt.Sprintf("%s.L1ITLB", b.name)
		comp := b.l1tlbFactory(name, b.simulation.GetEngine(), b.freq,
			b.pageTable, b.l1TLBAddressMapper, 4)
		b.l1iTLB = comp
		b.simulation.RegisterComponent(comp)
		return
	}

	builder := tlb.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithNumMSHREntry(4).
		WithNumSets(1).
		WithNumWays(64).
		WithNumReqPerCycle(4).
		WithTranslationProviderMapper(b.l1TLBAddressMapper)

	name := fmt.Sprintf("%s.L1ITLB", b.name)
	tlb := builder.Build(name)
	b.l1iTLB = tlb
	b.simulation.RegisterComponent(tlb)
}

func (b *Builder) buildL1ICache() {
	if b.l1iCacheMapper == nil {
		b.l1iCacheMapper = &mem.SinglePortMapper{}
	}
	builder := writethrough.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithBankLatency(1).
		WithNumBanks(1).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(4).
		WithNumMSHREntry(16).
		WithTotalByteSize(32 * mem.KB).
		WithNumReqsPerCycle(4).
		WithAddressToPortMapper(b.l1iCacheMapper)

	name := fmt.Sprintf("%s.L1ICache", b.name)
	cache := builder.Build(name)
	b.l1iCache = cache
	b.simulation.RegisterComponent(cache)
	// if b.memTracer != nil {
	// 	tracing.CollectTrace(cache, b.memTracer)
	// }
}
