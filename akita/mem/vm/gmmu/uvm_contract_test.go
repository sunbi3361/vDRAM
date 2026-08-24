package gmmu

// sbin_codex: UVM fault-ownership, block/watermark gate, and replay contract
// tests (plan todo 8 of mgpusim-uvm-manager). These plain Go tests drive the
// GMMU middleware with the package mocks and assert: managed faults are owned
// by the GMMU (records + typed faults, no panic), FIFO fault service with
// 64 KB region coalescing, waiter-delta propagation, range replay with
// backpressure, the BlockRange/watermark/ack protocol, and the barrier races
// at ROB→gate, gate→cache, cache MSHR, and remote read.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/pagewalkcache"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// uvmGMMUHarness builds a GMMU with mocked ports so the middleware can be
// driven deterministically.
type uvmGMMUHarness struct {
	ctrl        *gomock.Controller
	comp        *Comp
	mw          *middleware
	ctrlMw      *ctrlMiddleware
	topPort     *MockPort
	controlPort *MockPort
	cpPort      *MockPort
	pwcPort     *MockPort
	pageTable   vm.PageTable
	blockAck    *vm.BlockAck
	replayAck   *vm.ReplayAck
	faultNotif  *vm.FaultNotification
	faultRsp    *vm.TranslationRsp
}

func newUVMGMMUHarness(t *testing.T, maxInFlight int) *uvmGMMUHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)

	topPort := NewMockPort(ctrl)
	topPort.EXPECT().AsRemote().Return(sim.RemotePort("TopPort")).AnyTimes()
	topPort.EXPECT().Name().Return("TopPort").AnyTimes()
	controlPort := NewMockPort(ctrl)
	controlPort.EXPECT().AsRemote().Return(sim.RemotePort("ControlPort")).AnyTimes()
	controlPort.EXPECT().Name().Return("ControlPort").AnyTimes()
	cpPort := NewMockPort(ctrl)
	cpPort.EXPECT().AsRemote().Return(sim.RemotePort("CPPort")).AnyTimes()
	cpPort.EXPECT().Name().Return("CPPort").AnyTimes()
	pwcPort := NewMockPort(ctrl)
	pwcPort.EXPECT().AsRemote().Return(sim.RemotePort("PWCPort")).AnyTimes()
	pwcPort.EXPECT().Name().Return("PWCPort").AnyTimes()

	comp := &Comp{
		latency:            1,
		maxRequestsInFlight: maxInFlight,
		state:              gmmuStateEnable,
		deviceID:           1,
		gate:               gateState{gateID: TranslationGateID},
		pageTable:          vm.NewPageTable(12),
		pendingRegions:     make(map[uint64]bool),
		regionReplayTokens: make(map[uint64]vm.ReplayToken),
	}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUTest", engine, sim.GHz, nil)
	comp.topPort = topPort
	comp.controlPort = controlPort
	comp.commandProcessor = cpPort
	comp.pageWalkCachePort = pwcPort
	comp.pageWalkCache = pwcPort

	mw := &middleware{Comp: comp}
	ctrlMw := &ctrlMiddleware{Comp: comp}

	return &uvmGMMUHarness{
		ctrl:        ctrl,
		comp:        comp,
		mw:          mw,
		ctrlMw:      ctrlMw,
		topPort:     topPort,
		controlPort: controlPort,
		cpPort:      cpPort,
		pwcPort:     pwcPort,
		pageTable:   comp.pageTable,
	}
}

// admit injects a translation request into the GMMU top port.
func (h *uvmGMMUHarness) admit(req *vm.TranslationReq) {
	h.topPort.EXPECT().RetrieveIncoming().Return(req)
	h.mw.parseFromTop()
}

// walkToFinalize fast-forwards the first walking transaction to the
// pageWalkComplete state (the finalize step still needs a walkPageTable call).
func (h *uvmGMMUHarness) walkToFinalize() {
	h.pwcPort.EXPECT().CanSend().Return(true)
	h.pwcPort.EXPECT().Send(gomock.Any()).Return(nil)
	h.mw.walkPageTable()

	h.pwcPort.EXPECT().PeekIncoming().Return(&pagewalkcache.LookupRsp{
		RspTo: h.mw.walkingTranslations[0].msgID,
		Level: -1,
	})
	h.pwcPort.EXPECT().RetrieveIncoming()
	h.mw.parseFromPageWalkCache()

	h.mw.walkingTranslations[0].cycleLeft = 0
	h.mw.walkingTranslations[0].fillLevel = -1
	h.mw.walkPageTable()
}

// finalizeHit completes the walk with a normal response to the top. The
// captured response is exposed on the harness.
func (h *uvmGMMUHarness) finalizeHit() *vm.TranslationRsp {
	var rsp *vm.TranslationRsp
	h.topPort.EXPECT().CanSend().Return(true)
	h.topPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(r *vm.TranslationRsp) error {
			rsp = r
			h.faultRsp = r
			return nil
		})
	h.mw.walkPageTable()
	return rsp
}

// finalizeFault completes the walk as a managed fault: a fault notification
// to the CP and a fault-pending response to the top. The captured messages
// are exposed on the harness.
func (h *uvmGMMUHarness) finalizeFault() {
	h.faultNotif = nil
	h.faultRsp = nil
	h.cpPort.EXPECT().CanSend().Return(true)
	h.cpPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(n *vm.FaultNotification) error {
			h.faultNotif = n
			return nil
		})
	h.topPort.EXPECT().CanSend().Return(true)
	h.topPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(r *vm.TranslationRsp) error {
			h.faultRsp = r
			return nil
		})
	h.mw.walkPageTable()
}

// finalizeCoalescedFault completes the walk as a fault coalesced onto an
// already-notified 64 KB region: only the fault-pending response is sent.
func (h *uvmGMMUHarness) finalizeCoalescedFault() {
	h.faultNotif = nil
	h.faultRsp = nil
	h.topPort.EXPECT().CanSend().Return(true)
	h.topPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(r *vm.TranslationRsp) error {
			h.faultRsp = r
			return nil
		})
	h.mw.walkPageTable()
}

// deliverBlockRange injects a BlockRange into the control port.
func (h *uvmGMMUHarness) deliverBlockRange(
	cmdID uint64, pid vm.PID, startVA, size uint64,
) *vm.BlockRange {
	msg := &vm.BlockRange{CommandID: cmdID, PID: pid, StartVA: startVA, Size: size}
	msg.ID = sim.GetIDGenerator().Generate()
	msg.Src = sim.RemotePort("CP")
	msg.Dst = sim.RemotePort("ControlPort")
	h.controlPort.EXPECT().PeekIncoming().Return(msg)
	h.controlPort.EXPECT().RetrieveIncoming()
	h.ctrlMw.handleIncomingCommands()
	return msg
}

// deliverUnblockRange injects an UnblockRange into the control port.
func (h *uvmGMMUHarness) deliverUnblockRange(
	cmdID uint64, pid vm.PID, startVA, size uint64,
) *vm.UnblockRange {
	msg := &vm.UnblockRange{CommandID: cmdID, PID: pid, StartVA: startVA, Size: size}
	msg.ID = sim.GetIDGenerator().Generate()
	msg.Src = sim.RemotePort("CP")
	msg.Dst = sim.RemotePort("ControlPort")
	h.controlPort.EXPECT().PeekIncoming().Return(msg)
	h.controlPort.EXPECT().RetrieveIncoming()
	h.controlPort.EXPECT().CanSend().Return(true)
	h.controlPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(a *vm.UnblockAck) error {
			return nil
		})
	h.ctrlMw.handleIncomingCommands()
	return msg
}

// captureBlockAck expects the next BlockAck send on the control port. The
// captured ack is exposed on the harness.
func (h *uvmGMMUHarness) captureBlockAck() {
	h.blockAck = nil
	h.controlPort.EXPECT().CanSend().Return(true)
	h.controlPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(a *vm.BlockAck) error {
			h.blockAck = a
			return nil
		})
}

// deliverReplayRange injects a ReplayRange into the command-processor port.
func (h *uvmGMMUHarness) deliverReplayRange(
	pid vm.PID, startVA, size uint64, token vm.ReplayToken,
) *vm.ReplayRange {
	msg := &vm.ReplayRange{PID: pid, GPU: 1, StartVA: startVA, Size: size, ReplayToken: token}
	msg.ID = sim.GetIDGenerator().Generate()
	msg.Src = sim.RemotePort("CP")
	msg.Dst = sim.RemotePort("CPPort")
	h.cpPort.EXPECT().PeekIncoming().Return(msg)
	h.cpPort.EXPECT().RetrieveIncoming()
	h.mw.handleReplayRange()
	return msg
}

// captureReplayAck expects the next ReplayAck send on the command-processor
// port. The captured ack is exposed on the harness.
func (h *uvmGMMUHarness) captureReplayAck() {
	h.replayAck = nil
	h.cpPort.EXPECT().CanSend().Return(true)
	h.cpPort.EXPECT().Send(gomock.Any()).
		DoAndReturn(func(a *vm.ReplayAck) error {
			h.replayAck = a
			return nil
		})
}

func managedInvalidPage(vAddr uint64) vm.Page {
	return vm.Page{
		PID:      1,
		VAddr:    vAddr,
		PAddr:    0,
		Valid:    false,
		Managed:  true,
		Location: vm.MemoryLocationINVALID,
		PageSize: 4096,
	}
}

func managedRemotePage(vAddr uint64) vm.Page {
	return vm.Page{
		PID:      1,
		VAddr:    vAddr,
		PAddr:    0x50000,
		Valid:    true,
		Managed:  true,
		Location: vm.MemoryLocationCPU_REMOTE,
		PageSize: 4096,
	}
}

func managedLocalPage(vAddr uint64) vm.Page {
	return vm.Page{
		PID:      1,
		VAddr:    vAddr,
		PAddr:    0x60000,
		Valid:    true,
		Managed:  true,
		Location: vm.MemoryLocationGPU_LOCAL,
		PageSize: 4096,
	}
}

func unmanagedPage(vAddr uint64) vm.Page {
	return vm.Page{
		PID:      1,
		VAddr:    vAddr,
		PAddr:    0x70000,
		Valid:    true,
		PageSize: 4096,
	}
}

func translationReq(src string, vAddr uint64, kind vm.AccessKind) *vm.TranslationReq {
	return vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort(src)).
		WithPID(1).
		WithVAddr(vAddr).
		WithDeviceID(1).
		WithAccessKind(kind).
		Build()
}

// TestUVMFaultOwnership proves the GMMU detects managed invalid pages as
// typed faults: it owns the replay record, assigns a fault-pending token,
// returns a fault-pending response, and notifies the CP — never panicking.
func TestUVMFaultOwnership(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(managedInvalidPage(0x1000))

	req := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req)
	h.walkToFinalize()
	h.finalizeFault()

	if h.faultNotif == nil {
		t.Fatal("the GMMU must send a typed fault notification to the CP")
	}
	if h.faultNotif.PID != 1 || h.faultNotif.VAddr != 0x1000 || h.faultNotif.GPU != 1 {
		t.Fatalf("the notification must carry PID/GPU/VAddr, got %+v", h.faultNotif)
	}
	if h.faultNotif.AccessKind != vm.AccessKindRead {
		t.Fatalf("the notification must carry the access kind, got %v", h.faultNotif.AccessKind)
	}
	if h.faultNotif.FaultPendingToken == 0 {
		t.Fatal("the notification must carry a GMMU-assigned fault-pending token")
	}
	if h.faultRsp == nil {
		t.Fatal("the GMMU must return a fault-pending response to the top")
	}
	if h.faultRsp.FaultPendingToken != h.faultNotif.FaultPendingToken {
		t.Fatalf("the response must echo the notification token %d, got %d",
			h.faultNotif.FaultPendingToken, h.faultRsp.FaultPendingToken)
	}
	if h.faultRsp.Page.Valid {
		t.Fatal("the fault-pending response must carry the invalid managed page")
	}
	if len(h.comp.faultRecords) != 1 {
		t.Fatalf("the GMMU must own exactly one replay record, got %d",
			len(h.comp.faultRecords))
	}
	rec := h.comp.faultRecords[0]
	if rec.token != h.faultNotif.FaultPendingToken {
		t.Fatalf("the record must carry the assigned token, got %d", rec.token)
	}
	if rec.vAddr != 0x1000 || rec.pid != 1 {
		t.Fatalf("the record must carry the faulting request, got %+v", rec)
	}
	if rec.replayToken == 0 {
		t.Fatal("the record must carry the region replay token")
	}

	// Managed hits and unmanaged pages never fault.
	h.pageTable.Insert(managedLocalPage(0x2000))
	h.pageTable.Insert(unmanagedPage(0x3000))
	for _, vAddr := range []uint64{0x2000, 0x3000} {
		req := translationReq("L2", vAddr, vm.AccessKindRead)
		h.admit(req)
		h.walkToFinalize()
		rsp := h.finalizeHit()
		if rsp == nil || !rsp.Page.Valid {
			t.Fatalf("vAddr %#x must translate normally, got %+v", vAddr, rsp)
		}
	}
	if len(h.comp.faultRecords) != 1 {
		t.Fatalf("hits must not create replay records, got %d",
			len(h.comp.faultRecords))
	}
}

// TestUVMFIFOService proves faults are recorded FIFO by token and coalesced
// per 64 KB region: one notification per region, records in creation order.
func TestUVMFIFOService(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(managedInvalidPage(0x1000))
	h.pageTable.Insert(managedInvalidPage(0x2000)) // same 64 KB region
	h.pageTable.Insert(managedInvalidPage(0x10000))

	// First fault in region A: notification + record.
	req1 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req1)
	h.walkToFinalize()
	h.finalizeFault()
	if h.faultNotif == nil {
		t.Fatal("the first region-A fault must notify the CP")
	}
	firstToken := h.faultNotif.FaultPendingToken

	// Second fault in the same region: coalesced — record but no notification.
	req2 := translationReq("L2", 0x2000, vm.AccessKindWrite)
	h.admit(req2)
	h.walkToFinalize()
	h.finalizeCoalescedFault()
	if h.faultNotif != nil {
		t.Fatal("a duplicate-region fault must not notify the CP again")
	}
	if len(h.comp.faultRecords) != 2 {
		t.Fatalf("the coalesced fault must still own a record, got %d",
			len(h.comp.faultRecords))
	}
	if h.comp.faultRecords[0].token >= h.comp.faultRecords[1].token {
		t.Fatal("records must be FIFO by token")
	}
	if h.comp.faultRecords[0].regionBase != h.comp.faultRecords[1].regionBase {
		t.Fatal("both records must share the 64 KB region base")
	}

	// Third fault in a different region: new notification, FIFO after the
	// first.
	req3 := translationReq("L2", 0x10000, vm.AccessKindRead)
	h.admit(req3)
	h.walkToFinalize()
	h.finalizeFault()
	if h.faultNotif == nil {
		t.Fatal("a new-region fault must notify the CP")
	}
	if h.faultNotif.FaultPendingToken <= firstToken {
		t.Fatal("notifications must be FIFO by token")
	}
	if len(h.comp.faultRecords) != 3 {
		t.Fatalf("expected three records, got %d", len(h.comp.faultRecords))
	}
}

// TestUVMLateWaiterDelta proves the GMMU records the leaf waiter delta on the
// fault record and echoes it in the fault-pending response.
func TestUVMLateWaiterDelta(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(managedInvalidPage(0x1000))

	req := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("L2")).
		WithPID(1).
		WithVAddr(0x1000).
		WithDeviceID(1).
		WithAccessKind(vm.AccessKindRead).
		WithWaiterDelta(vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 2}).
		Build()
	h.admit(req)
	h.walkToFinalize()
	h.finalizeFault()

	want := vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 2}
	if h.faultRsp.WaiterDelta != want {
		t.Fatalf("the fault-pending response must echo the leaf delta, got %+v",
			h.faultRsp.WaiterDelta)
	}
	if h.comp.faultRecords[0].waiterDelta != want {
		t.Fatalf("the record must retain the leaf delta, got %+v",
			h.comp.faultRecords[0].waiterDelta)
	}
}

// TestUVMReplay proves the GMMU replays the records for a serviced range:
// re-running translation with the new mapping, retiring the records, and
// acknowledging the replay command. Backpressure bounds the re-injection.
func TestUVMReplay(t *testing.T) {
	h := newUVMGMMUHarness(t, 1) // one walk slot: backpressure
	defer h.ctrl.Finish()
	h.pageTable.Insert(managedInvalidPage(0x1000))
	h.pageTable.Insert(managedInvalidPage(0x2000)) // same region

	// Two faults in the same region.
	for _, vAddr := range []uint64{0x1000, 0x2000} {
		req := translationReq("L2", vAddr, vm.AccessKindRead)
		h.admit(req)
		h.walkToFinalize()
		if vAddr == 0x1000 {
			h.finalizeFault()
			if h.faultNotif == nil {
				t.Fatal("the first fault must notify the CP")
			}
		} else {
			h.finalizeCoalescedFault()
		}
	}
	replayToken := h.comp.faultRecords[0].replayToken
	if len(h.comp.faultRecords) != 2 {
		t.Fatalf("expected two records, got %d", len(h.comp.faultRecords))
	}

	// Service completes: the region becomes GPU_LOCAL.
	h.pageTable.Update(managedLocalPage(0x1000))
	h.pageTable.Update(managedLocalPage(0x2000))

	// Replay with backpressure: only the first record is re-injected while
	// the single walk slot is occupied.
	h.deliverReplayRange(1, 0x1000, 0x10000, replayToken)
	if len(h.mw.walkingTranslations) != 1 {
		t.Fatalf("backpressure must re-inject one walk at a time, got %d",
			len(h.mw.walkingTranslations))
	}
	if h.comp.activeReplay == nil || len(h.comp.activeReplay.pending) != 1 {
		t.Fatalf("the second record must stay pending, got %+v",
			h.comp.activeReplay)
	}

	// The first re-walk completes with the new mapping.
	h.walkToFinalize()
	rsp1 := h.finalizeHit()
	if rsp1 == nil || !rsp1.Page.Valid ||
		rsp1.Page.Location != vm.MemoryLocationGPU_LOCAL {
		t.Fatalf("the replay must use the new mapping, got %+v", h.faultRsp)
	}
	if len(h.comp.faultRecords) != 1 {
		t.Fatalf("the completed record must be retired, got %d",
			len(h.comp.faultRecords))
	}

	// The second record is now re-injected and completes.
	h.mw.handleReplayTick()
	h.walkToFinalize()
	h.captureReplayAck()
	h.finalizeHit()
	if h.faultRsp == nil || !h.faultRsp.Page.Valid {
		t.Fatalf("the second replay must translate, got %+v", h.faultRsp)
	}
	if h.replayAck == nil {
		t.Fatal("the replay command must be acknowledged after all records complete")
	}
	if h.replayAck.ReplayToken != replayToken {
		t.Fatalf("the replay ack must echo the token, got %d", h.replayAck.ReplayToken)
	}
	if len(h.comp.faultRecords) != 0 {
		t.Fatalf("all replayed records must be retired, got %d",
			len(h.comp.faultRecords))
	}
	if h.comp.activeReplay != nil {
		t.Fatal("the replay command must be cleared")
	}
}

// TestUVMBlockUnblock proves the block/unblock protocol: BlockRange closes
// matching admission, snapshots the watermark, and acks; requests arriving
// after closure park; UnblockRange releases them and acks.
func TestUVMBlockUnblock(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(unmanagedPage(0x1000))

	// A request admitted and completed before the block.
	req1 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req1)
	h.walkToFinalize()
	h.finalizeHit()
	if h.comp.gate.lastAssignedSequence != 1 {
		t.Fatalf("the first admission must get sequence 1, got %d",
			h.comp.gate.lastAssignedSequence)
	}

	// Block: watermark = 1, no matching walking request, ack immediately.
	h.captureBlockAck()
	h.deliverBlockRange(100, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("the block must be acknowledged once all earlier requests are disposed")
	}
	if h.blockAck.CommandID != 100 || h.blockAck.GateID != TranslationGateID || h.blockAck.Watermark != 1 {
		t.Fatalf("the ack must carry {commandID, gateID, watermark}, got %+v", h.blockAck)
	}

	// A request arriving after closure parks: no walk, no response.
	req2 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req2)
	if len(h.mw.walkingTranslations) != 0 {
		t.Fatal("a post-closure request must not be walked")
	}
	if len(h.comp.activeBlocks) != 1 || len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatalf("the post-closure request must park, got %+v", h.comp.activeBlocks)
	}
	if h.comp.activeBlocks[0].parked[0].sequence <= h.blockAck.Watermark {
		t.Fatal("a post-closure request must get a sequence above the watermark")
	}

	// Unblock releases the parked request, which now walks and completes.
	h.deliverUnblockRange(100, 1, 0x1000, 0x10000)
	if len(h.comp.activeBlocks) != 0 {
		t.Fatal("the block must be removed after unblock")
	}
	if len(h.mw.walkingTranslations) != 1 {
		t.Fatalf("the parked request must be released, got %d",
			len(h.mw.walkingTranslations))
	}
	h.walkToFinalize()
	rsp := h.finalizeHit()
	if rsp == nil || !rsp.Page.Valid {
		t.Fatalf("the released request must translate, got %+v", rsp)
	}
}

// TestUVMGateLocalWatermark proves each gate snapshots its local watermark
// and the ack waits for every matching request with sequence<=watermark to be
// disposed.
func TestUVMGateLocalWatermark(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(unmanagedPage(0x1000))

	// Two admitted requests, both still walking when the block arrives. Both
	// are parked at the lookup-sent stage; only the first receives its
	// page-walk-cache response before the block.
	req1 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req1)
	h.pwcPort.EXPECT().CanSend().Return(true)
	h.pwcPort.EXPECT().Send(gomock.Any()).Return(nil)
	h.mw.walkPageTable()
	req2 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req2)
	h.pwcPort.EXPECT().CanSend().Return(true)
	h.pwcPort.EXPECT().Send(gomock.Any()).Return(nil)
	h.mw.walkPageTable()
	if h.comp.gate.lastAssignedSequence != 2 {
		t.Fatalf("expected watermark base 2, got %d",
			h.comp.gate.lastAssignedSequence)
	}

	// The first request advances to the finalize step.
	h.pwcPort.EXPECT().PeekIncoming().Return(&pagewalkcache.LookupRsp{
		RspTo: h.mw.walkingTranslations[0].msgID,
		Level: -1,
	})
	h.pwcPort.EXPECT().RetrieveIncoming()
	h.mw.parseFromPageWalkCache()
	h.mw.walkingTranslations[0].cycleLeft = 0
	h.mw.walkingTranslations[0].fillLevel = -1
	h.mw.walkPageTable()

	// Block arrives: watermark = 2, both requests still walking → no ack.
	h.controlPort.EXPECT().PeekIncoming().Return(&vm.BlockRange{
		CommandID: 7, PID: 1, StartVA: 0x1000, Size: 0x10000,
	})
	h.controlPort.EXPECT().RetrieveIncoming()
	h.ctrlMw.handleIncomingCommands()
	if len(h.comp.activeBlocks) != 1 {
		t.Fatal("the block must be registered")
	}
	if h.comp.activeBlocks[0].watermark != 2 {
		t.Fatalf("the watermark must snapshot the last assigned sequence, got %d",
			h.comp.activeBlocks[0].watermark)
	}

	// The first request completes: still no ack (the second is walking).
	h.finalizeHit()
	if h.comp.activeBlocks[0].acked {
		t.Fatal("the ack must wait for every matching request with sequence<=watermark")
	}

	// The second completes: the ack fires with the snapshot watermark.
	h.pwcPort.EXPECT().PeekIncoming().Return(&pagewalkcache.LookupRsp{
		RspTo: h.mw.walkingTranslations[0].msgID,
		Level: -1,
	})
	h.pwcPort.EXPECT().RetrieveIncoming()
	h.mw.parseFromPageWalkCache()
	h.mw.walkingTranslations[0].cycleLeft = 0
	h.mw.walkingTranslations[0].fillLevel = -1
	h.mw.walkPageTable()
	h.captureBlockAck()
	h.finalizeHit()
	if h.blockAck == nil {
		t.Fatal("the ack must fire after the last matching request is disposed")
	}
	if h.blockAck.Watermark != 2 {
		t.Fatalf("the ack must carry the snapshot watermark 2, got %d", h.blockAck.Watermark)
	}
	if !h.comp.activeBlocks[0].acked {
		t.Fatal("the block must be marked acked")
	}
}

// TestUVMBarrierROBToGate proves a request still in the ROB (not yet at the
// gate) when the barrier lands arrives after closure, gets sequence>watermark,
// and parks — the ack fires without it.
func TestUVMBarrierROBToGate(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(unmanagedPage(0x1000))

	// Barrier first: no request has reached the gate.
	h.captureBlockAck()
	h.deliverBlockRange(11, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("the barrier must ack with no in-gate request")
	}
	if h.blockAck.Watermark != 0 {
		t.Fatalf("the watermark must be 0 with no admitted request, got %d",
			h.blockAck.Watermark)
	}

	// The ROB request arrives after closure: parked, never walked.
	req := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req)
	if len(h.mw.walkingTranslations) != 0 {
		t.Fatal("the post-barrier request must not walk")
	}
	if len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatal("the post-barrier request must park")
	}
	if h.comp.activeBlocks[0].parked[0].sequence <= h.blockAck.Watermark {
		t.Fatal("the post-barrier request must have sequence above the watermark")
	}

	// Unblock releases it with the new mapping.
	h.pageTable.Update(managedLocalPage(0x1000))
	h.deliverUnblockRange(11, 1, 0x1000, 0x10000)
	h.walkToFinalize()
	rsp := h.finalizeHit()
	if rsp == nil || rsp.Page.Location != vm.MemoryLocationGPU_LOCAL {
		t.Fatalf("the released request must use the new mapping, got %+v", rsp)
	}
}

// TestUVMBarrierGateToCache proves a request between the gate and the cache
// (walk complete, response not yet sent) when the barrier lands delays the
// ack until the response is downstream-visible.
func TestUVMBarrierGateToCache(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(unmanagedPage(0x1000))

	// The request is admitted and walked to the finalize step.
	req := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req)
	h.walkToFinalize()
	if len(h.mw.walkingTranslations) != 1 {
		t.Fatal("the request must still be in flight at the gate->cache boundary")
	}

	// The barrier lands while the response is not yet sent.
	h.controlPort.EXPECT().PeekIncoming().Return(&vm.BlockRange{
		CommandID: 12, PID: 1, StartVA: 0x1000, Size: 0x10000,
	})
	h.controlPort.EXPECT().RetrieveIncoming()
	h.ctrlMw.handleIncomingCommands()
	if h.comp.activeBlocks[0].acked {
		t.Fatal("the ack must wait for the in-flight request to become downstream-visible")
	}

	// The response is sent: the request is downstream-visible, ack fires.
	h.captureBlockAck()
	h.finalizeHit()
	if h.faultRsp == nil {
		t.Fatal("the in-flight request must complete downstream")
	}
	if h.blockAck == nil {
		t.Fatal("the ack must fire once the request is downstream-visible")
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}
}

// TestUVMBarrierCacheMSHR proves a request retained in the cache MSHR
// (fault-pending record) when the barrier lands counts as retained and does
// not delay the ack.
func TestUVMBarrierCacheMSHR(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(managedInvalidPage(0x1000))

	// The request faults and is retained in the GMMU replay record.
	req := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req)
	h.walkToFinalize()
	h.finalizeFault()
	if h.faultRsp == nil {
		t.Fatal("the fault must return a fault-pending response")
	}
	if len(h.comp.faultRecords) != 1 {
		t.Fatal("the request must be retained in the GMMU replay records")
	}

	// The barrier lands: the retained request counts as disposed.
	h.captureBlockAck()
	h.deliverBlockRange(13, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("a retained request must not delay the ack")
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}

	// A post-barrier request to the same range parks.
	req2 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req2)
	if len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatal("the post-barrier request must park")
	}
}

// TestUVMBarrierRemoteRead proves an old remote read committed before the
// barrier counts as disposed, while a remote read arriving after the barrier
// parks and never uses the old mapping.
func TestUVMBarrierRemoteRead(t *testing.T) {
	h := newUVMGMMUHarness(t, 16)
	defer h.ctrl.Finish()
	h.pageTable.Insert(managedRemotePage(0x1000))

	// The old remote read is admitted and its translation completes before
	// the barrier: committed to the endpoint.
	req1 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req1)
	h.walkToFinalize()
	rsp1 := h.finalizeHit()
	if rsp1 == nil || rsp1.Page.Location != vm.MemoryLocationCPU_REMOTE {
		t.Fatalf("the remote read must translate to the CPU mapping, got %+v", h.faultRsp)
	}

	// The barrier acks immediately: the old remote read is committed.
	h.captureBlockAck()
	h.deliverBlockRange(14, 1, 0x1000, 0x10000)
	if h.blockAck == nil {
		t.Fatal("an old remote read committed to the endpoint must not delay the ack")
	}
	if h.blockAck.Watermark != 1 {
		t.Fatalf("the watermark must be 1, got %d", h.blockAck.Watermark)
	}

	// A remote read arriving after the barrier parks: no old-mapping use.
	req2 := translationReq("L2", 0x1000, vm.AccessKindRead)
	h.admit(req2)
	if len(h.mw.walkingTranslations) != 0 {
		t.Fatal("the post-barrier remote read must not walk")
	}
	if len(h.comp.activeBlocks[0].parked) != 1 {
		t.Fatal("the post-barrier remote read must park")
	}

	// After the transition the mapping is GPU_LOCAL; the unblocked read uses
	// the new mapping.
	h.pageTable.Update(managedLocalPage(0x1000))
	h.deliverUnblockRange(14, 1, 0x1000, 0x10000)
	h.walkToFinalize()
	rsp2 := h.finalizeHit()
	if rsp2 == nil || rsp2.Page.Location != vm.MemoryLocationGPU_LOCAL {
		t.Fatalf("no post-ack request may use the old mapping, got %+v", h.faultRsp)
	}
}