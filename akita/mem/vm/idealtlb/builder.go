package idealtlb

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// Builder can build ideal TLBs.
// sbin_codex: kept as a value receiver builder to match tlb.Builder style.
type Builder struct {
	engine         sim.Engine
	freq           sim.Freq
	numReqPerCycle int
	latency        int
	state          int
	pageTable      vm.PageTable

	// sbin_codex: field exists for API parity with tlb.Builder; ideal TLB
	// never forwards misses to Bottom.
	addressMapper mem.AddressToPortMapper
}

// MakeBuilder returns a Builder with sensible defaults.
func MakeBuilder() Builder {
	return Builder{
		freq:           1 * sim.GHz,
		numReqPerCycle: 4,
		latency:        0,
		state:          tlbStateEnable,
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

// WithNumReqPerCycle sets the number of translation requests processed per
// cycle.
func (b Builder) WithNumReqPerCycle(n int) Builder {
	b.numReqPerCycle = n
	return b
}

// WithLatency sets the lookup latency in cycles. A value of 0 means the
// response is sent on the same tick.
func (b Builder) WithLatency(cycles int) Builder {
	b.latency = cycles
	return b
}

// WithPageTable sets the page table used for address resolution.
func (b Builder) WithPageTable(pageTable vm.PageTable) Builder {
	b.pageTable = pageTable
	return b
}

// WithTranslationProviderMapper accepts the provider mapper for API parity.
// sbin_codex: the ideal TLB discards this value; it never forwards misses.
func (b Builder) WithTranslationProviderMapper(
	mapper mem.AddressToPortMapper,
) Builder {
	b.addressMapper = mapper
	return b
}

// Build creates a new ideal TLB component.
func (b Builder) Build(name string) *Comp {
	c := &Comp{}
	c.TickingComponent = sim.NewTickingComponent(name, b.engine, b.freq, c)

	c.pageTable = b.pageTable
	c.numReqPerCycle = b.numReqPerCycle
	c.latency = b.latency
	c.state = b.state

	c.topPort = sim.NewPort(c,
		b.numReqPerCycle, b.numReqPerCycle,
		name+".TopPort")
	c.AddPort("Top", c.topPort)

	c.bottomPort = sim.NewPort(c,
		b.numReqPerCycle, b.numReqPerCycle,
		name+".BottomPort")
	c.AddPort("Bottom", c.bottomPort)

	c.controlPort = sim.NewPort(c, 1, 1,
		name+".ControlPort")
	c.AddPort("Control", c.controlPort)

	// sbin_codex: install middleware (todo 3).
	c.AddMiddleware(&ctrlMiddleware{Comp: c})
	c.AddMiddleware(&transMiddleware{Comp: c})

	return c
}
