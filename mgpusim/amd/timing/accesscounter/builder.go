// Package accesscounter provides the PCIe-visible CPU-memory proxy and UVM
// remote access counter. // sbin_codex
package accesscounter

import "github.com/sarchlab/akita/v4/sim"

// Builder configures an access-counter proxy. // sbin_codex
type Builder struct {
	engine            sim.Engine
	freq              sim.Freq
	threshold         uint64
	bufferSize        int
	bottomDestination sim.RemotePort
	driverDestination sim.RemotePort
}

// MakeBuilder creates a builder with timing-component defaults. // sbin_codex
func MakeBuilder() Builder {
	return Builder{
		freq:       sim.GHz,
		threshold:  1,
		bufferSize: 128,
	}
}

// WithEngine sets the simulation engine. // sbin_codex
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the component frequency. // sbin_codex
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithThreshold sets the number of completed remote demands that triggers a
// notification. // sbin_codex
func (b Builder) WithThreshold(threshold uint64) Builder {
	b.threshold = threshold
	return b
}

// WithBufferSize sets each port's incoming and outgoing capacity. // sbin_codex
func (b Builder) WithBufferSize(size int) Builder {
	b.bufferSize = size
	return b
}

// WithBottomDestination sets the CPU-memory destination. // sbin_codex
func (b Builder) WithBottomDestination(dst sim.RemotePort) Builder {
	b.bottomDestination = dst
	return b
}

// WithDriverDestination sets the access-counter notification destination.
// sbin_codex
func (b Builder) WithDriverDestination(dst sim.RemotePort) Builder {
	b.driverDestination = dst
	return b
}

// Build creates an access-counter proxy. // sbin_codex
func (b Builder) Build(name string) *Comp {
	c := &Comp{
		threshold:         b.threshold,
		bottomDestination: b.bottomDestination,
		driverDestination: b.driverDestination,
		counters:          make(map[RegionKey]*counterState),
		transactions:      make(map[string]transaction),
	}
	c.TickingComponent = sim.NewTickingComponent(name, b.engine, b.freq, c)
	c.Top = sim.NewPort(c, b.bufferSize, b.bufferSize, name+".Top")
	c.Bottom = sim.NewPort(c, b.bufferSize, b.bufferSize, name+".Bottom")
	c.AddPort("Top", c.Top)
	c.AddPort("Bottom", c.Bottom)
	return c
}
