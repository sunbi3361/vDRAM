package gmmu

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type transactionState int // sbin_gmmu

const ( // sbin_codex: states for the typed page-walk-cache latency model.
	newTransaction transactionState = iota
	sentToPageWalkCache
	pageWalkCacheDone
	fillingPageWalkCache
	pageWalkComplete
	transactionFinished
)

// sbin_codex: GMMU and its page-walk cache model a five-level page table.
const pageTableLevels = 5

const lowestPageWalkCacheLevel = 1 // sbin_codex: level zero is never cached.

// sbin_codex: control states are local to GMMU rather than borrowed from TLB.
type controlState int

const (
	gmmuStateEnable controlState = iota
	gmmuStatePause
	gmmuStateDrain
	gmmuStateFlush
)

type transaction struct {
	req       *vm.TranslationReq
	page      vm.Page
	cycleLeft uint64 // sbin_codex: remaining modeled page-walk cycles.

	level     int // sbin_codex: first uncached level still walked by GMMU.
	fillLevel int // sbin_codex: next cacheable level to install.
	msgID     string
	state     transactionState

	// sbin_codex: UVM gate admission and replay linkage (todo 8 of
	// mgpusim-uvm-manager). sequence is the monotonic ingress sequence the
	// gate assigned at admission; disposition records how the request was
	// resolved relative to a block barrier; faultRecord and replay link the
	// walk to the GMMU-owned replay record and replay command.
	sequence    uint64
	disposition disposition
	faultRecord *faultRecord
	replay      *replayCommand
}

// sbin_codex: disposition classifies how an admitted translation request was
// resolved relative to a block barrier (todo 8 of mgpusim-uvm-manager).
type disposition int

const (
	dispositionNone disposition = iota
	// dispositionDownstreamVisible marks a request whose response was sent to
	// the downstream translation hierarchy.
	dispositionDownstreamVisible
	// dispositionRetained marks a request retained in the GMMU replay records
	// or parked in a closed gate.
	dispositionRetained
	// dispositionRemoteCommitted marks an old remote read committed to the
	// CPU endpoint.
	dispositionRemoteCommitted
)

// sbin_codex: the GMMU translation gate assigns a monotonic ingress sequence
// to every admitted request and snapshots the local watermark when a block
// closes admission (todo 8 of mgpusim-uvm-manager).
type gateState struct {
	gateID               uint64
	lastAssignedSequence uint64
}

// sbin_codex: a faultRecord is a GMMU-owned replay record for a stalled
// managed translation request (todo 8 of mgpusim-uvm-manager). The GMMU owns
// the replay queue; the driver owns the 64 KB service transaction.
type faultRecord struct {
	token        vm.FaultPendingToken
	replayToken  vm.ReplayToken
	pid          vm.PID
	vAddr        uint64
	deviceID     uint64
	accessKind   vm.AccessKind
	waiterDelta  vm.WaiterDelta
	regionBase   uint64
	sequence     uint64
	req          *vm.TranslationReq
	notify       bool
	notified     bool
}

// sbin_codex: a parkedRequest is a translation request that arrived after a
// block closed admission. It retains its ingress sequence (above the
// watermark) until the unblock releases it.
type parkedRequest struct {
	sequence uint64
	req      *vm.TranslationReq
}

// sbin_codex: a blockCommand is an active BlockRange on the translation gate.
// pendingDisposals counts the matching admitted requests with
// sequence<=watermark that are still walking; the ack fires only when it
// reaches zero.
type blockCommand struct {
	commandID        uint64
	pid              vm.PID
	startVA          uint64
	size             uint64
	watermark        uint64
	src              sim.RemotePort
	pendingDisposals int
	acked            bool
	parked           []*parkedRequest
}

// sbin_codex: a replayCommand tracks a ReplayRange: the records matched to
// the serviced range, the re-injected walks still in flight, and the original
// command for the acknowledgement.
type replayCommand struct {
	req     *vm.ReplayRange
	pending []*faultRecord
	inFlight int
}

// sbin_codex: a tlbInvalidateCommand tracks a range TLB invalidation the GMMU
// coordinates: the original request for the completion response and the
// broadcast request IDs still awaiting acknowledgements (plan todo 14 of
// mgpusim-uvm-manager, uvm-manager.md §21.1).
type tlbInvalidateCommand struct {
	reqID   string
	src     sim.RemotePort
	pending map[string]bool
}

// sbin_codex: the GMMU is the single translation gate of the GPU. The CP
// pre-registers this gate ID for block/unblock commands (todo 8 of
// mgpusim-uvm-manager).
const TranslationGateID uint64 = 1

// sbin_codex: fault-service regions are 64 KB (uvm-manager.md §8.3).
const faultRegionSize = 64 * 1024

// Comp is the default gmmu implementation. It is also an akita Component.
type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder // sbin: v4

	deviceID uint64
	state    controlState // sbin_codex: initialized to enabled by Builder.

	topPort          sim.Port
	bottomPort       sim.Port
	controlPort      sim.Port // sbin_gmmu
	commandProcessor sim.Port // sbin_gmmu
	LowModule        sim.Port

	pageWalkCachePort sim.Port // sbin_gmmu
	pageWalkCache     sim.Port // sbin_gmmu

	addressToPortMapper mem.AddressToPortMapper // sbin_gmmu
	memAddrOffset       uint64
	memoryPerChiplet    uint64

	pageTable           vm.PageTable
	latency             int
	maxRequestsInFlight int
	log2PageSize        uint64

	walkingTranslations []transaction

	// sbin_codex: UVM fault ownership, replay records, and the translation
	// gate (todo 8 of mgpusim-uvm-manager).
	gate               gateState
	faultRecords       []*faultRecord
	nextFaultToken     vm.FaultPendingToken
	nextReplayToken    vm.ReplayToken
	pendingRegions     map[uint64]bool
	regionReplayTokens map[uint64]vm.ReplayToken
	activeBlocks       []*blockCommand
	activeReplay       *replayCommand
	pendingReplays     []*vm.ReplayRange

	// sbin_codex: UVM range TLB invalidation coordination (plan todo 14 of
	// mgpusim-uvm-manager). The GMMU is the invalidation coordinator: it
	// broadcasts the request to every topology-present TLB endpoint, collects
	// the acknowledgements, and returns one completion response
	// (uvm-manager.md §21.1).
	tlbEndpoints         []sim.Port
	activeTLBInvalidates map[string]*tlbInvalidateCommand
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// sbin_mcm
func (c *Comp) SetCommandProcessor(cp sim.Port) {
	c.commandProcessor = cp
}

// sbin_codex: SetTLBEndpoints registers the topology-present TLB endpoint set
// for range invalidation coordination (plan todo 14 of mgpusim-uvm-manager).
func (c *Comp) SetTLBEndpoints(endpoints []sim.Port) {
	c.tlbEndpoints = endpoints
}

// sbin_codex: TLBEndpoints returns the registered TLB endpoint set.
func (c *Comp) TLBEndpoints() []sim.Port {
	return c.tlbEndpoints
}
