package idealtlb

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/tracing"
)

// ctrlMiddleware handles command messages on the control port.
type ctrlMiddleware struct {
	*Comp
}

// Tick processes incoming control messages.
func (m *ctrlMiddleware) Tick() bool {
	return m.handleIncomingCommands()
}

func (m *ctrlMiddleware) handleIncomingCommands() bool {
	msg := m.controlPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *mem.ControlMsg:
		return m.handleControlMsg(msg)
	case *tlb.FlushReq:
		return m.handleFlushReq(msg)
	case *tlb.RestartReq:
		return m.handleRestartReq(msg)
	default:
		panic("Unhandled message")
	}
}

func (m *ctrlMiddleware) handleControlMsg(msg *mem.ControlMsg) bool {
	m.ctrlMsgMustBeValidInCurrentStage(msg)

	return m.performCtrlReq()
}

func (m *ctrlMiddleware) ctrlMsgMustBeValidInCurrentStage(msg *mem.ControlMsg) {
	switch state := m.state; state {
	case tlbStateEnable:
		if msg.Enable {
			panic("TLB is already enabled")
		}
	case tlbStatePause:
		if msg.Pause {
			panic("TLB is already paused")
		}
		if msg.Drain {
			panic("Cannot drain when TLB is paused")
		}
	case tlbStateDrain:
		if msg.Drain {
			panic("TLB is already draining")
		}
		if msg.Pause || msg.Enable {
			panic("Cannot pause/enable when TLB is draining")
		}
	case tlbStateFlush:
		if msg.Drain || msg.Enable || msg.Pause {
			panic("Cannot pause/enable/drain when TLB is flushing")
		}
	default:
		panic("Unknown TLB state")
	}
}

func (m *ctrlMiddleware) performCtrlReq() bool {
	item := m.controlPort.PeekIncoming()
	if item == nil {
		return false
	}

	req := item.(*mem.ControlMsg)

	if req.Enable {
		m.state = tlbStateEnable
	} else if req.Drain {
		m.state = tlbStateDrain
	} else if req.Pause {
		m.state = tlbStatePause
	}

	item = m.controlPort.RetrieveIncoming()
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(item, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.controlPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	return true
}

func (m *ctrlMiddleware) handleFlushReq(req *tlb.FlushReq) bool {
	if !m.controlPort.CanSend() {
		return false
	}

	rsp := tlb.FlushRspBuilder{}.
		WithSrc(m.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	if err := m.controlPort.Send(rsp); err != nil {
		return false
	}

	// sbin_codex: ideal TLB holds no entries, so flush completes immediately.
	m.controlPort.RetrieveIncoming()
	m.state = tlbStateEnable

	return true
}

func (m *ctrlMiddleware) handleRestartReq(req *tlb.RestartReq) bool {
	rsp := tlb.RestartRspBuilder{}.
		WithSrc(m.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	if err := m.controlPort.Send(rsp); err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.controlPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	m.state = tlbStateEnable

	for m.topPort.PeekIncoming() != nil {
		m.topPort.RetrieveIncoming()
	}

	for m.bottomPort.PeekIncoming() != nil {
		m.bottomPort.RetrieveIncoming()
	}

	m.controlPort.RetrieveIncoming()

	return true
}
