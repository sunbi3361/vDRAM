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

	// sbin_codex: UVM control plane. ToUVMDriver faces the host UVM driver
	// across PCIe; ToUVMInternal faces the GPU-internal UVM endpoints.
	ToUVMDriver   sim.Port
	ToUVMInternal sim.Port

	GMMU          sim.RemotePort
	AccessCounter sim.RemotePort
	UVMDriverPort sim.RemotePort
	// UVMTranslators are every address translator that must drain a region
	// before its cache lines are written back. Kept separate from the legacy
	// shootdown groups so the non-UVM path is untouched. // sbin_codex
	UVMTranslators []sim.RemotePort

	currShootdownRequest *protocol.ShootDownCommand
	currFlushRequest     *protocol.FlushReq

	numCUAck                       uint64
	numPreCacheTranslatorFlushAck  uint64 // sbin_codex
	numPostCacheTranslatorFlushAck uint64 // sbin_codex
	numAddrTranslationRestartAck   uint64
	numTLBAck                      uint64
	numCacheACK                    uint64

	currCacheRangeFlush   *protocol.UVMCacheRangeFlushReq // sbin_codex
	numCacheRangeFlushAck uint64                          // sbin_codex
	numUVMDrainAck        uint64                          // sbin_codex
	cacheFlushIssued      bool                            // sbin_codex
	currRemoteDrain       *protocol.UVMRemoteDrainReq     // sbin_codex
	remoteDrainID         string                          // sbin_codex

	shootDownInProcess bool

	bottomKernelLaunchReqIDToTopReqMap map[string]*protocol.LaunchKernelReq
	bottomMemCopyH2DReqIDToTopReqMap   map[string]*protocol.MemCopyH2DReq
	bottomMemCopyD2HReqIDToTopReqMap   map[string]*protocol.MemCopyD2HReq

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
	madeProgress = p.uvmMiddleware.Tick() || madeProgress // sbin_codex

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

	if !madeProgress {
		return false
	}
	return madeProgress
}

func (p *CommandProcessor) processRspFromInternal() bool {
	madeProgress := false

	madeProgress = p.middleware.Tick() || madeProgress
	madeProgress = p.ctrlMiddleware.Tick() || madeProgress

	return madeProgress
}
