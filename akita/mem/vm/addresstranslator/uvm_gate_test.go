package addresstranslator

// sbin_codex: UVM baseline access-gate contract tests (plan todo 9 of
// mgpusim-uvm-manager). These plain Go tests drive the gate-enabled address
// translator middleware with the package mocks and assert: GPU_LOCAL uses the
// HBM PA through the data cache, CPU_REMOTE reads are carried only to the
// remote endpoint (never into the GPU cache), CPU_REMOTE writes park, INVALID
// faults retain, the BlockRange/watermark/ack protocol, and the barrier races
// at ROB→gate, gate→cache, cache MSHR, and pre/post-barrier remote reads.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// uvmGateHarness builds a gate-enabled address translator with mocked ports so
// the middleware can be driven deterministically.
type uvmGateHarness struct {
	ctrl            *gomock.Controller
	comp            *Comp
	mw              *middleware
	topPort         *MockPort
	bottomPort      *MockPort
	translationPort *MockPort
	ctrlPort        *MockPort
	memMapper       *MockAddressToPortMapper
	transMapper     *MockAddressToPortMapper
	blockAck        *vm.BlockAck
	unblockAck      *vm.UnblockAck
	translated      mem.AccessReq
	transReq        *vm.TranslationReq
}

func newUVMGateHarness(t *testing.T, gateID uint64) *uvmGateHarness {
	t.Helper()
	ctrl := gomock.NewController(t)

	topPort := NewMockPort(ctrl)
	topPort.EXPECT().AsRemote().Return(sim.RemotePort("TopPort")).AnyTimes()
	topPort.EXPECT().Name().Return("TopPort").AnyTimes()
	bottomPort := NewMockPort(ctrl)
	bottomPort.EXPECT().AsRemote().Return(sim.RemotePort("BottomPort")).AnyTimes()
	bottomPort.EXPECT().Name().Return("BottomPort").AnyTimes()
	translationPort := NewMockPort(ctrl)
	translationPort.EXPECT().AsRemote().
		Return(sim.RemotePort("TranslationPort")).AnyTimes()
	translationPort.EXPECT().Name().Return("TranslationPort").AnyTimes()
	ctrlPort := NewMockPort(ctrl)
	ctrlPort.EXPECT().AsRemote().Return(sim.RemotePort("CtrlPort")).AnyTimes()
	ctrlPort.EXPECT().Name().Return("CtrlPort").AnyTimes()
	memMapper := NewMockAddressToPortMapper(ctrl)
	transMapper := NewMockAddressToPortMapper(ctrl)

	comp := MakeBuilder().
		WithLog2PageSize(12).
		WithFreq(1).
		WithMemoryProviderMapper(memMapper).
		WithTranslationProviderMapper(transMapper).
		WithUVMGateID(gateID).
		Build("UVMGateAT")
	comp.topPort = topPort
	comp.bottomPort = bottomPort
	comp.translationPort = translationPort
	comp.ctrlPort = ctrlPort

	mw := comp.Middlewares()[0].(*middleware)

	return &uvmGateHarness{
		ctrl:            ctrl,
		comp:            comp,
		mw:              mw,
		topPort:         topPort,
		bottomPort:      bottomPort,
		translationPort: translationPort,
		ctrlPort:        ctrlPort,
		memMapper:       memMapper,
		transMapper:     transMapper,
	}
}

func readReq(vAddr uint64) *mem.ReadReq {
	return mem.ReadReqBuilder{}.
		WithAddress(vAddr).
		WithByteSize(4).
		WithPID(1).
		Build()
}

func writeReq(vAddr uint64) *mem.WriteReq {
	return mem.WriteReqBuilder{}.
		WithAddress(vAddr).
		WithPID(1).
		WithData([]byte{1, 2, 3, 4}).
		WithDirtyMask([]bool{true, true, true, true}).
		Build()
}

// admit injects a request into the top port and runs the translate stage.
// expectTranslation selects whether the request is expected to start a
// translation (no matching closed block) or park (matching closed block).
func (h *uvmGateHarness) admit(req mem.AccessReq, expectTranslation bool) {
	h.topPort.EXPECT().PeekIncoming().Return(req)
	if expectTranslation {
		h.transMapper.EXPECT().Find(req.GetAddress()).
			Return(h.translationPort.AsRemote())
		h.translationPort.EXPECT().Send(gomock.Any()).
			DoAndReturn(func(r *vm.TranslationReq) *sim.SendError {
				h.transReq = r
				return nil
			})
	}
	h.topPort.EXPECT().RetrieveIncoming()
	if !h.mw.translate() {
		panic("translate stage made no progress")
	}
}

// deliverTranslation injects a translation response for the most recent
// transaction and runs the parse-translation stage. expectBottom selects
// whether the request is expected to be forwarded to the data cache.
func (h *uvmGateHarness) deliverTranslation(
	location vm.MemoryLocation,
	pAddr uint64,
	faultToken vm.FaultPendingToken,
	expectBottom bool,
) {
	trans := h.comp.transactions[len(h.comp.transactions)-1]
	req := trans.incomingReqs[0]
	rsp := vm.TranslationRspBuilder{}.
		WithRspTo(trans.translationReq.ID).
		WithPage(vm.Page{
			PID:      req.GetPID(),
			VAddr:    trans.translationReq.VAddr,
			PAddr:    pAddr,
			Valid:    location != vm.MemoryLocationINVALID,
			Managed:  true,
			Location: location,
			PageSize: 4096,
		}).
		WithLocation(location).
		WithFaultPendingToken(faultToken).
		Build()

	if expectBottom {
		addr := pAddr + (req.GetAddress() % 4096)
		h.memMapper.EXPECT().Find(addr).Return(h.bottomPort.AsRemote())
		h.bottomPort.EXPECT().Send(gomock.Any()).
			DoAndReturn(func(r mem.AccessReq) *sim.SendError {
				h.translated = r
				return nil
			})
	}
	h.translationPort.EXPECT().PeekIncoming().Return(rsp)
	h.translationPort.EXPECT().RetrieveIncoming()
	h.mw.parseTranslation()
}

// deliverBlockRange injects a BlockRange into the control port.
func (h *uvmGateHarness) deliverBlockRange(
	cmdID uint64, pid vm.PID, startVA, size uint64,
) {
	msg := &vm.BlockRange{CommandID: cmdID, PID: pid, StartVA: startVA, Size: size}
	msg.ID = sim.GetIDGenerator().Generate()
	msg.Src = sim.RemotePort("CP")
	msg.Dst = sim.RemotePort("CtrlPort")
	h.ctrlPort.EXPECT().PeekIncoming().Return(msg)
	h.ctrlPort.EXPECT().RetrieveIncoming()
	h.mw.handleCtrlRequest()
}

// deliverUnblockRange injects an UnblockRange into the control port and
// expects the UnblockAck.
func (h *uvmGateHarness) deliverUnblockRange(
	cmdID uint64, pid vm.PID, startVA, size uint64,
) {
	msg := &vm.UnblockRange{CommandID: cmdID, PID: pid, StartVA: startVA, Size: size}
	msg.ID = sim.GetIDGenerator().Generate()
	msg.Src = sim.RemotePort("CP")
	msg.Dst = sim.RemotePort("CtrlPort")
	h.ctrlPort.EXPECT().PeekIncoming().Return(msg)
	h.ctrlPort.EXPECT().RetrieveIncoming()
	h.ctrlPort.EXPECT().CanSend().Return(true)
	h.ctrlPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(a *vm.UnblockAck) error {
			h.unblockAck = a
			return nil
		})
	h.mw.handleCtrlRequest()
}

// captureBlockAck expects the next BlockAck send on the control port.
func (h *uvmGateHarness) captureBlockAck() {
	h.blockAck = nil
	h.ctrlPort.EXPECT().CanSend().Return(true)
	h.ctrlPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(a *vm.BlockAck) error {
			h.blockAck = a
			return nil
		})
}

// TestUVMBaselineLocal proves a GPU_LOCAL translation uses the HBM PA and is
// forwarded to the data cache as downstream-visible.
func TestUVMBaselineLocal(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(readReq(0x1000), true)
	h.deliverTranslation(vm.MemoryLocationGPU_LOCAL, 0x60000, 0, true)

	if h.translated == nil {
		t.Fatal("a local read must be forwarded to the data cache")
	}
	if h.translated.GetAddress() != 0x60000 {
		t.Fatalf("a local read must use the HBM PA, got %#x",
			h.translated.GetAddress())
	}
	if len(h.comp.transactions) != 0 {
		t.Fatal("the local read must be removed after forwarding")
	}
	if len(h.comp.inflightReqToBottom) != 1 {
		t.Fatal("the local read must be tracked in flight to the cache")
	}
}

// TestUVMBaselineRemoteRead proves a CPU_REMOTE read is carried only to the
// remote endpoint: it never enters the GPU data cache and is disposed as
// remote-committed, so a later block acks immediately.
func TestUVMBaselineRemoteRead(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(readReq(0x1000), true)
	h.deliverTranslation(vm.MemoryLocationCPU_REMOTE, 0x50000, 0, false)

	if h.translated != nil {
		t.Fatal("a remote read must never be inserted into the GPU data cache")
	}
	if len(h.comp.transactions) != 0 {
		t.Fatal("the remote read must be committed to the endpoint")
	}

	h.captureBlockAck()
	h.deliverBlockRange(15, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("an old remote read committed to the endpoint must not delay the ack")
	}
	if h.blockAck.GateID != 2 {
		t.Fatalf("the ack must carry the gate ID 2, got %d", h.blockAck.GateID)
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}
}

// TestUVMBaselineRemoteWrite proves a CPU_REMOTE write parks: it is never
// committed to the host or the cache and is retained in the gate.
func TestUVMBaselineRemoteWrite(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(writeReq(0x1000), true)
	h.deliverTranslation(vm.MemoryLocationCPU_REMOTE, 0x50000, 0, false)

	if h.translated != nil {
		t.Fatal("a remote write must never be committed to the host or cache")
	}
	if len(h.comp.transactions) != 1 {
		t.Fatal("the remote write must be parked in the gate")
	}
	if h.comp.transactions[0].disposition != dispositionRetained {
		t.Fatalf("the remote write must be retained, got %v",
			h.comp.transactions[0].disposition)
	}
	if !h.comp.transactions[0].disposed {
		t.Fatal("the parked remote write must count as disposed for a barrier")
	}
}

// TestUVMBaselineBarrierROBToGate proves a request still in the ROB when the
// barrier lands arrives after closure, gets sequence>watermark, and parks.
func TestUVMBaselineBarrierROBToGate(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.captureBlockAck()
	h.deliverBlockRange(11, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("the barrier must ack with no in-gate request")
	}
	if h.blockAck.Watermark != 0 {
		t.Fatalf("the watermark must be 0 with no admitted request, got %d",
			h.blockAck.Watermark)
	}

	h.admit(readReq(0x1000), false)
	if len(h.comp.transactions) != 0 {
		t.Fatal("the post-barrier request must not be translated")
	}
	if len(h.comp.activeBlocks) != 1 || len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatalf("the post-barrier request must park, got %+v", h.comp.activeBlocks)
	}
	if h.comp.activeBlocks[0].parked[0].sequence <= h.blockAck.Watermark {
		t.Fatal("the post-barrier request must have sequence above the watermark")
	}
}

// TestUVMBaselineBarrierGateToCache proves a request between the gate and the
// cache when the barrier lands delays the ack until it is downstream-visible.
func TestUVMBaselineBarrierGateToCache(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(readReq(0x1000), true)
	if len(h.comp.transactions) != 1 {
		t.Fatal("the request must be in flight at the gate->cache boundary")
	}

	h.deliverBlockRange(12, 1, 0x1000, 0x10000)
	if h.comp.activeBlocks[0].acked {
		t.Fatal("the ack must wait for the in-flight request to become downstream-visible")
	}
	if h.comp.activeBlocks[0].pendingDisposals != 1 {
		t.Fatalf("the in-flight request must be counted, got %d",
			h.comp.activeBlocks[0].pendingDisposals)
	}

	h.captureBlockAck()
	h.deliverTranslation(vm.MemoryLocationGPU_LOCAL, 0x60000, 0, true)
	if h.translated == nil {
		t.Fatal("the in-flight request must complete downstream")
	}
	if h.blockAck == nil {
		t.Fatal("the ack must fire once the request is downstream-visible")
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}
}

// TestUVMBaselineBarrierCacheMSHR proves a request retained for a fault
// (cache MSHR / GMMU replay record) when the barrier lands counts as disposed
// and does not delay the ack.
func TestUVMBaselineBarrierCacheMSHR(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(readReq(0x1000), true)
	h.deliverTranslation(vm.MemoryLocationINVALID, 0, 1, false)
	if len(h.comp.transactions) != 1 {
		t.Fatal("the faulted request must be retained in the gate")
	}
	if !h.comp.transactions[0].disposed {
		t.Fatal("the retained request must be marked disposed")
	}

	h.captureBlockAck()
	h.deliverBlockRange(13, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("a retained request must not delay the ack")
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}

	h.admit(readReq(0x1000), false)
	if len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatal("the post-barrier request must park")
	}
}

// TestUVMBaselineBarrierRemotePrePost proves an old remote read committed
// before the barrier counts as disposed, while a remote read arriving after
// the barrier parks and never uses the old mapping.
func TestUVMBaselineBarrierRemotePrePost(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(readReq(0x1000), true)
	h.deliverTranslation(vm.MemoryLocationCPU_REMOTE, 0x50000, 0, false)
	if h.translated != nil {
		t.Fatal("a remote read must never be inserted into the GPU data cache")
	}
	if len(h.comp.transactions) != 0 {
		t.Fatal("the old remote read must be committed to the endpoint")
	}

	h.captureBlockAck()
	h.deliverBlockRange(14, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("an old remote read committed to the endpoint must not delay the ack")
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}

	h.admit(readReq(0x1000), false)
	if len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatal("the post-barrier remote read must park")
	}

	// After the transition the mapping is GPU_LOCAL; the unblocked read uses
	// the new mapping.
	h.transMapper.EXPECT().Find(uint64(0x1000)).
		Return(h.translationPort.AsRemote())
	h.translationPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(r *vm.TranslationReq) *sim.SendError {
			h.transReq = r
			return nil
		})
	h.deliverUnblockRange(14, 1, 0x1000, 0x10000)
	if len(h.comp.transactions) != 1 {
		t.Fatalf("the parked read must be released, got %d",
			len(h.comp.transactions))
	}
	h.deliverTranslation(vm.MemoryLocationGPU_LOCAL, 0x60000, 0, true)
	if h.translated == nil {
		t.Fatal("the released read must complete with the new mapping")
	}
}

// TestUVMBaselineUnknownLocation proves an unknown MemoryLocation fails.
func TestUVMBaselineUnknownLocation(t *testing.T) {
	h := newUVMGateHarness(t, 2)
	defer h.ctrl.Finish()

	h.admit(readReq(0x1000), true)
	trans := h.comp.transactions[0]
	rsp := vm.TranslationRspBuilder{}.
		WithRspTo(trans.translationReq.ID).
		WithPage(vm.Page{
			PID:      1,
			VAddr:    trans.translationReq.VAddr,
			PAddr:    0x60000,
			Valid:    true,
			Managed:  true,
			Location: vm.MemoryLocation(99),
			PageSize: 4096,
		}).
		Build()
	h.translationPort.EXPECT().PeekIncoming().Return(rsp)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("an unknown location must fail")
		}
	}()
	h.mw.parseTranslation()
}
