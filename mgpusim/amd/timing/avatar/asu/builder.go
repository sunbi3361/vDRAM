// sbin_claude_avatar
package asu

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

const (
	// defaultValidationLatency models the speculative sector fetch through
	// the data hierarchy plus the CAVA metadata check (avatar-plan.md 1.5).
	defaultValidationLatency = 200
	// defaultModNumEntries and defaultConfidenceThreshold follow the paper
	// (refs/avatar.md 5.2, 5.3).
	defaultModNumEntries       = 32
	defaultConfidenceThreshold = 2
	// defaultNumReqPerCycle matches the L2 TLB top throughput so the
	// interposer does not throttle the baseline translation path.
	defaultNumReqPerCycle = 1024
	defaultMaxInFlight    = 4096
)

// A Builder can build Avatar Speculation Unit components.
type Builder struct {
	engine       sim.Engine
	freq         sim.Freq
	deviceID     uint64
	log2PageSize uint64
	registry     *meta.Registry
	pageTable    vm.PageTable

	validationLatency   int
	modNumEntries       int
	confidenceThreshold int
	maxReqInFlight      int
	numReqPerCycle      int
}

// MakeBuilder creates a Builder with the default ASU parameters.
func MakeBuilder() Builder {
	return Builder{
		freq:                1 * sim.GHz,
		log2PageSize:        12,
		validationLatency:   defaultValidationLatency,
		modNumEntries:       defaultModNumEntries,
		confidenceThreshold: defaultConfidenceThreshold,
		maxReqInFlight:      defaultMaxInFlight,
		numReqPerCycle:      defaultNumReqPerCycle,
	}
}

// WithEngine sets the simulation engine.
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the clock frequency of the ASU.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithDeviceID sets the driver device ID of the owning GPU.
func (b Builder) WithDeviceID(deviceID uint64) Builder {
	b.deviceID = deviceID
	return b
}

// WithLog2PageSize sets the log2 page size.
func (b Builder) WithLog2PageSize(size uint64) Builder {
	b.log2PageSize = size
	return b
}

// WithRegistry sets the shared authoritative Avatar metadata registry.
func (b Builder) WithRegistry(registry *meta.Registry) Builder {
	b.registry = registry
	return b
}

// WithPageTable sets the GPU page table used to cross-check a validated
// speculation before it becomes architecturally visible.
func (b Builder) WithPageTable(pageTable vm.PageTable) Builder {
	b.pageTable = pageTable
	return b
}

// WithValidationLatency sets the modeled speculative-fetch-plus-CAVA
// latency in cycles.
func (b Builder) WithValidationLatency(latency int) Builder {
	if latency > 0 {
		b.validationLatency = latency
	}
	return b
}

// WithModNumEntries sets the per-requester MOD table size.
func (b Builder) WithModNumEntries(n int) Builder {
	if n > 0 {
		b.modNumEntries = n
	}
	return b
}

// WithConfidenceThreshold sets the MOD confidence needed to speculate.
func (b Builder) WithConfidenceThreshold(threshold int) Builder {
	if threshold > 0 {
		b.confidenceThreshold = threshold
	}
	return b
}

// WithMaxReqInFlight sets the number of concurrent tracked misses.
func (b Builder) WithMaxReqInFlight(n int) Builder {
	if n > 0 {
		b.maxReqInFlight = n
	}
	return b
}

// Build creates the ASU component.
func (b Builder) Build(name string) *Comp {
	if b.registry == nil {
		panic("ASU requires an avatar metadata registry")
	}
	if b.pageTable == nil {
		panic("ASU requires the GPU page table")
	}

	c := new(Comp)
	c.TickingComponent = *sim.NewTickingComponent(name, b.engine, b.freq, c)

	c.deviceID = b.deviceID
	c.log2PageSize = b.log2PageSize
	c.registry = b.registry
	c.pageTable = b.pageTable
	c.validationLatency = b.validationLatency
	c.modNumEntries = b.modNumEntries
	c.confidenceThreshold = b.confidenceThreshold
	c.maxReqInFlight = b.maxReqInFlight
	c.numReqPerCycle = b.numReqPerCycle
	c.mods = make(map[sim.RemotePort]*modTable)

	c.topPort = sim.NewPort(c, 4096, 4096, name+".Top")
	c.AddPort("Top", c.topPort)
	c.bottomPort = sim.NewPort(c, 4096, 4096, name+".Bottom")
	c.AddPort("Bottom", c.bottomPort)

	c.AddMiddleware(&middleware{Comp: c})

	return c
}
