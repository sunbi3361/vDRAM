package accesscounter

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

func (c *Comp) processBottomResponse() bool {
	message := c.Bottom.PeekIncoming()
	response, ok := message.(mem.AccessRsp)
	if message == nil || !ok {
		return false
	}
	trans, found := c.transactions[response.GetRspTo()]
	if !found {
		c.Bottom.RetrieveIncoming()
		return true
	}
	forwarded := c.cloneResponse(response, trans.originalRequest)
	if sendError := c.Top.Send(forwarded); sendError != nil {
		return false
	}
	c.Bottom.RetrieveIncoming()
	delete(c.transactions, response.GetRspTo())
	return true
}

func (c *Comp) cloneResponse(response mem.AccessRsp, original sim.Msg) mem.AccessRsp {
	switch response := response.(type) {
	case *mem.DataReadyRsp:
		return mem.DataReadyRspBuilder{}.
			WithSrc(c.Top.AsRemote()).
			WithDst(original.Meta().Src).
			WithRspTo(original.Meta().ID).
			WithData(response.Data).
			Build()
	case *mem.WriteDoneRsp:
		return mem.WriteDoneRspBuilder{}.
			WithSrc(c.Top.AsRemote()).
			WithDst(original.Meta().Src).
			WithRspTo(original.Meta().ID).
			Build()
	default:
		return nil
	}
}
