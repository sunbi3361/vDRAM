package gmmu

import (
	"log"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type ctrlMiddleware struct {
	*Comp
}

func (m *ctrlMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.handleIncomingCommands() || madeProgress
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
	case *vm.BlockRange: // sbin_codex: UVM mapping-transition block (todo 8).
		madeProgress = m.handleBlockRange(msg) || madeProgress
	case *vm.UnblockRange: // sbin_codex: UVM mapping-transition unblock (todo 8).
		madeProgress = m.handleUnblockRange(msg) || madeProgress
	default:
		panic("Unhandled message")
	}

	return madeProgress
}

// sbin_codex: handleBlockRange atomically closes matching admission on the
// translation gate, snapshots the local watermark, and acknowledges the
// command once every matching request with sequence<=watermark is disposed
// (todo 8 of mgpusim-uvm-manager). A duplicate command is rejected.
func (m *ctrlMiddleware) handleBlockRange(msg *vm.BlockRange) bool {
	for _, block := range m.activeBlocks {
		if block.commandID == msg.CommandID {
			m.controlPort.RetrieveIncoming()
			return true
		}
	}

	block := &blockCommand{
		commandID: msg.CommandID,
		pid:       msg.PID,
		startVA:   msg.StartVA,
		size:      msg.Size,
		watermark: m.gate.lastAssignedSequence,
		src:       msg.Src,
	}
	for i := range m.walkingTranslations {
		trans := &m.walkingTranslations[i]
		if trans.sequence <= block.watermark && block.matches(trans.req) {
			block.pendingDisposals++
		}
	}
	m.activeBlocks = append(m.activeBlocks, block)

	m.controlPort.RetrieveIncoming()
	m.trySendBlockAcks()

	return true
}

// sbin_codex: handleUnblockRange releases the parked requests of the matching
// block and acknowledges the command. An unknown command is rejected.
func (m *ctrlMiddleware) handleUnblockRange(msg *vm.UnblockRange) bool {
	for i, block := range m.activeBlocks {
		if block.commandID != msg.CommandID {
			continue
		}

		m.activeBlocks = append(m.activeBlocks[:i], m.activeBlocks[i+1:]...)
		m.releaseParked(block)

		m.controlPort.RetrieveIncoming()
		m.sendUnblockAck(msg)

		return true
	}

	m.controlPort.RetrieveIncoming()
	return true
}

// sbin_codex: releaseParked re-admits the parked requests of a released
// block. A request that matches another still-closed block stays parked
// there.
func (m *ctrlMiddleware) releaseParked(block *blockCommand) {
	for _, parked := range block.parked {
		if next := m.matchingClosedBlock(parked.req); next != nil {
			next.parked = append(next.parked, parked)
			continue
		}
		m.appendWalking(parked.req, parked.sequence, nil, nil)
	}
	block.parked = nil
}

func (m *ctrlMiddleware) sendUnblockAck(msg *vm.UnblockRange) bool {
	if !m.controlPort.CanSend() {
		return false
	}
	ack := &vm.UnblockAck{CommandID: msg.CommandID}
	ack.ID = sim.GetIDGenerator().Generate()
	ack.Src = m.controlPort.AsRemote()
	ack.Dst = msg.Src
	if err := m.controlPort.Send(ack); err != nil {
		return false
	}
	return true
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
