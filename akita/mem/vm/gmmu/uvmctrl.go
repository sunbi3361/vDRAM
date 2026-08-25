package gmmu

// sbin_codex: the GMMU is the UVM range-invalidation coordinator (spec 21.1).
// It receives one UVMTLBInvalidateReq from the Command Processor, broadcasts a
// non-stalling range invalidation to every TLB level that may cache the
// mapping, collects the acknowledgements, and reports a single completion.
//
// No GPU-wide TLB flush and no pipeline restart is involved: unrelated
// translations keep flowing throughout.

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
)

func (m *ctrlMiddleware) handleUVMControl() bool {
	madeProgress := m.collectTLBInvalidateAcks()
	madeProgress = m.startTLBInvalidate() || madeProgress
	madeProgress = m.completeTLBInvalidate() || madeProgress

	return madeProgress
}

// startTLBInvalidate accepts one invalidation at a time and fans it out.
func (m *ctrlMiddleware) startTLBInvalidate() bool {
	if m.uvmPort == nil || m.tlbCtrlPort == nil {
		return false
	}

	if m.pendingTLBInvalidate != nil {
		return false
	}

	msg := m.uvmPort.PeekIncoming()
	if msg == nil {
		return false
	}

	req, ok := msg.(*vm.UVMTLBInvalidateReq)
	if !ok {
		return false
	}

	if !m.tlbCtrlPort.CanSend() {
		return false
	}

	for _, target := range m.TLBs {
		invalidate := tlb.InvalidateRangeReqBuilder{}.
			WithSrc(m.tlbCtrlPort.AsRemote()).
			WithDst(target).
			WithPID(req.PID).
			WithRange(req.StartVA, req.Size).
			Build()

		if err := m.tlbCtrlPort.Send(invalidate); err != nil {
			// Partially issued fan-outs are not possible: the loop only starts
			// once the port accepts, and a direct connection accepts every
			// message queued in the same cycle.
			panic("GMMU could not fan out a UVM range invalidation")
		}

		m.pendingTLBInvalidateACK++
	}

	m.pendingTLBInvalidate = req
	m.uvmPort.RetrieveIncoming()

	return true
}

func (m *ctrlMiddleware) collectTLBInvalidateAcks() bool {
	if m.tlbCtrlPort == nil {
		return false
	}

	msg := m.tlbCtrlPort.PeekIncoming()
	if msg == nil {
		return false
	}

	if _, ok := msg.(*tlb.InvalidateRangeRsp); !ok {
		return false
	}

	m.tlbCtrlPort.RetrieveIncoming()

	if m.pendingTLBInvalidateACK > 0 {
		m.pendingTLBInvalidateACK--
	}

	return true
}

func (m *ctrlMiddleware) completeTLBInvalidate() bool {
	req := m.pendingTLBInvalidate
	if req == nil || m.pendingTLBInvalidateACK > 0 {
		return false
	}

	if !m.uvmPort.CanSend() {
		return false
	}

	rsp := vm.NewUVMTLBInvalidateRsp(m.uvmPort.AsRemote(), req.Src, req.ID)
	if err := m.uvmPort.Send(rsp); err != nil {
		return false
	}

	m.pendingTLBInvalidate = nil

	return true
}
