package cp

import (
	"github.com/sarchlab/akita/v4/mem/idealmemcontroller"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp/internal/dispatching"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp/internal/resource"
)

// CommandProcessor is an Akita component that is responsible for receiving
// requests from the driver and dispatch the requests to other parts of the
// GPU.
type CommandProcessor struct {
	*sim.TickingComponent

	Dispatchers          []dispatching.Dispatcher
	DMAEngine            sim.Port
	Driver               sim.Port
	TLBs                 []sim.Port
	CUs                  []sim.RemotePort
	PreCacheTranslators  TranslatorControlGroup // sbin_codex: discarded before cache flush so L1 traffic is quiescent.
	PostCacheTranslators TranslatorControlGroup // sbin_codex: kept active through dirty writeback, then discarded before TLB flush.
	RDMA                 sim.Port
	PMC                  sim.Port
	L1VCaches            []sim.Port
	L1SCaches            []sim.Port
	L1ICaches            []sim.Port
	L2Caches             []sim.Port
	DRAMControllers      []*idealmemcontroller.Comp

	ToDriver             sim.Port
	ToDMA                sim.Port
	ToCUs                sim.Port
	ToTLBs               sim.Port
	ToAddressTranslators sim.Port
	ToCaches             sim.Port
	ToRDMA               sim.Port
	ToPMC                sim.Port
	ToGMMU               sim.Port // sbin_codex: UVM GMMU control seam (todo 8).
	ToAccessCounter      sim.Port // sbin_codex: GPU-wide AccessCounter seam (todo 12).

	// sbin_codex: UVMGateIDs is the pre-registered gate ID set for UVM
	// block/unblock commands (todo 8 of mgpusim-uvm-manager). The topology
	// registers one entry per gate; the CP aggregates one matching ack per
	// gate.
	UVMGateIDs []uint64

	// sbin_codex: GMMUControl is the GMMU control port remote that the UVM
	// middleware targets for block/unblock commands (todo 12 of
	// mgpusim-uvm-manager). The topology builder registers it alongside the
	// ToGMMU seam.
	GMMUControl sim.RemotePort

	currShootdownRequest *protocol.ShootDownCommand
	currFlushRequest     *protocol.FlushReq

	numCUAck                       uint64
	numPreCacheTranslatorFlushAck  uint64 // sbin_codex
	numPostCacheTranslatorFlushAck uint64 // sbin_codex
	numAddrTranslationRestartAck   uint64
	numTLBAck                      uint64
	numCacheACK                    uint64

	shootDownInProcess bool

	bottomKernelLaunchReqIDToTopReqMap map[string]*protocol.LaunchKernelReq
	bottomMemCopyH2DReqIDToTopReqMap   map[string]*protocol.MemCopyH2DReq
	bottomMemCopyD2HReqIDToTopReqMap   map[string]*protocol.MemCopyD2HReq

	// sbin_codex: UVM routing state (todo 12 of mgpusim-uvm-manager).
	activeUVMBlocks               map[uint64]*uvmBlockState
	activeUVMTLBInvalidates       map[string]*uvmTLBInvalidateState
	activeUVMCacheFlushes         map[string]*uvmCacheFlushState
	uvmTLBInvalidateFanout        map[string]string
	uvmCacheFlushFanout           map[string]string
	uvmReplayRangeIDToDriverReqID map[string]string
	counterResetPending           bool

	middleware     *cpMiddleware
	ctrlMiddleware *ctrlMiddleware
	uvmMiddleware  *uvmMiddleware // sbin_codex
}

// TranslatorControlGroup is a lifecycle phase's ordered translator controls. // sbin_codex
type TranslatorControlGroup struct {
	Ports []sim.Port // sbin_codex
}

// CUInterfaceForCP defines the interface that a CP requires from CU.
type CUInterfaceForCP interface {
	resource.DispatchableCU

	// ControlPort returns a port on the CU that the CP can send controlling
	// messages to.
	ControlPort() sim.RemotePort
}

// RegisterCU allows the Command Processor to control the CU.
func (p *CommandProcessor) RegisterCU(cu CUInterfaceForCP) {
	p.CUs = append(p.CUs, cu.ControlPort())
	for _, d := range p.Dispatchers {
		d.RegisterCU(cu)
	}
}

// Tick ticks
func (p *CommandProcessor) Tick() bool {
	madeProgress := false

	madeProgress = p.tickDispatchers() || madeProgress
	madeProgress = p.processReqFromDriver() || madeProgress
	madeProgress = p.processRspFromInternal() || madeProgress

	return madeProgress
}

func (p *CommandProcessor) tickDispatchers() (madeProgress bool) {
	for _, d := range p.Dispatchers {
		madeProgress = d.Tick() || madeProgress
	}

	return madeProgress
}

func (p *CommandProcessor) processReqFromDriver() bool {
	madeProgress := false
	msg := p.ToDriver.PeekIncoming()

	if msg == nil {
		return madeProgress
	}

	madeProgress = p.middleware.Tick() || madeProgress
	madeProgress = p.ctrlMiddleware.Tick() || madeProgress
	madeProgress = p.uvmMiddleware.Tick() || madeProgress // sbin_codex

	if !madeProgress {
		return false
	}
	return madeProgress
}

func (p *CommandProcessor) processRspFromInternal() bool {
	madeProgress := false

	madeProgress = p.middleware.Tick() || madeProgress
	madeProgress = p.ctrlMiddleware.Tick() || madeProgress
	madeProgress = p.uvmMiddleware.Tick() || madeProgress // sbin_codex

	return madeProgress
}
