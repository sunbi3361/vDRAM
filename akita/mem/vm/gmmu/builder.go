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
}

// MakeBuilder creates a new builder
func MakeBuilder() Builder {
	return Builder{
		freq:               1 * sim.GHz,
		log2PageSize:       12,
		maxNumReqInFlight:  16,
		pageWalkingLatency: 100, // sbin_codex: latency of each uncached page-table level.
		hptAccessesPerWalk: 1,   // sbin_claude_hpt: ideal HPT resolves in one access.
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
	c.canceledReqs = make(map[string]struct{})    // sbin_claude_avatar
	c.addressToPortMapper = b.addressToPortMapper // sbin_gmmu
	c.UVMServiceProvider = b.uvmServiceProvider   // sbin_codex
	// c.accessCounterThreshold = b.accessCounterThresh // sbin_codex
	// c.accessCounters = make(map[uint64]uint64)
	// c.accessCounterNotified = make(map[uint64]bool)
}

func (b Builder) createPageWalkCache(name string, c *Comp) {
	pageWalkCacheBuilder := pagewalkcache.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq). // sbin_codex: keep cache and GMMU in the same clock domain.
		WithLog2PageSize(b.log2PageSize).
		WithNumLevels(pageTableLevels). // sbin_codex
		WithNumBlocks(128).             // sbin_codex
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
