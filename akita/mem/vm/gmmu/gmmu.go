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

	// Pre-edit field (commented per AGENTS.md convention). A parked fault used
	// to keep occupying a page-walk slot:
	// waitingOnUVM bool
	//
	// sbin_codex: parked faults now leave walkingTranslations entirely and live
	// in replayQueue until the driver reports the region replayable.
}

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

	// sbin_codex: UVM control port. The GMMU forwards managed-page faults to
	// the GPU Command Processor, which relays them to the host UVM driver over
	// PCIe. The same port carries the driver's range invalidation and fault
	// replay commands back.
	uvmPort            sim.Port
	UVMServiceProvider sim.RemotePort

	// sbin_codex: the GMMU is the range-invalidation coordinator (spec 21.1).
	// It broadcasts a 64KB invalidation to every TLB level that may cache the
	// mapping and reports one aggregated completion.
	tlbCtrlPort sim.Port
	TLBs        []sim.RemotePort

	pendingTLBInvalidate    *vm.UVMTLBInvalidateReq
	pendingTLBInvalidateACK int

	// sbin_codex: translations stalled on an unresolved UVM mapping. The GMMU
	// owns them (spec 22); the driver only owns 64KB service transactions.
	replayQueue []transaction

	// accessCounters         map[uint64]uint64 // sbin_codex
	// accessCounterNotified  map[uint64]bool   // sbin_codex
	// accessCounterThreshold uint64            // sbin_codex

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
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// sbin_mcm
func (c *Comp) SetCommandProcessor(cp sim.Port) {
	c.commandProcessor = cp
}

// SetTLBs registers every TLB control port that the GMMU must reach when it
// coordinates a UVM range invalidation. // sbin_codex
func (c *Comp) SetTLBs(ports []sim.RemotePort) {
	c.TLBs = ports
}
