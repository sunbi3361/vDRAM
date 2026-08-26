// sbin_claude_utopia
package rsw

import (
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
)

// metadata line geometry: TAR entries are ~4B and SF counters ~1B, so one
// 64B memory line covers 16 TAR entries or 64 SF counters.
const (
	metaLineBytes = 64
	// Pre-edit code (commented per project convention):
	// tarEntriesPerLine  = 16
	// defaultCacheBytes  = 2048 // 2KB TAR and SF caches (utopia.md 4.6)
	//
	// sbin_claude_utopia: one TAR entry is one way's {valid, PID, VPN tag}
	// (~4B: 23-bit tag for a 512MB/16-way RestSeg plus valid/PID bits), so a
	// 64B line holds lineBytes/(assoc*4B) sets — exactly one set at the
	// default 16-way associativity. The old constant packed 16 sets per line
	// (2 bits per way), overstating the TAR cache reach ~16x. The per-set
	// packing is derived from the segment associativity at first use
	// (Comp.segmentConfigs), since the driver registers the RestSeg after
	// the GPU is built.
	tarEntryBytes     = 4
	sfCountersPerLine = 64
	metaCacheAssoc    = 8
	// sbin_claude_utopia: the default TAR cache matches the baseline GMMU
	// page-walk-cache storage budget (128 entries x 4 cached levels x 8B =
	// 4KB) so TAR-vs-PWC comparisons are iso-capacity; the SF cache keeps
	// the paper's 2KB (utopia.md 4.6).
	defaultTARCacheBytes = 4096
	defaultSFCacheBytes  = 2048
	defaultHitLatency    = 2
	defaultMissLatency   = 100 // one modeled memory access, like the GMMU walk
	defaultMaxInFlight   = 16
)

// A Builder can build UTU (RestSeg walker) components.
type Builder struct {
	engine   sim.Engine
	freq     sim.Freq
	deviceID uint64
	registry *restseg.Registry

	tarCacheBytes  uint64
	sfCacheBytes   uint64
	hitLatency     int
	missLatency    int
	maxReqInFlight int
}

// MakeBuilder creates a Builder with the default UTU parameters.
func MakeBuilder() Builder {
	return Builder{
		freq: 1 * sim.GHz,
		// Pre-edit code (commented per project convention):
		// tarCacheBytes:  defaultCacheBytes,
		// sfCacheBytes:   defaultCacheBytes,
		//
		// sbin_claude_utopia: TAR default sized to the baseline GMMU PWC.
		tarCacheBytes:  defaultTARCacheBytes,
		sfCacheBytes:   defaultSFCacheBytes,
		hitLatency:     defaultHitLatency,
		missLatency:    defaultMissLatency,
		maxReqInFlight: defaultMaxInFlight,
	}
}

// WithEngine sets the simulation engine.
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the clock frequency of the UTU.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithDeviceID sets the driver device ID of the owning GPU.
func (b Builder) WithDeviceID(deviceID uint64) Builder {
	b.deviceID = deviceID
	return b
}

// WithRegistry sets the shared authoritative RestSeg registry.
func (b Builder) WithRegistry(registry *restseg.Registry) Builder {
	b.registry = registry
	return b
}

// WithTARCacheBytes sets the TAR metadata cache capacity.
func (b Builder) WithTARCacheBytes(bytes uint64) Builder {
	if bytes > 0 {
		b.tarCacheBytes = bytes
	}
	return b
}

// WithSFCacheBytes sets the SF metadata cache capacity.
func (b Builder) WithSFCacheBytes(bytes uint64) Builder {
	if bytes > 0 {
		b.sfCacheBytes = bytes
	}
	return b
}

// WithHitLatency sets the TAR/SF cache hit latency in cycles.
func (b Builder) WithHitLatency(latency int) Builder {
	if latency > 0 {
		b.hitLatency = latency
	}
	return b
}

// WithMissLatency sets the memory-fetch latency charged when TAR/SF metadata
// misses its cache.
func (b Builder) WithMissLatency(latency int) Builder {
	if latency > 0 {
		b.missLatency = latency
	}
	return b
}

// WithMaxReqInFlight sets the number of concurrent RestSeg walks.
func (b Builder) WithMaxReqInFlight(n int) Builder {
	if n > 0 {
		b.maxReqInFlight = n
	}
	return b
}

// Build creates the UTU component.
func (b Builder) Build(name string) *Comp {
	if b.registry == nil {
		panic("UTU requires a RestSeg registry")
	}

	c := new(Comp)
	c.TickingComponent = *sim.NewTickingComponent(name, b.engine, b.freq, c)

	c.deviceID = b.deviceID
	c.registry = b.registry
	c.sfHitLatency = b.hitLatency
	c.tarHitLatency = b.hitLatency
	c.missLatency = b.missLatency
	c.maxReqInFlight = b.maxReqInFlight

	c.sfCache = newMetaCache(
		b.sfCacheBytes, metaLineBytes, metaCacheAssoc, sfCountersPerLine)
	// Pre-edit code (commented per project convention):
	// c.tarCache = newMetaCache(
	// 	b.tarCacheBytes, metaLineBytes, metaCacheAssoc, tarEntriesPerLine)
	//
	// sbin_claude_utopia: the TAR sets-per-line packing depends on the
	// RestSeg associativity, which the driver registers only after the GPU
	// is built. Start at the 1-set-per-line floor; segmentConfigs() derives
	// the exact packing before the first cache probe.
	c.tarCache = newMetaCache(
		b.tarCacheBytes, metaLineBytes, metaCacheAssoc, 1)

	c.topPort = sim.NewPort(c, 4096, 4096, name+".Top")
	c.AddPort("Top", c.topPort)
	c.bottomPort = sim.NewPort(c, 4096, 4096, name+".Bottom")
	c.AddPort("Bottom", c.bottomPort)

	c.AddMiddleware(&middleware{Comp: c})

	return c
}
