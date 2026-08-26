// Package accesscounter implements the GPU-side UVM access counter.
//
// sbin_codex: the counter sits on the GPU's remote-memory egress, immediately
// after address translation has identified a request as CPU-remote (spec 6.1,
// 14). That placement is what makes a remote PTE cached in the L2 TLB unable
// to bypass accounting.
//
// It implements two policies:
//
//	remote read  -> forward over PCIe, count, notify at the threshold
//	remote write -> stall, notify immediately, replay after migration
//
// A remote write is never committed to host memory (spec 15).
package accesscounter

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type transaction struct {
	originalRequest sim.Msg
	region          RegionKey // sbin_codex
}

// Comp forwards GPU remote-memory traffic and owns the 64KB remote-access
// counters of one GPU.
type Comp struct {
	*sim.TickingComponent

	Top    sim.Port
	Bottom sim.Port
	Ctrl   sim.Port

	deviceID          uint64
	threshold         uint64
	numReqPerCycle    int
	bottomDestination sim.RemotePort
	ctrlDestination   sim.RemotePort

	counters      map[RegionKey]*counterState
	transactions  map[string]transaction
	stalledWrites map[RegionKey][]*mem.WriteReq

	pendingNotifications []*vm.AccessCounterNotifyReq
	pendingRetries       []*vm.UVMRemoteRetryRsp
	pendingRemoteWrites  []*mem.WriteReq

	stats Stats
}

// Tick advances forwarding, control handling, and notification retries.
func (c *Comp) Tick() bool {
	madeProgress := false

	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.processBottomResponse() || madeProgress
		madeProgress = c.processTopMessage() || madeProgress
	}

	madeProgress = c.processCtrlMessage() || madeProgress
	madeProgress = c.retryNotification() || madeProgress
	madeProgress = c.sendPendingRetry() || madeProgress
	madeProgress = c.sendPendingRemoteWrite() || madeProgress

	return madeProgress
}

func (c *Comp) processTopMessage() bool {
	message := c.Top.PeekIncoming()

	switch request := message.(type) {
	case nil:
		return false
	case *mem.ReadReq:
		return c.forwardRead(request)
	case *mem.WriteReq:
		return c.handleWrite(request)
	default:
		c.Top.RetrieveIncoming()
		return true
	}
}

// processCtrlMessage handles the driver commands the Command Processor relays.
func (c *Comp) processCtrlMessage() bool {
	message := c.Ctrl.PeekIncoming()

	switch request := message.(type) {
	case nil:
		return false
	case *vm.AccessCounterResetReq:
		c.Ctrl.RetrieveIncoming()

		if request.ResetAll {
			c.ResetAll()
			return true
		}

		c.resetRegion(RegionKey{
			PID: request.PID, RegionBase: request.RegionBase,
		})

		return true
	case *vm.UVMFaultReplayReq:
		c.Ctrl.RetrieveIncoming()
		c.releaseRange(request)

		return true
	case *vm.UVMDrainRangeReq: // sbin_codex
		return c.drainRange(request)
	default:
		c.Ctrl.RetrieveIncoming()
		return true
	}
}

func (c *Comp) forwardRead(request *mem.ReadReq) bool {
	if request.RemoteDemandInfo == nil {
		return c.forwardRequest(request, request.Clone(), nil)
	}

	forwarded := request.Clone().(*mem.ReadReq)

	return c.forwardRequest(request, forwarded, request.RemoteDemandInfo)
}

// handleWrite stalls a remote write instead of performing it, and asks the
// driver to migrate the region (spec 15).
func (c *Comp) handleWrite(request *mem.WriteReq) bool {
	if request.RemoteDemandInfo == nil {
		return c.forwardRequest(request, request.Clone(), nil)
	}

	key := regionKey(
		request.RemoteDemandInfo.PID, request.RemoteDemandInfo.VAddr)

	c.Top.RetrieveIncoming()
	c.stalledWrites[key] = append(c.stalledWrites[key], request)
	c.stats.StalledWrites++
	c.notifyRegion(key, request.RemoteDemandInfo.DeviceID)

	return true
}

func (c *Comp) forwardRequest(
	original sim.Msg,
	forwarded sim.Msg,
	remoteDemand *mem.RemoteDemandInfo,
) bool {
	forwarded.Meta().Src = c.Bottom.AsRemote()
	forwarded.Meta().Dst = c.bottomDestination

	if sendError := c.Bottom.Send(forwarded); sendError != nil {
		return false
	}

	c.Top.RetrieveIncoming()
	c.trackTransaction(forwarded.Meta().ID, original, remoteDemand)

	if remoteDemand != nil {
		c.countAccepted(*remoteDemand)
	}

	return true
}

// trackTransaction remembers which region an outstanding remote access belongs
// to, so a drain can tell when the region is quiet. // sbin_codex
func (c *Comp) trackTransaction(
	id string,
	original sim.Msg,
	remoteDemand *mem.RemoteDemandInfo,
) {
	trans := transaction{originalRequest: original}
	if remoteDemand != nil {
		trans.region = regionKey(remoteDemand.PID, remoteDemand.VAddr)
	}

	c.transactions[id] = trans
}

// drainRange answers once no remote access to the region is outstanding.
// sbin_codex
func (c *Comp) drainRange(req *vm.UVMDrainRangeReq) bool {
	if c.hasOutstandingInRange(req) {
		return false
	}

	rsp := vm.NewUVMDrainRangeRsp(c.Ctrl.AsRemote(), req.Src, req.ID)
	if err := c.Ctrl.Send(rsp); err != nil {
		return false
	}

	c.Ctrl.RetrieveIncoming()

	return true
}

func (c *Comp) hasOutstandingInRange(req *vm.UVMDrainRangeReq) bool {
	end := req.StartVA + req.Size

	for _, trans := range c.transactions {
		if trans.region.PID != req.PID {
			continue
		}

		if trans.region.RegionBase >= req.StartVA &&
			trans.region.RegionBase < end {
			return true
		}
	}

	for _, write := range c.pendingRemoteWrites {
		if write.RemoteDemandInfo == nil {
			continue
		}

		key := regionKey(
			write.RemoteDemandInfo.PID, write.RemoteDemandInfo.VAddr)
		if key.PID == req.PID &&
			key.RegionBase >= req.StartVA && key.RegionBase < end {
			return true
		}
	}

	return false
}

// GetDeviceID reports which GPU this counter belongs to.
func (c *Comp) GetDeviceID() uint64 {
	return c.deviceID
}

// SetCtrlDestination sets the UVM control endpoint notifications are sent to.
func (c *Comp) SetCtrlDestination(dst sim.RemotePort) {
	c.ctrlDestination = dst
}

// SetBottomDestination sets the remote-memory endpoint reads are forwarded to.
func (c *Comp) SetBottomDestination(dst sim.RemotePort) {
	c.bottomDestination = dst
}
