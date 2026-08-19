package pagewalkcache

import "github.com/sarchlab/akita/v4/sim"

// Builder constructs a GMMU-facing page-walk cache.
type Builder struct {
	engine              sim.Engine
	freq                sim.Freq
	numBlocks           int
	numLevels           int
	numReqPerCycle      int
	maxRequestsInFlight int
	pageSize            uint64
	log2PageSize        uint64
	bitsPerLevel        uint64
	latency             int
}

// MakeBuilder returns a builder with the v4 page-table defaults.
func MakeBuilder() Builder {
	return Builder{
		freq:                1 * sim.GHz,
		numBlocks:           1,
		numLevels:           5,
		numReqPerCycle:      4,
		maxRequestsInFlight: 4,
		pageSize:            4096,
		log2PageSize:        12,
		bitsPerLevel:        9,
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

// WithNumBlocks sets the number of fully-associative entries per cacheable
// page-table level. // sbin_codex
func (b Builder) WithNumBlocks(numBlocks int) Builder {
	b.numBlocks = numBlocks
	return b
}

// WithNumLevels sets the number of page-table levels. Level zero remains
// uncached. // sbin_codex
func (b Builder) WithNumLevels(numLevels int) Builder {
	b.numLevels = numLevels
	return b
}

// WithNumReqPerCycle sets the number of messages consumed per tick.
func (b Builder) WithNumReqPerCycle(numReqPerCycle int) Builder {
	b.numReqPerCycle = numReqPerCycle
	return b
}

// WithMaxNumReqInFlight limits lookup reads waiting for their internal read
// latency. A miss is removed from this set as soon as its miss response is
// emitted; it never waits for a fill.
func (b Builder) WithMaxNumReqInFlight(maxRequests int) Builder {
	b.maxRequestsInFlight = maxRequests
	return b
}

// WithPageSize sets the page size and derives its log2 value.
func (b Builder) WithPageSize(pageSize uint64) Builder {
	if pageSize == 0 || pageSize&(pageSize-1) != 0 {
		panic("page-walk cache page size must be a power of 2")
	}
	log2PageSize := uint64(0)
	for size := pageSize; size > 1; size >>= 1 {
		log2PageSize++
	}
	b.pageSize = pageSize
	b.log2PageSize = log2PageSize
	return b
}

// WithLog2PageSize sets the page-size exponent.
func (b Builder) WithLog2PageSize(log2PageSize uint64) Builder {
	b.log2PageSize = log2PageSize
	b.pageSize = uint64(1) << log2PageSize
	return b
}

// WithBitsPerLevel sets the number of virtual-page-number bits in one level.
func (b Builder) WithBitsPerLevel(bitsPerLevel uint64) Builder {
	b.bitsPerLevel = bitsPerLevel
	return b
}

// WithLatency sets the internal page-walk-cache read latency in cycles.
func (b Builder) WithLatency(latency int) Builder {
	b.latency = latency
	return b
}

// WithPageWalkingLatency is an alias matching the MMU builder vocabulary.
func (b Builder) WithPageWalkingLatency(latency int) Builder {
	return b.WithLatency(latency)
}

// Build creates a cache with one GMMU-facing Top port. LookupReq and FillReq
// share this bidirectional port; LookupRsp is emitted on the same port.
func (b Builder) Build(name string) *Comp {
	if b.engine == nil {
		panic("pagewalkcache: WithEngine is required")
	}
	if b.numBlocks <= 0 || b.numLevels <= 0 || b.numReqPerCycle <= 0 {
		panic("pagewalkcache: cache dimensions must be greater than zero")
	}
	if b.maxRequestsInFlight <= 0 {
		panic("pagewalkcache: max requests in flight must be greater than zero")
	}
	if b.latency < 0 {
		panic("pagewalkcache: latency must not be negative")
	}
	if b.bitsPerLevel == 0 || b.bitsPerLevel > 63 {
		panic("pagewalkcache: bits per level must be between 1 and 63")
	}
	if uint64(b.numLevels)*b.bitsPerLevel > 63 { // sbin_codex
		panic("pagewalkcache: page-table VPN must fit in 63 bits")
	}

	cache := &Comp{
		numBlocks:           b.numBlocks,
		numLevels:           b.numLevels,
		numReqPerCycle:      b.numReqPerCycle,
		maxRequestsInFlight: b.maxRequestsInFlight,
		pageSize:            b.pageSize,
		log2PageSize:        b.log2PageSize,
		bitsPerLevel:        b.bitsPerLevel,
		latency:             b.latency,
		sets:                initSets(b.numLevels, b.numBlocks),
	}
	cache.TickingComponent = *sim.NewTickingComponent(name, b.engine, b.freq, cache)
	cache.topPort = sim.NewPort(cache, b.numReqPerCycle, b.numReqPerCycle, name+".ToTop")
	cache.AddPort("Top", cache.topPort)
	cache.AddMiddleware(&middleware{Comp: cache})
	return cache
}
