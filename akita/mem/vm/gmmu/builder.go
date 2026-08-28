package gmmu

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/pagewalkcache"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// A Builder can build GMMU component
type Builder struct {
	engine              sim.Engine
	freq                sim.Freq
	log2PageSize        uint64
	pageTable           vm.PageTable
	maxNumReqInFlight   int
	pageWalkingLatency  int
	deviceID            uint64
	memAddrOffset       uint64
	memoryPerChiplet    uint64
	addressToPortMapper mem.AddressToPortMapper // sbin_gmmu
	rpcacheEnabled      bool
	rpcacheNumSets      uint64
	rpcacheNumWays      uint64
	rpcacheLineSize     uint64
	rpcacheBytesPerChip uint64
	uvmServiceProvider  sim.RemotePort // sbin_codex: UVM fault service provider.
	// accessCounterThresh uint64 // sbin_codex

	// sbin_claude_hpt: hashed-page-table (FS-HPT, PACT'24) walk mode. When
	// enabled the walk costs hptAccessesPerWalk memory references instead of
	// one per radix level, and the page-walk cache is not built at all
	// because a hashed table has no intermediate levels to cache.
	hptEnabled         bool
	hptAccessesPerWalk int

	// sbin_claude_softwalker: SoftWalker (MICRO'25) software walk mode.
	swEnabled bool
	swConfig  SoftwareWalkConfig
	// sbin_claude_latpc: LATP batched page walks (MICRO'25 §5.4). Members
	// of an in-flight walk's group share its walker slot and are answered
	// at the L4 row-buffer-hit latency each after the lead completes.
	latpEnabled         bool
	latpL4RowHitLatency int
	// sbin_claude_latpc: depth of the queue that holds translation requests
	// waiting for a walker (paper Table 2: 128).
	pwQueueSize int
	// sbin_claude_latpc: false tags PW Buffer entries by the Regularity
	// Detector group ID; true uses the paper's Base Address + Stride*Index
	// arithmetic (Fig. 15).
	latpAddrTag bool
}

// MakeBuilder creates a new builder
func MakeBuilder() Builder {
	return Builder{
		freq:               1 * sim.GHz,
		log2PageSize:       12,
		maxNumReqInFlight:  16,
		pageWalkingLatency: 100, // sbin_codex: latency of each uncached page-table level.
		hptAccessesPerWalk: 1,   // sbin_claude_hpt: ideal HPT resolves in one access.
		// sbin_claude_latpc: a row-buffer hit is a CAS-only access, ~1/5 of
		// the 100-cycle modeled memory reference (refs/latpc-plan.md 1.4).
		latpL4RowHitLatency: 20,
		// sbin_claude_latpc: the queue that holds translation requests
		// waiting for a walker. MICRO'25 Table 2 sizes it at 128 entries,
		// and it is baseline hardware, not a LATPC addition - LATPC only
		// searches it.
		pwQueueSize: 128,
		// sbin_claude_latpc: tag PW Buffer entries the way MICRO'25 Fig. 15
		// does (Base Address + Stride*Index over a 32-bit valid mask). The
		// Regularity Detector group ID is the strictly narrower tag and is
		// kept only as an ablation.
		latpAddrTag: true,
	}
}

// WithEngine sets the engine to be used with the GMMU
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the GMMU to work at
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithLog2PageSize sets the page size that the gmmu support.
func (b Builder) WithLog2PageSize(log2PageSize uint64) Builder {
	b.log2PageSize = log2PageSize
	return b
}

// WithPageTable sets the page table that the GMMU uses.
func (b Builder) WithPageTable(pageTable vm.PageTable) Builder {
	b.pageTable = pageTable
	return b
}

// WithMaxNumReqInFlight sets the number of requests can be concurrently
// processed by the GMMU.
func (b Builder) WithMaxNumReqInFlight(maxNumReqInFlight int) Builder {
	b.maxNumReqInFlight = maxNumReqInFlight
	return b
}

// WithPageWalkingLatency sets the latency of each page-table level. // sbin_codex
func (b Builder) WithPageWalkingLatency(pageWalkingLatency int) Builder {
	b.pageWalkingLatency = pageWalkingLatency
	return b
}

// WithDeviceID sets the device ID of the GMMU
func (b Builder) WithDeviceID(deviceID uint64) Builder {
	b.deviceID = deviceID
	return b
}

// WithMemAddrOffset sets the base physical address of GPU memory.
func (b Builder) WithMemAddrOffset(memAddrOffset uint64) Builder {
	b.memAddrOffset = memAddrOffset
	return b
}

// WithMemoryPerChiplet sets the physical memory span of a single chiplet.
func (b Builder) WithMemoryPerChiplet(memoryPerChiplet uint64) Builder {
	b.memoryPerChiplet = memoryPerChiplet
	return b
}

// sbin_gmmu
// WithAddressToPortMapper sets the AddressToPortMapper to be used.
func (b Builder) WithAddressToPortMapper(f mem.AddressToPortMapper) Builder {
	b.addressToPortMapper = f
	return b
}

// WithUVMServiceProvider sets the driver UVM manager port that services
// managed-page demand faults. When nil, managed-page gating is disabled.
func (b Builder) WithUVMServiceProvider(provider sim.RemotePort) Builder {
	b.uvmServiceProvider = provider
	return b
}

// WithHashedPageTable selects the FS-HPT walk mode. A hashed page table is
// indexed directly by hash(VPN), so a walk is one memory reference rather than
// one per radix level. // sbin_claude_hpt
func (b Builder) WithHashedPageTable(enabled bool) Builder {
	b.hptEnabled = enabled
	return b
}

// WithHPTAccessesPerWalk sets how many memory references one hashed-page-table
// walk costs. Ideal HPT (no hash collision) is 1; larger values model
// collision chains. Only meaningful with WithHashedPageTable(true).
// sbin_claude_hpt
func (b Builder) WithHPTAccessesPerWalk(n int) Builder {
	b.hptAccessesPerWalk = n
	return b
}

// WithSoftwareWalk selects the SoftWalker (MICRO'25) software walk mode:
// page walks execute as PW-warp threads on the GPU cores, so walk
// concurrency scales to cfg.NumCores x cfg.SlotsPerCore instead of
// maxRequestsInFlight, and each walk pays communication plus instruction
// latency on top of the unchanged radix+PWC traversal. Mutually exclusive
// with WithHashedPageTable. sbin_claude_softwalker
func (b Builder) WithSoftwareWalk(cfg SoftwareWalkConfig) Builder {
	b.swEnabled = true
	b.swConfig = cfg
	return b
}

// WithLATPBatching selects LATP batched page walks (MICRO'25 §5.4): a
// translation request whose GroupID matches an in-flight walk attaches to it
// as a member instead of taking a walker slot; the L1-L3 traversal and the
// page-walk cache are shared, and each member costs one L4 row-buffer hit
// after the lead completes. Non-UVM only. Off by default. // sbin_claude_latpc
func (b Builder) WithLATPBatching(enabled bool) Builder {
	b.latpEnabled = enabled
	return b
}

// WithLATPL4RowHitLatency sets the cycles one batch member's L4 PTE load
// costs (a DRAM row-buffer hit). Only meaningful with WithLATPBatching(true).
// sbin_claude_latpc
func (b Builder) WithLATPL4RowHitLatency(cycles int) Builder {
	b.latpL4RowHitLatency = cycles
	return b
}

// WithPageWalkQueue sets the depth of the queue that holds translation
// requests waiting for a page table walker (MICRO'25 Table 2 and Figure 10:
// 128 entries). Admission into a walker is in order, so the depth changes
// nothing on its own; the queue is what LATP's PW Buffer tag check searches,
// so a request that can coalesce does not have to reach the head of the line
// before the walk it would have joined retires. // sbin_claude_latpc
func (b Builder) WithPageWalkQueue(entries int) Builder {
	b.pwQueueSize = entries
	return b
}

// WithLATPAddressTag selects the paper's PW Buffer tag (Figure 15):
// Base Address + Stride*Index, with a 32-bit Valid Mask, carrying no warp
// instruction identity, so walks issued by different warp instructions can
// share an entry and a group's demand can claim the base slot of an entry a
// lone prefetch opened. On by default; tagging by the Regularity Detector's
// group ID instead is the ablation. Only meaningful with
// WithLATPBatching(true). // sbin_claude_latpc
func (b Builder) WithLATPAddressTag(enabled bool) Builder {
	b.latpAddrTag = enabled
	return b
}

// func (b Builder) WithAccessCounterThreshold(thresh uint64) Builder { // sbin_codex
// 	b.accessCounterThresh = thresh
// 	return b
// }

func (b Builder) Build(name string) *Comp {
	// sbin_codex: reject a latency that cannot represent a cycle countdown.
	if b.pageWalkingLatency < 0 {
		panic("GMMU page-walking latency must not be negative")
	}

	// sbin_claude_hpt: a walk must cost at least one memory reference.
	if b.hptEnabled && b.hptAccessesPerWalk < 1 {
		panic("GMMU HPT accesses per walk must be at least 1")
	}

	b.validateSoftwareWalk() // sbin_claude_softwalker
	// sbin_claude_latpc: a member's L4 access must cost at least one cycle.
	if b.latpEnabled && b.latpL4RowHitLatency < 1 {
		panic("GMMU LATP L4 row-hit latency must be at least 1")
	}

	// sbin_claude_latpc: a page walk queue has to be able to hold the head.
	if b.pwQueueSize < 1 {
		panic("GMMU page walk queue must hold at least one request")
	}

	gmmu := new(Comp)
	gmmu.TickingComponent = *sim.NewTickingComponent(
		name, b.engine, b.freq, gmmu)

	b.createPorts(name, gmmu)
	b.createPageTable(gmmu)
	// Pre-edit code (commented per AGENTS.md convention):
	// b.createPageWalkCache(name, gmmu)
	//
	// sbin_claude_hpt: a hashed page table has no intermediate levels, so no
	// page-walk cache is built in HPT mode. The middleware guards the nil
	// port.
	if !b.hptEnabled {
		b.createPageWalkCache(name, gmmu)
	}
	b.configureInternalStates(gmmu)

	ctrlMiddleware := &ctrlMiddleware{Comp: gmmu}
	gmmu.AddMiddleware(ctrlMiddleware)

	middleware := &middleware{Comp: gmmu} // sbin_gmmu
	gmmu.AddMiddleware(middleware)        // sbin_gmmu

	return gmmu
}

// validateSoftwareWalk rejects software-walk configurations that cannot
// model anything sensible. sbin_claude_softwalker
func (b Builder) validateSoftwareWalk() {
	if !b.swEnabled {
		return
	}

	if b.hptEnabled {
		panic("GMMU cannot combine the software walk mode with HPT")
	}

	if b.swConfig.NumCores < 1 || b.swConfig.SlotsPerCore < 1 {
		panic("GMMU software walk needs at least one core and one slot")
	}

	if b.swConfig.CommCycles < 0 || b.swConfig.SetupCycles < 0 ||
		b.swConfig.PerLevelCycles < 0 {
		panic("GMMU software walk latencies must not be negative")
	}
}

func (b Builder) configureInternalStates(c *Comp) {
	c.maxRequestsInFlight = b.maxNumReqInFlight
	c.latency = b.pageWalkingLatency
	c.deviceID = b.deviceID
	c.memAddrOffset = b.memAddrOffset
	c.memoryPerChiplet = b.memoryPerChiplet     // sbin_codex
	c.log2PageSize = b.log2PageSize             // sbin_codex
	c.state = gmmuStateEnable                   // sbin_codex
	c.hptEnabled = b.hptEnabled                 // sbin_claude_hpt
	c.hptAccessesPerWalk = b.hptAccessesPerWalk // sbin_claude_hpt
	c.swEnabled = b.swEnabled                   // sbin_claude_softwalker
	c.swConfig = b.swConfig                     // sbin_claude_softwalker
	if b.swEnabled {                            // sbin_claude_softwalker
		c.swCoreInFlight = make([]int, b.swConfig.NumCores)
	}
	c.latpEnabled = b.latpEnabled                 // sbin_claude_latpc
	c.latpL4RowHitLatency = b.latpL4RowHitLatency // sbin_claude_latpc
	c.pwQueueSize = b.pwQueueSize                 // sbin_claude_latpc
	c.latpAddrTag = b.latpAddrTag                 // sbin_claude_latpc
	c.canceledReqs = make(map[string]struct{})    // sbin_claude_avatar
	c.addressToPortMapper = b.addressToPortMapper // sbin_gmmu
	c.UVMServiceProvider = b.uvmServiceProvider   // sbin_codex
	// c.accessCounterThreshold = b.accessCounterThresh // sbin_codex
	// c.accessCounters = make(map[uint64]uint64)
	// c.accessCounterNotified = make(map[uint64]bool)
}

func (b Builder) createPageWalkCache(name string, c *Comp) {
	// Pre-edit code (commented per project convention): the page-walk cache
	// was built without a lookup latency, so a hit cost zero cycles.
	//
	// sbin_claude: target spec - 128-entry page walk cache, 10 cycle lookup.
	pageWalkCacheBuilder := pagewalkcache.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq). // sbin_codex: keep cache and GMMU in the same clock domain.
		WithLog2PageSize(b.log2PageSize).
		WithNumLevels(pageTableLevels). // sbin_codex
		WithNumBlocks(128).             // sbin_codex
		WithLatency(10).                // sbin_claude
		WithBitsPerLevel(9)

	pageWalkCache := pageWalkCacheBuilder.Build(name + ".PageWalkCache")

	c.pageWalkCache = pageWalkCache.GetPortByName("Top")
	c.pageWalkCachePort = sim.NewPort(c, 4096, 4096, name+".PageWalkCachePort")
	c.AddPort("PageWalkCache", c.pageWalkCachePort)

	gmmuToPageWalkCache := directconnection.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		Build(name + "GMMUToPageWalkCache")

	gmmuToPageWalkCache.PlugIn(pageWalkCache.GetPortByName("Top"))
	gmmuToPageWalkCache.PlugIn(c.pageWalkCachePort)
}

func (b Builder) createPageTable(c *Comp) {
	if b.pageTable != nil {
		c.pageTable = b.pageTable
	} else {
		panic("GMMU requires a page table to be set.")
		// c.pageTable = vm.NewPageTable(b.log2PageSize)
	}
}

func (b Builder) createPorts(name string, c *Comp) {
	c.topPort = sim.NewPort(c,
		4096, 4096,
		name+".ToTop")
	c.AddPort("Top", c.topPort)

	c.bottomPort = sim.NewPort(c,
		4096, 4096,
		name+".BottomPort")
	c.AddPort("Bottom", c.bottomPort)

	c.controlPort = sim.NewPort(c,
		4096, 4096,
		name+".ControlPort")
	c.AddPort("Control", c.controlPort)

	// sbin_claude_avatar: out-of-band walk-cancel ingress (Avatar EAF).
	c.cancelPort = sim.NewPort(c,
		4096, 4096,
		name+".CancelPort")
	c.AddPort("Cancel", c.cancelPort)

	if b.uvmServiceProvider != "" {
		c.uvmPort = sim.NewPort(c, 4096, 4096, name+".UVMPort")
		c.AddPort("UVM", c.uvmPort)

		// sbin_codex: master port used to fan a UVM range invalidation out to
		// every TLB level.
		c.tlbCtrlPort = sim.NewPort(c, 4096, 4096, name+".TLBCtrlPort")
		c.AddPort("TLBCtrl", c.tlbCtrlPort)
	}
}
