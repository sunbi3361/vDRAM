package accesscounter

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type transaction struct {
	originalRequest sim.Msg
}

// Comp proxies PCIe requests to CPU memory and owns remote-demand counters.
// sbin_codex
type Comp struct {
	*sim.TickingComponent

	Top    sim.Port
	Bottom sim.Port

	threshold         uint64
	bottomDestination sim.RemotePort
	driverDestination sim.RemotePort

	counters             map[RegionKey]*counterState
	transactions         map[string]transaction
	pendingNotifications []*vm.AccessCounterNotifyReq
}

// Tick advances forwarding, reset handling, and notification retries.
// sbin_codex
func (c *Comp) Tick() bool {
	madeProgress := c.processBottomResponse()
	madeProgress = c.processTopMessage() || madeProgress
	madeProgress = c.retryNotification() || madeProgress
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
		return c.forwardWrite(request)
	case *vm.AccessCounterResetReq:
		c.Top.RetrieveIncoming()
		c.reset(request)
		return true
	default:
		c.Top.RetrieveIncoming()
		return true
	}
}

func (c *Comp) forwardRead(request *mem.ReadReq) bool {
	forwarded := request.Clone().(*mem.ReadReq)
	return c.forwardRequest(request, forwarded, request.RemoteDemandInfo)
}

func (c *Comp) forwardWrite(request *mem.WriteReq) bool {
	forwarded := request.Clone().(*mem.WriteReq)
	return c.forwardRequest(request, forwarded, request.RemoteDemandInfo)
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
	c.transactions[forwarded.Meta().ID] = transaction{originalRequest: original}
	if remoteDemand != nil {
		c.countAccepted(*remoteDemand)
	}
	return true
}
