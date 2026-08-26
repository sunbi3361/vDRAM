// sbin_claude_fbt
package fbt

import "github.com/sarchlab/akita/v4/sim"

const (
	// defaultNumEntries is the paper's modelled BT size: 16K entries, a
	// reach of 64MB at 4KB pages, chosen to cover a unique page for every
	// L2 cache entry (ASPLOS'18 section 4.3).
	defaultNumEntries = 16384
	// defaultNumWays keeps the same associativity as the shared L2 TLB.
	defaultNumWays = 16
	// defaultLookupLatency is the paper's 5-cycle FBT lookup. The 10-cycle
	// L2-to-FBT interconnect it also models is not represented here; the
	// L2 TLB above carries its own access latency.
	defaultLookupLatency = 5
	// defaultMaxReqInFlight matches the L2 TLB's MSHR count so the table
	// does not throttle the misses handed to it.
	defaultMaxReqInFlight = 64
	defaultLog2PageSize   = 12
)

// A Builder can build FBT components.
type Builder struct {
	engine sim.Engine
	freq   sim.Freq

	numEntries     int
	numWays        int
	log2PageSize   uint64
	lookupLatency  int
	maxReqInFlight int
}

// MakeBuilder creates a Builder with the paper's FBT parameters.
func MakeBuilder() Builder {
	return Builder{
		freq:           1 * sim.GHz,
		numEntries:     defaultNumEntries,
		numWays:        defaultNumWays,
		log2PageSize:   defaultLog2PageSize,
		lookupLatency:  defaultLookupLatency,
		maxReqInFlight: defaultMaxReqInFlight,
	}
}

// WithEngine sets the simulation engine.
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the clock frequency.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithNumEntries sets how many page mappings the table holds.
func (b Builder) WithNumEntries(n int) Builder {
	if n > 0 {
		b.numEntries = n
	}

	return b
}

// WithNumWays sets the associativity.
func (b Builder) WithNumWays(n int) Builder {
	if n > 0 {
		b.numWays = n
	}

	return b
}

// WithLog2PageSize sets the page size as a power of two.
func (b Builder) WithLog2PageSize(n uint64) Builder {
	if n > 0 {
		b.log2PageSize = n
	}

	return b
}

// WithLookupLatency sets the table access latency in cycles.
func (b Builder) WithLookupLatency(cycles int) Builder {
	if cycles >= 0 {
		b.lookupLatency = cycles
	}

	return b
}

// WithMaxReqInFlight sets the number of concurrent lookups.
func (b Builder) WithMaxReqInFlight(n int) Builder {
	if n > 0 {
		b.maxReqInFlight = n
	}

	return b
}

// Build creates the FBT component.
func (b Builder) Build(name string) *Comp {
	numWays := b.numWays
	if numWays > b.numEntries {
		numWays = b.numEntries
	}

	numSets := b.numEntries / numWays
	if numSets == 0 {
		numSets = 1
	}

	c := new(Comp)
	c.TickingComponent = *sim.NewTickingComponent(name, b.engine, b.freq, c)

	c.log2PageSize = b.log2PageSize
	c.lookupLatency = b.lookupLatency
	c.maxReqInFlight = b.maxReqInFlight
	c.table = newTable(numSets, numWays)

	c.topPort = sim.NewPort(c, b.maxReqInFlight, b.maxReqInFlight, name+".Top")
	c.AddPort("Top", c.topPort)
	c.bottomPort = sim.NewPort(c,
		b.maxReqInFlight, b.maxReqInFlight, name+".Bottom")
	c.AddPort("Bottom", c.bottomPort)

	c.AddMiddleware(&middleware{Comp: c})

	return c
}
