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
