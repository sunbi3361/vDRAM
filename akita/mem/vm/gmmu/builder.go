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
}

// MakeBuilder creates a new builder
func MakeBuilder() Builder {
	return Builder{
		freq:               1 * sim.GHz,
		log2PageSize:       12,
		maxNumReqInFlight:  16,
		pageWalkingLatency: 100, // sbin_codex: latency of each uncached page-table level.
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

// func (b Builder) WithAccessCounterThreshold(thresh uint64) Builder { // sbin_codex
// 	b.accessCounterThresh = thresh
// 	return b
// }

func (b Builder) Build(name string) *Comp {
	// sbin_codex: reject a latency that cannot represent a cycle countdown.
	if b.pageWalkingLatency < 0 {
		panic("GMMU page-walking latency must not be negative")
	}

	gmmu := new(Comp)
	gmmu.TickingComponent = *sim.NewTickingComponent(
		name, b.engine, b.freq, gmmu)

	b.createPorts(name, gmmu)
	b.createPageTable(gmmu)
	b.createPageWalkCache(name, gmmu)
	b.configureInternalStates(gmmu)

	ctrlMiddleware := &ctrlMiddleware{Comp: gmmu}
	gmmu.AddMiddleware(ctrlMiddleware)

	middleware := &middleware{Comp: gmmu} // sbin_gmmu
	gmmu.AddMiddleware(middleware)        // sbin_gmmu

	return gmmu
}

func (b Builder) configureInternalStates(c *Comp) {
	c.maxRequestsInFlight = b.maxNumReqInFlight
	c.latency = b.pageWalkingLatency
	c.deviceID = b.deviceID
	c.memAddrOffset = b.memAddrOffset
	c.memoryPerChiplet = b.memoryPerChiplet       // sbin_codex
	c.log2PageSize = b.log2PageSize               // sbin_codex
	c.state = gmmuStateEnable                     // sbin_codex
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

	if b.uvmServiceProvider != "" {
		c.uvmPort = sim.NewPort(c, 4096, 4096, name+".UVMPort")
		c.AddPort("UVM", c.uvmPort)
	}
}
