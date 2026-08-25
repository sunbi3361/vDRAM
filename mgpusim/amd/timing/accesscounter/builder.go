package accesscounter

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// Builder configures a GPU-side UVM access counter. // sbin_codex
type Builder struct {
	engine            sim.Engine
	freq              sim.Freq
	deviceID          uint64
	threshold         uint64
	bufferSize        int
	numReqPerCycle    int
	bottomDestination sim.RemotePort
	ctrlDestination   sim.RemotePort
}

// MakeBuilder creates a builder with timing-component defaults.
func MakeBuilder() Builder {
	return Builder{
		freq:           sim.GHz,
		threshold:      8,
		bufferSize:     4096,
		numReqPerCycle: 32,
	}
}

// WithEngine sets the simulation engine.
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the component frequency.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithDeviceID sets the GPU this counter belongs to.
func (b Builder) WithDeviceID(deviceID uint64) Builder {
	b.deviceID = deviceID
	return b
}

// WithThreshold sets the number of remote demands that triggers a
// notification.
func (b Builder) WithThreshold(threshold uint64) Builder {
	b.threshold = threshold
	return b
}

// WithBufferSize sets each port's incoming and outgoing capacity.
func (b Builder) WithBufferSize(size int) Builder {
	b.bufferSize = size
	return b
}

// WithNumReqPerCycle sets how many remote requests may be forwarded per cycle.
// A GPU aggregates the remote traffic of every CU here, so this must be wide
// enough not to become an artificial bottleneck. // sbin_codex
func (b Builder) WithNumReqPerCycle(n int) Builder {
	b.numReqPerCycle = n
	return b
}

// WithBottomDestination sets the remote-memory destination.
func (b Builder) WithBottomDestination(dst sim.RemotePort) Builder {
	b.bottomDestination = dst
	return b
}

// WithCtrlDestination sets the UVM control endpoint of the GPU.
func (b Builder) WithCtrlDestination(dst sim.RemotePort) Builder {
	b.ctrlDestination = dst
	return b
}

// Build creates an access counter.
func (b Builder) Build(name string) *Comp {
	c := &Comp{
		deviceID:          b.deviceID,
		threshold:         b.threshold,
		numReqPerCycle:    b.numReqPerCycle,
		bottomDestination: b.bottomDestination,
		ctrlDestination:   b.ctrlDestination,
		counters:          make(map[RegionKey]*counterState),
		transactions:      make(map[string]transaction),
		stalledWrites:     make(map[RegionKey][]*mem.WriteReq),
	}
	c.TickingComponent = sim.NewTickingComponent(name, b.engine, b.freq, c)

	c.Top = sim.NewPort(c, b.bufferSize, b.bufferSize, name+".Top")
	c.Bottom = sim.NewPort(c, b.bufferSize, b.bufferSize, name+".Bottom")
	c.Ctrl = sim.NewPort(c, b.bufferSize, b.bufferSize, name+".Ctrl")

	c.AddPort("Top", c.Top)
	c.AddPort("Bottom", c.Bottom)
	c.AddPort("Ctrl", c.Ctrl)

	return c
}
