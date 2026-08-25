package gmmu

import (
	"log"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/tracing"
)

type ctrlMiddleware struct {
	*Comp
}

func (m *ctrlMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.handleIncomingCommands() || madeProgress
	madeProgress = m.handleUVMControl() || madeProgress // sbin_codex
	return madeProgress
}

func (m *ctrlMiddleware) handleIncomingCommands() bool {
	madeProgress := false
	msg := m.controlPort.PeekIncoming()

	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *mem.ControlMsg:
		madeProgress = m.handleControlMsg(msg) || madeProgress
	default:
		panic("Unhandled message")
	}

	return madeProgress
}

func (m *ctrlMiddleware) handleControlMsg(
	msg *mem.ControlMsg) bool {
	m.ctrlMsgMustBeValidInCurrentStage(msg)

	return m.performCtrlReq()
}

func (m *ctrlMiddleware) ctrlMsgMustBeValidInCurrentStage(msg *mem.ControlMsg) {
	switch state := m.state; state {
	case gmmuStateEnable: // sbin_codex
		if msg.Enable {
			log.Panic("GMMU is already enabled")
		}
	case gmmuStatePause: // sbin_codex
		if msg.Pause {
			log.Panic("GMMU is already paused")
		}
		if msg.Drain {
			log.Panic("Cannot drain when GMMU is paused")
		}
	case gmmuStateDrain: // sbin_codex
		if msg.Drain {
			log.Panic("GMMU is already draining")
		}
		if msg.Pause || msg.Enable {
			log.Panic("Cannot pause/enable when GMMU is draining")
		}
	case gmmuStateFlush: // sbin_codex
		if msg.Drain || msg.Enable || msg.Pause {
			log.Panic("Cannot pause/enable/drain when GMMU is flushing")
		}
	default:
		log.Panic("Unknown GMMU state")
	}
}

func (m *ctrlMiddleware) performCtrlReq() bool {
	item := m.controlPort.PeekIncoming()
	if item == nil {
		return false
	}

	req := item.(*mem.ControlMsg)

	if req.Enable {
		m.state = gmmuStateEnable // sbin_codex
	} else if req.Drain {
		m.state = gmmuStateDrain // sbin_codex
	} else if req.Pause {
		m.state = gmmuStatePause // sbin_codex
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
