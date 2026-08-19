package dispatching

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/monitoring"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/kernels"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/sampling"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp/internal/resource"
)

// A Dispatcher is a sub-component of a command processor that can dispatch
// work-groups to compute units.
type Dispatcher interface {
	tracing.NamedHookable
	RegisterCU(cu resource.DispatchableCU)
	IsDispatching() bool
	StartDispatching(req *protocol.LaunchKernelReq)
	Tick() (madeProgress bool)
}

// A DispatcherImpl is a ticking component that can dispatch work-groups.
type DispatcherImpl struct {
	sim.HookableBase

	cp                             tracing.NamedHookable
	name                           string
	respondingPort                 sim.Port
	dispatchingPort                sim.Port
	alg                            algorithm
	dispatching                    *protocol.LaunchKernelReq
	currWG                         dispatchLocation
	cycleLeft                      int
	numDispatchedWGs               int
	numCompletedWGs                int
	inflightWGs                    map[string]dispatchLocation
	originalReqs                   map[string]*protocol.MapWGReq
	latencyTable                   []int
	constantKernelOverhead         int
	constantKernelLaunchOverhead   int
	subsequentKernelLaunchOverhead int
	firstKernelLaunched            bool
	prevKernelWGCount              int
	wgScalingThreshold             int
	maxInflightWGs                 int // sbin_codex

	monitor     *monitoring.Monitor
	progressBar *monitoring.ProgressBar
}

// Name returns the name of the dispatcher
func (d *DispatcherImpl) Name() string {
	return d.name
}

// RegisterCU allows the dispatcher to dispatch work-groups to the CU.
func (d *DispatcherImpl) RegisterCU(cu resource.DispatchableCU) {
	d.alg.RegisterCU(cu)
}

// IsDispatching checks if the dispatcher is dispatching another kernel.
func (d *DispatcherImpl) IsDispatching() bool {
	return d.dispatching != nil
}

// StartDispatching lets the dispatcher to start dispatch another kernel.
func (d *DispatcherImpl) StartDispatching(req *protocol.LaunchKernelReq) {
	d.mustNotBeDispatchingAnotherKernel()

	d.alg.StartNewKernel(kernels.KernelLaunchInfo{
		CodeObject: req.CodeObject,
		Packet:     req.Packet,
		PacketAddr: req.PacketAddress,
		WGFilter:   req.WGFilter,
	})
	d.dispatching = req

	d.numDispatchedWGs = 0
	d.numCompletedWGs = 0

	d.maxInflightWGs = 0      // sbin_codex
	totalWGs := d.alg.NumWG() // sbin_codex

	// sbin_codex
	if req.CodeObject != nil && req.CodeObject.Symbol != nil &&
		req.Packet != nil {
		fmt.Printf("[kernel-info] %s grid=%dx%dx%d wg=%dx%dx%d totalWGs=%d\n",
			req.CodeObject.Symbol.Name,
			req.Packet.GridSizeX, req.Packet.GridSizeY, req.Packet.GridSizeZ,
			req.Packet.WorkgroupSizeX, req.Packet.WorkgroupSizeY,
			req.Packet.WorkgroupSizeZ, totalWGs)
	}
	if !d.firstKernelLaunched {
		d.cycleLeft = d.constantKernelLaunchOverhead
		d.firstKernelLaunched = true
	} else {
		if d.prevKernelWGCount > 0 && d.wgScalingThreshold > 0 {
			scale := float64(d.wgScalingThreshold) / float64(d.prevKernelWGCount)
			d.cycleLeft = int(float64(d.subsequentKernelLaunchOverhead) * scale)
		} else {
			d.cycleLeft = d.subsequentKernelLaunchOverhead
		}
	}

	d.initializeProgressBar(req.ID)
}

func (d *DispatcherImpl) initializeProgressBar(kernelID string) {
	if d.monitor != nil {
		d.progressBar = d.monitor.CreateProgressBar(
			fmt.Sprintf("At %s, Kernel: %s, ", d.Name(), kernelID),
			uint64(d.alg.NumWG()),
		)
	}
}

func (d *DispatcherImpl) mustNotBeDispatchingAnotherKernel() {
	if d.IsDispatching() {
		panic("dispatcher is dispatching another request")
	}
}

// Tick updates the state of the dispatcher.
func (d *DispatcherImpl) Tick() (madeProgress bool) {
	if d.cycleLeft > 0 {
		d.cycleLeft--
		return true
	}

	if d.dispatching != nil {
		if d.kernelCompleted() {
			madeProgress = d.completeKernel() || madeProgress
		} else {
			// Dispatch up to 8 WGs per cycle
			for i := 0; i < 8; i++ {
				progress := d.dispatchNextWG()
				madeProgress = progress || madeProgress
				if !progress || d.cycleLeft > 0 {
					break
				}
			}
		}
	}

	madeProgress = d.processMessagesFromCU() || madeProgress

	return madeProgress
}

func (d *DispatcherImpl) collectSamplingData(locations []protocol.WfDispatchLocation) {
	if *sampling.SampledRunnerFlag {
		for _, l := range locations {
			wavefront := l.Wavefront
			sampling.SampledEngineInstance.Collect(
				wavefront.IssueTime, wavefront.FinishTime)
		}
	}
}

func (d *DispatcherImpl) processMessagesFromCU() bool {
	madeProgress := false

	for i := 0; i < 8; i++ {
		msg := d.dispatchingPort.PeekIncoming()
		if msg == nil {
			break
		}

		switch msg := msg.(type) {
		case *protocol.WGCompletionMsg:
			count := 0
			for _, rspToID := range msg.RspTo {
				location, ok := d.inflightWGs[rspToID]
				if ok {
					count += 1
					///sampling
					d.collectSamplingData(location.locations)
				}
			}

			if count == 0 {
				return madeProgress
			} else if count < len(msg.RspTo) {
				log.Panic("In emulation all finished WGs from more than one dispatcher")
			}

			for _, rspToID := range msg.RspTo {
				location := d.inflightWGs[rspToID]
				d.alg.FreeResources(location)
				delete(d.inflightWGs, rspToID)
				d.numCompletedWGs++
				if d.numCompletedWGs == d.alg.NumWG() {
					d.cycleLeft = d.constantKernelOverhead
				}
				d.printWGProgress() // sbin_codex

				originalReq := d.originalReqs[rspToID]
				delete(d.originalReqs, rspToID)
				tracing.TraceReqFinalize(originalReq, d)

				if d.progressBar != nil {
					d.progressBar.MoveInProgressToFinished(1)
				}
			}

			d.dispatchingPort.RetrieveIncoming()
			madeProgress = true
		default:
			// Unknown message type, stop processing
			return madeProgress
		}
	}

	return madeProgress
}

// sbin_codex:
// updateMaxInflight tracks the highest number of work-groups resident on the
// GPU at once. Once the GPU has been filled, this equals the number of
// work-groups that can be loaded concurrently (its resident capacity).
func (d *DispatcherImpl) updateMaxInflight() {
	inflight := d.numDispatchedWGs - d.numCompletedWGs
	if inflight > d.maxInflightWGs {
		d.maxInflightWGs = inflight
	}
}

// sbin_codex:
// printWGProgress reports kernel progress as completed GPU-loads ("waves"):
// each wave is one full batch of work-groups resident on the GPU at once.
func (d *DispatcherImpl) printWGProgress() {
	capacity := d.maxInflightWGs
	total := d.alg.NumWG()
	// if capacity <= 0 || d.numCompletedWGs%capacity != 0 {
	// 	return
	// }
	if capacity <= 0 ||
		(d.numCompletedWGs%capacity != 0 && d.numCompletedWGs != total) {
		return
	}
	totalWaves := (total + capacity - 1) / capacity
	curWave := (d.numCompletedWGs + capacity - 1) / capacity
	fmt.Printf("[kernel-progress] %s wave %d/%d (%d%%) [cap=%d WGs]\n",
		d.kernelName(), curWave, totalWaves, curWave*100/totalWaves, capacity)
}

// sbin_codex
func (d *DispatcherImpl) kernelName() string {
	if d.dispatching.CodeObject != nil && d.dispatching.CodeObject.Symbol != nil {
		if n := d.dispatching.CodeObject.Symbol.Name; n != "" {
			return n
		}
	}
	if d.dispatching.Packet != nil {
		return fmt.Sprintf("kernel@0x%x", d.dispatching.Packet.KernelObject)
	}
	return "kernel"
}

func (d *DispatcherImpl) kernelCompleted() bool {
	if d.currWG.valid {
		return false
	}

	if d.alg.HasNext() {
		return false
	}

	if d.numCompletedWGs < d.numDispatchedWGs {
		return false
	}

	return true
}

func (d *DispatcherImpl) completeKernel() (
	madeProgress bool,
) {
	req := d.dispatching

	rsp := protocol.NewLaunchKernelRsp(req.Dst, req.Src, req.ID)

	err := d.respondingPort.Send(rsp)
	if err == nil {
		d.prevKernelWGCount = d.numDispatchedWGs
		d.dispatching = nil

		if d.monitor != nil {
			d.monitor.CompleteProgressBar(d.progressBar)
		}

		tracing.TraceReqComplete(req, d.cp)

		return true
	}

	return false
}

func (d *DispatcherImpl) dispatchNextWG() (madeProgress bool) {
	if !d.currWG.valid {
		if !d.alg.HasNext() {
			return false
		}
		d.currWG = d.alg.Next()
		if !d.currWG.valid {
			return false
		}
	}

	reqBuilder := protocol.MapWGReqBuilder{}.
		WithSrc(d.dispatchingPort.AsRemote()).
		WithDst(d.currWG.cu).
		WithPID(d.dispatching.PID).
		WithWG(d.currWG.wg)
	for _, l := range d.currWG.locations {
		reqBuilder = reqBuilder.AddWf(l)
	}
	req := reqBuilder.Build()
	err := d.dispatchingPort.Send(req)

	// fmt.Printf("%.10f, %d, %d\n", now, d.currWG.wg.IDX, d.currWG.cuID)

	if err == nil {
		d.currWG.valid = false
		d.numDispatchedWGs++
		d.updateMaxInflight() // sbin_codex
		d.inflightWGs[req.ID] = d.currWG
		d.originalReqs[req.ID] = req
		d.cycleLeft = d.latencyTable[len(d.currWG.locations)]

		if d.progressBar != nil {
			d.progressBar.IncrementInProgress(1)
		}

		tracing.TraceReqInitiate(req, d,
			tracing.MsgIDAtReceiver(d.dispatching, d.cp))

		return true
	}

	return false
}
