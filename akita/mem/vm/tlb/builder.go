package tlb

import (
	"fmt" // sbin_claude_vc

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm" // sbin_codex
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

// A Builder can build TLBs
type Builder struct {
	engine                 sim.Engine
	freq                   sim.Freq
	numReqPerCycle         int
	numTopChannels         int // sbin_claude_vc
	numSets                int
	numWays                int
	log2PageSize           uint64
	pageSize               uint64
	numMSHREntry           int
	state                  int
	latency                int
	addressMapper          mem.AddressToPortMapper
	addressMapperType      string
	remotePorts            []sim.RemotePort
	pageAdmissionPredicate func(vm.Page) bool // sbin_codex
	compressedMSHR         bool               // sbin_claude_latpc
}

// MakeBuilder returns a Builder
func MakeBuilder() Builder {
	return Builder{
		freq:           1 * sim.GHz,
		numReqPerCycle: 4,
		numTopChannels: 1, // sbin_claude_vc: one top-side request class by default.
		numSets:        1,
		numWays:        32,
		pageSize:       4096,
		numMSHREntry:   4,
		state:          tlbStateEnable,
		// latency: 4, // sbin_codex: pre-edit default.
		latency:                4,
		pageAdmissionPredicate: func(vm.Page) bool { return true }, // sbin_codex: admit-all default.
	}
}

// WithEngine sets the engine that the TLBs to use
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the freq the TLBs use
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithNumSets sets the number of sets in a TLB. Use 1 for fully associated
// TLBs.
func (b Builder) WithNumSets(n int) Builder {
	b.numSets = n
	return b
}

// WithNumWays sets the number of ways in a TLB. Set this field to the number
// of TLB entries for all the functions.
func (b Builder) WithNumWays(n int) Builder {
	b.numWays = n
	return b
}

// WithLog2PageSize sets the page size as a power of 2
func (b Builder) WithLog2PageSize(n uint64) Builder {
	b.log2PageSize = n
	return b
}

// WithPageSize sets the page size that the TLB works with.
//
// Deprecated: Use `WithLog2PageSize` instead.
func (b Builder) WithPageSize(n uint64) Builder {
	// Check if n is a power of 2 by counting the number of 1s in binary
	if n == 0 || (n&(n-1)) != 0 {
		panic("page size must be a power of 2")
	}

	log2 := 0
	temp := n

	for temp > 0 {
		temp >>= 1
		log2++
	}

	b.log2PageSize = uint64(log2 - 1) // Subtract 1 because we count one extra iteration
	b.pageSize = 1 << b.log2PageSize

	return b
}

// WithNumReqPerCycle sets the number of requests per cycle can be processed by
// a TLB
func (b Builder) WithNumReqPerCycle(n int) Builder {
	b.numReqPerCycle = n
	return b
}

// WithNumTopChannels sets how many independent top-side request classes the
// TLB exposes. Each channel gets its own port ("Top" for channel 0, "Top[i]"
// for the rest), its own lookup pipeline and its own response queue, so a
// back-pressured client of one channel cannot hold up the answers of another.
//
// Use more than one channel when client classes with different drain paths
// share the TLB - for example demand translations from the L1 TLBs and
// fill/writeback translations from a memory-side address translator. Sharing
// one port between such classes deadlocks: the port queue is in-order, so a
// blocked client of one class blocks every answer queued behind it, including
// the answers the other class needs in order to unblock.
//
// The default is 1, which keeps the single-channel behaviour unchanged.
// sbin_claude_vc
func (b Builder) WithNumTopChannels(n int) Builder {
	if n < 1 {
		panic("a TLB needs at least one top channel")
	}

	b.numTopChannels = n

	return b
}

// WithLowModule sets the port that can provide the address translation in case
// of tlb miss.
//
// Deprecated: Use `WithTranslationProviderMapper` instead.
func (b Builder) WithLowModule(lowModule sim.RemotePort) Builder {
	b.addressMapper = &mem.SinglePortMapper{
		Port: lowModule,
	}
	return b
}

// WithNumMSHREntry sets the number of mshr entry
func (b Builder) WithNumMSHREntry(num int) Builder {
	b.numMSHREntry = num
	return b
}

// WithCompressedMSHR selects LATPC's LATC compressed MSHR (MICRO'25 §5.3):
// one MSHR entry tracks up to 32 outstanding misses of one
// Regularity-Detector group. The entry count set by WithNumMSHREntry then
// counts group entries. Off by default. // sbin_claude_latpc
func (b Builder) WithCompressedMSHR(enabled bool) Builder {
	b.compressedMSHR = enabled
	return b
}

// WithLatency sets the latency of the TLB lookup. The latency is counted in
// both hit and miss cases.
func (b Builder) WithLatency(cycles int) Builder {
	b.latency = cycles
	return b
}

// WithPageAdmissionPredicate controls which translated pages are stored while
// preserving responses to requests already waiting in the MSHR. // sbin_codex
func (b Builder) WithPageAdmissionPredicate(predicate func(vm.Page) bool) Builder {
	b.pageAdmissionPredicate = predicate
	return b
}

// WithTranslationProviderMapper sets the mapper that can find the remote port
// that can provide the translation service according to the virtual address.
func (b Builder) WithTranslationProviderMapper(
	mapper mem.AddressToPortMapper,
) Builder {
	b.addressMapper = mapper
	return b
}

// WithTranslationProviderMapperType sets the type of the translation provider
// mapper. The mapper can find the remote port that can provide the translation
// service according to the virtual address. The type can be "single" or
// "interleaved".
func (b Builder) WithTranslationProviderMapperType(t string) Builder {
	b.addressMapperType = t
	return b
}

// WithTranslationProviders registers the remote ports that handle address
// translation requests.
//
// Use together with `WithTranslationProviderMapperType` to control request
// distribution:
//   - "single": exactly one port must be provided.
//   - "interleaved": the number of ports must be a power of two; requests are
//     interleaved at page granularity (4 KiB by default).
func (b Builder) WithTranslationProviders(ports ...sim.RemotePort) Builder {
	b.remotePorts = ports
	return b
}

// Build creates a new TLB
func (b Builder) Build(name string) *Comp {
	tlb := &Comp{}
	tlb.TickingComponent = sim.NewTickingComponent(name, b.engine, b.freq, tlb)

	tlb.numSets = b.numSets
	tlb.numWays = b.numWays
	tlb.numReqPerCycle = b.numReqPerCycle
	tlb.pageSize = b.pageSize
	tlb.addressMapper = b.addressMapper
	// tlb.mshr = newMSHR(b.numMSHREntry) // sbin_claude_latpc: pre-edit unconditional classic MSHR.
	if b.compressedMSHR { // sbin_claude_latpc
		tlb.mshr = newLATCMSHR(b.numMSHREntry, b.pageSize)
	} else {
		tlb.mshr = newMSHR(b.numMSHREntry)
	}
	// tlb.state = b.state // sbin_codex: pre-edit assignment.
	tlb.state = b.state
	tlb.pageAdmissionPredicate = b.pageAdmissionPredicate // sbin_codex
	tlb.pendingCancels = make(map[string]struct{})        // sbin_claude_avatar

	b.createPorts(name, tlb)
	b.createTranslationProviderMapper(tlb)

	tlb.reset()

	// Pre-edit code (commented per project convention). A single lookup
	// pipeline was built here, shared by every top-side requester:
	//
	// buf := sim.NewBuffer(name+".ResponsePipelineBuf", 16)
	// tlb.responseBuffer = buf
	// tlb.responsePipeline = pipelining.MakeBuilder().
	// 	WithNumStage(b.latency).
	// 	WithCyclePerStage(1).
	// 	WithPipelineWidth(tlb.numReqPerCycle).
	// 	WithPostPipelineBuffer(buf).
	// 	Build(name + ".ResponsePipeline")
	//
	// sbin_claude_vc: each top channel gets its own pipeline, so a channel
	// that cannot send its answers does not hold the other channels' lookups
	// at the head of a shared buffer.
	b.createPipelines(name, tlb)

	ctrlMiddleware := &ctrlMiddleware{Comp: tlb}
	tlb.AddMiddleware(ctrlMiddleware)

	middleware := &tlbMiddleware{Comp: tlb}
	tlb.AddMiddleware(middleware)

	return tlb
}

func (b Builder) createTranslationProviderMapper(c *Comp) {
	if c.addressMapper != nil {
		return
	}

	switch b.addressMapperType {
	case "single":
		if len(b.remotePorts) != 1 {
			panic("single address mapper requires exactly 1 port")
		}
		c.addressMapper = &mem.SinglePortMapper{
			Port: b.remotePorts[0],
		}
	case "interleaved":
		if len(b.remotePorts) == 0 {
			panic("interleaved address mapper requires at least 1 port")
		}
		mapper := mem.NewInterleavedAddressPortMapper(1 << b.log2PageSize)
		mapper.LowModules = append(mapper.LowModules, b.remotePorts...)
		c.addressMapper = mapper
	default:
		panic("invalid address mapper type: " + b.addressMapperType)
	}
}

func (b Builder) createPorts(name string, c *Comp) {
	// Pre-edit code (commented per project convention). There was exactly one
	// top port:
	//
	// c.topPort = sim.NewPort(c,
	// 	b.numReqPerCycle, b.numReqPerCycle,
	// 	name+".TopPort")
	// c.AddPort("Top", c.topPort)
	//
	// sbin_claude_vc: channel 0 keeps the original port key and name, so
	// single-channel TLBs are wired exactly as before.
	for i := 0; i < b.numTopChannels; i++ {
		portKey, portName := topChannelNames(name, i)
		port := sim.NewPort(c,
			b.numReqPerCycle, b.numReqPerCycle,
			portName)
		c.AddPort(portKey, port)
		c.topChannels = append(c.topChannels, &topChannel{port: port})
	}

	c.topPort = c.topChannels[0].port

	c.bottomPort = sim.NewPort(c,
		b.numReqPerCycle, b.numReqPerCycle,
		name+".BottomPort")
	c.AddPort("Bottom", c.bottomPort)

	c.controlPort = sim.NewPort(c, 1, 1,
		name+".ControlPort")
	c.AddPort("Control", c.controlPort)

	// sbin_claude_avatar: out-of-band cancel ingress (Avatar EAF). Inert
	// unless a speculation unit is wired to it.
	c.cancelPort = sim.NewPort(c,
		b.numReqPerCycle, b.numReqPerCycle,
		name+".CancelPort")
	c.AddPort("Cancel", c.cancelPort)
}

// topChannelNames returns the port key and the port name of one top channel.
// Channel 0 keeps the historical "Top"/".TopPort" naming. // sbin_claude_vc
func topChannelNames(compName string, channelID int) (portKey, portName string) {
	if channelID == 0 {
		return "Top", compName + ".TopPort"
	}

	return fmt.Sprintf("Top[%d]", channelID),
		fmt.Sprintf("%s.TopPort[%d]", compName, channelID)
}

// createPipelines builds one lookup pipeline per top channel. // sbin_claude_vc
func (b Builder) createPipelines(name string, c *Comp) {
	for i, channel := range c.topChannels {
		bufName, pipelineName := name+".ResponsePipelineBuf",
			name+".ResponsePipeline"
		if i > 0 {
			bufName = fmt.Sprintf("%s.ResponsePipelineBuf[%d]", name, i)
			pipelineName = fmt.Sprintf("%s.ResponsePipeline[%d]", name, i)
		}

		buf := sim.NewBuffer(bufName, 16)
		channel.buffer = buf
		channel.pipeline = pipelining.MakeBuilder().
			WithNumStage(b.latency).
			WithCyclePerStage(1).
			WithPipelineWidth(c.numReqPerCycle).
			WithPostPipelineBuffer(buf).
			Build(pipelineName)
	}

	c.responseBuffer = c.topChannels[0].buffer
	c.responsePipeline = c.topChannels[0].pipeline
}
