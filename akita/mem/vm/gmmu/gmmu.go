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

	// refaultedBy is the ID of the replay request that released this
	// transaction only for it to fault again. That replay must not pick it up
	// a second time, or it would keep releasing and re-parking the same
	// translation for as long as it sits at the head of the port. // sbin_codex
	refaultedBy string

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

	// sbin_claude_avatar: out-of-band cancel ingress (Avatar EAF,
	// refs/avatar.md 5.9). A cancel names a TranslationReq that its
	// requester abandoned; if that request is still queued in the top port
	// it is dropped at retrieve time, before it can occupy a walker slot.
	// A walk that already started is left to finish; its response is
	// dropped upstream. Inert on non-Avatar platforms.
	cancelPort   sim.Port
	canceledReqs map[string]struct{}

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

	// sbin_claude_hpt: FS-HPT (PACT'24) walk mode. hash(VPN) indexes a
	// fixed-size hashed table directly, so a walk costs hptAccessesPerWalk
	// memory references (1 when there is no hash collision) instead of one
	// per radix level, and no page-walk cache exists.
	hptEnabled         bool
	hptAccessesPerWalk int
	hptWalks           uint64
	hptMemoryAccesses  uint64

	walkingTranslations []transaction
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

// sbin_mcm
func (c *Comp) SetCommandProcessor(cp sim.Port) {
	c.commandProcessor = cp
}

// HPTStats reports the hashed-page-table walk counters. // sbin_claude_hpt
type HPTStats struct {
	// Walks is the number of page walks served by the hashed table.
	Walks uint64
	// MemoryAccesses is the number of modeled memory references those walks
	// cost (Walks x accesses-per-walk).
	MemoryAccesses uint64
}

// HashedPageTableEnabled reports whether this GMMU walks a hashed page table
// instead of the radix page table. // sbin_claude_hpt
func (c *Comp) HashedPageTableEnabled() bool {
	return c.hptEnabled
}

// HasPageWalkCache reports whether this GMMU owns a page-walk cache. HPT mode
// builds none: a hashed page table has no intermediate levels to cache.
// sbin_claude_hpt
func (c *Comp) HasPageWalkCache() bool {
	return c.pageWalkCachePort != nil
}

// HPTStats returns the hashed-page-table walk counters. They stay zero when
// the GMMU walks the radix page table. // sbin_claude_hpt
func (c *Comp) HPTStats() HPTStats {
	return HPTStats{
		Walks:          c.hptWalks,
		MemoryAccesses: c.hptMemoryAccesses,
	}
}

// SetTLBs registers every TLB control port that the GMMU must reach when it
// coordinates a UVM range invalidation. // sbin_codex
func (c *Comp) SetTLBs(ports []sim.RemotePort) {
	c.TLBs = ports
}
