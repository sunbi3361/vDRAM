package cp

// sbin_codex: CP UVM routing middleware contract tests (plan todo 12 of
// mgpusim-uvm-manager). These plain Go tests drive the uvmMiddleware with the
// package mocks and assert: fault notification routing, block/unblock gateID
// aggregation with strict ack validation, exact baseline/virtual TLB endpoint
// sets for range invalidation, data-cache-only fan-out (L1I excluded), the
// counter-reset kernel-dispatch barrier, overlapping migration DMA, and ack
// correlation across concurrent commands.

import (
	"fmt"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp/internal/dispatching"
	"go.uber.org/mock/gomock"
)

// uvmCPHarness builds a CommandProcessor whose UVM-relevant ports are mocks so
// the uvmMiddleware can be driven deterministically.
type uvmCPHarness struct {
	ctrl            *gomock.Controller
	cp              *CommandProcessor
	mw              *uvmMiddleware
	toDriver        *MockPort
	toDMA           *MockPort
	toGMMU          *MockPort
	toCaches        *MockPort
	toTLBs          *MockPort
	toAccessCounter *MockPort
	driver          *MockPort
	dmaEngine       *MockPort
}

func newUVMcpHarness(t *testing.T) *uvmCPHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)

	toDriver := NewMockPort(ctrl)
	toDMA := NewMockPort(ctrl)
	toGMMU := NewMockPort(ctrl)
	toCaches := NewMockPort(ctrl)
	toTLBs := NewMockPort(ctrl)
	toAccessCounter := NewMockPort(ctrl)
	driver := NewMockPort(ctrl)
	dmaEngine := NewMockPort(ctrl)

	toDriver.EXPECT().AsRemote().Return(sim.RemotePort("ToDriver")).AnyTimes()
	toDMA.EXPECT().AsRemote().Return(sim.RemotePort("ToDMA")).AnyTimes()
	toGMMU.EXPECT().AsRemote().Return(sim.RemotePort("ToGMMU")).AnyTimes()
	toCaches.EXPECT().AsRemote().Return(sim.RemotePort("ToCaches")).AnyTimes()
	toTLBs.EXPECT().AsRemote().Return(sim.RemotePort("ToTLBs")).AnyTimes()
	toAccessCounter.EXPECT().AsRemote().Return(sim.RemotePort("ToAccessCounter")).AnyTimes()
	driver.EXPECT().AsRemote().Return(sim.RemotePort("Driver")).AnyTimes()
	dmaEngine.EXPECT().AsRemote().Return(sim.RemotePort("DMAEngine")).AnyTimes()

	// Backpressure is not exercised by these fixtures; every forward port can
	// always accept a message.
	toGMMU.EXPECT().CanSend().Return(true).AnyTimes()
	toTLBs.EXPECT().CanSend().Return(true).AnyTimes()
	toCaches.EXPECT().CanSend().Return(true).AnyTimes()
	toAccessCounter.EXPECT().CanSend().Return(true).AnyTimes()

	cp := MakeBuilder().WithEngine(engine).WithFreq(1).Build("CP")
	cp.ToDriver = toDriver
	cp.ToDMA = toDMA
	cp.ToGMMU = toGMMU
	cp.ToCaches = toCaches
	cp.ToTLBs = toTLBs
	cp.ToAccessCounter = toAccessCounter
	cp.Driver = driver
	cp.DMAEngine = dmaEngine
	cp.GMMUControl = sim.RemotePort("GMMUControl")

	return &uvmCPHarness{
		ctrl:            ctrl,
		cp:              cp,
		mw:              cp.uvmMiddleware,
		toDriver:        toDriver,
		toDMA:           toDMA,
		toGMMU:          toGMMU,
		toCaches:        toCaches,
		toTLBs:          toTLBs,
		toAccessCounter: toAccessCounter,
		driver:          driver,
		dmaEngine:       dmaEngine,
	}
}

// mockTLBPort builds a MockPort whose remote is the given name.
func mockTLBPort(ctrl *gomock.Controller, name string) *MockPort {
	p := NewMockPort(ctrl)
	p.EXPECT().AsRemote().Return(sim.RemotePort(name)).AnyTimes()
	return p
}

func TestUVMRouteFault(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	notif := &vm.FaultNotification{
		PID:               1,
		GPU:               0,
		VAddr:             0x1000,
		AccessKind:        vm.AccessKindRead,
		FaultPendingToken: 7,
		ReplayToken:       3,
	}
	notif.ID = "fault-1"
	notif.Src = sim.RemotePort("ToGMMU")
	notif.Dst = sim.RemotePort("ToGMMU")

	var sent *protocol.PageFaultReq
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		sent = msg.(*protocol.PageFaultReq)
		return nil
	})
	h.toGMMU.EXPECT().RetrieveIncoming()

	if !h.mw.processFaultNotification(notif) {
		t.Fatal("a fault notification must be routed to the driver")
	}
	if sent == nil {
		t.Fatal("no PageFaultReq was sent to the driver")
	}
	if sent.PID != 1 || sent.GPU != 0 || sent.VAddr != 0x1000 ||
		sent.AccessType != vm.AccessKindRead || sent.FaultPendingToken != 7 {
		t.Fatalf("PageFaultReq fields mismatch: %+v", sent)
	}
}

func TestUVMBlockFanoutLocalWatermarks(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	h.cp.UVMGateIDs = []uint64{1, 2, 3}

	block := &vm.BlockRange{CommandID: 100, PID: 1, StartVA: 0x10000, Size: 64 * 1024}
	block.ID = "block-100"
	block.Src = sim.RemotePort("Driver")
	block.Dst = sim.RemotePort("ToDriver")

	var forwarded *vm.BlockRange
	h.toGMMU.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		forwarded = msg.(*vm.BlockRange)
		return nil
	})
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processBlockRange(block) {
		t.Fatal("a block range must be accepted")
	}
	if forwarded == nil || forwarded.CommandID != 100 {
		t.Fatalf("the block range must be forwarded to the GMMU: %+v", forwarded)
	}

	// One {commandID, gateID, watermark} ack per pre-registered gate, each
	// carrying its own local watermark. Completion fires only after the exact
	// gateID set is exhausted.
	acks := []*vm.BlockAck{
		{CommandID: 100, GateID: 1, Watermark: 5},
		{CommandID: 100, GateID: 2, Watermark: 7},
		{CommandID: 100, GateID: 3, Watermark: 9},
	}
	var completed *sim.GeneralRsp
	for i, ack := range acks {
		ack.ID = fmt.Sprintf("ack-%d", i)
		ack.Src = sim.RemotePort("ToGMMU")
		ack.Dst = sim.RemotePort("ToGMMU")
		h.toGMMU.EXPECT().RetrieveIncoming()
		if i == len(acks)-1 {
			h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
				completed = msg.(*sim.GeneralRsp)
				return nil
			})
		}
		if !h.mw.processBlockAck(ack) {
			t.Fatalf("block ack %d must be accepted", i)
		}
		if i < len(acks)-1 && completed != nil {
			t.Fatal("the block must not complete before the gateID set is exhausted")
		}
	}
	if completed == nil {
		t.Fatal("the block must complete after every gate ack")
	}
	if completed.OriginalReq != block {
		t.Fatal("the completion must reference the original block command")
	}
}

func TestUVMDuplicateUnknownWrongBlockAck(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	h.cp.UVMGateIDs = []uint64{1, 2, 3}

	block := &vm.BlockRange{CommandID: 200, PID: 1, StartVA: 0x10000, Size: 64 * 1024}
	block.ID = "block-200"
	block.Src = sim.RemotePort("Driver")
	block.Dst = sim.RemotePort("ToDriver")

	h.toGMMU.EXPECT().Send(gomock.Any()).Return(nil)
	h.toDriver.EXPECT().RetrieveIncoming()
	if !h.mw.processBlockRange(block) {
		t.Fatal("the block must be accepted")
	}

	// A wrong-command fixture: an unblock command whose command ID is distinct
	// from the block command, so a BlockAck for it must be rejected.
	unblock := &vm.UnblockRange{CommandID: 300, PID: 1, StartVA: 0x20000, Size: 64 * 1024}
	unblock.ID = "unblock-300"
	unblock.Src = sim.RemotePort("Driver")
	unblock.Dst = sim.RemotePort("ToDriver")
	h.toGMMU.EXPECT().Send(gomock.Any()).Return(nil)
	h.toDriver.EXPECT().RetrieveIncoming()
	if !h.mw.processUnblockRange(unblock) {
		t.Fatal("the unblock must be accepted")
	}

	// No completion may fire from any bad ack.
	h.toDriver.EXPECT().Send(gomock.Any()).Times(0)

	// Duplicate: gate 1 acked twice.
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 200, GateID: 1, Watermark: 3})
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 200, GateID: 1, Watermark: 3})

	// Unknown gate: gate 99 is not pre-registered.
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 200, GateID: 99, Watermark: 3})

	// Unknown command: no active block carries command ID 999.
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 999, GateID: 1, Watermark: 3})

	// Changed watermark: gate 2 acked with 5, then a second ack with 6.
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 200, GateID: 2, Watermark: 5})
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 200, GateID: 2, Watermark: 6})

	// Wrong command: a BlockAck for the unblock command ID 300.
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.mw.processBlockAck(&vm.BlockAck{CommandID: 300, GateID: 1, Watermark: 3})

	// The block must still be pending: gate 3 has not acked.
	if _, ok := h.cp.activeUVMBlocks[200]; !ok {
		t.Fatal("the block must remain pending after bad acks")
	}

	// The exact good ack set then completes it.
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.toDriver.EXPECT().Send(gomock.Any()).Return(nil)
	if !h.mw.processBlockAck(&vm.BlockAck{CommandID: 200, GateID: 3, Watermark: 11}) {
		t.Fatal("the final good ack must be accepted")
	}
	if _, ok := h.cp.activeUVMBlocks[200]; ok {
		t.Fatal("the block must complete after the exact gateID set is exhausted")
	}
}

func TestUVMBaselineTLBEndpointSet(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	// Baseline: private L1V/L1S/L1I TLBs plus the shared L2 TLB.
	l1v0 := mockTLBPort(h.ctrl, "L1V0")
	l1v1 := mockTLBPort(h.ctrl, "L1V1")
	l1s := mockTLBPort(h.ctrl, "L1S")
	l1i := mockTLBPort(h.ctrl, "L1I")
	l2 := mockTLBPort(h.ctrl, "L2")
	h.cp.TLBs = []sim.Port{l1v0, l1v1, l1s, l1i, l2}

	req := &protocol.UVMTLBInvalidateReq{PID: 1, StartVA: 0x10000, Size: 64 * 1024}
	req.ID = "inv-1"
	req.Src = sim.RemotePort("Driver")
	req.Dst = sim.RemotePort("ToDriver")

	sent := map[sim.RemotePort]bool{}
	h.toTLBs.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		inv := msg.(*tlb.UVMTLBInvalidateReq)
		sent[inv.Dst] = true
		return nil
	}).Times(5)
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processTLBInvalidateReq(req) {
		t.Fatal("the TLB invalidate must be accepted")
	}
	for _, want := range []sim.RemotePort{"L1V0", "L1V1", "L1S", "L1I", "L2"} {
		if !sent[want] {
			t.Fatalf("baseline fan-out must include %s, got %v", want, sent)
		}
	}
	if len(sent) != 5 {
		t.Fatalf("baseline fan-out must target exactly 5 endpoints, got %v", sent)
	}
}

func TestUVMVirtualTLBEndpointSet(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	// Virtual-caching: private L1I plus the shared L2 TLB only.
	l1i := mockTLBPort(h.ctrl, "L1I")
	l2 := mockTLBPort(h.ctrl, "L2")
	h.cp.TLBs = []sim.Port{l1i, l2}

	req := &protocol.UVMTLBInvalidateReq{PID: 1, StartVA: 0x10000, Size: 64 * 1024}
	req.ID = "inv-2"
	req.Src = sim.RemotePort("Driver")
	req.Dst = sim.RemotePort("ToDriver")

	sent := map[sim.RemotePort]bool{}
	h.toTLBs.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		inv := msg.(*tlb.UVMTLBInvalidateReq)
		sent[inv.Dst] = true
		return nil
	}).Times(2)
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processTLBInvalidateReq(req) {
		t.Fatal("the TLB invalidate must be accepted")
	}
	for _, want := range []sim.RemotePort{"L1I", "L2"} {
		if !sent[want] {
			t.Fatalf("virtual fan-out must include %s, got %v", want, sent)
		}
	}
	if len(sent) != 2 {
		t.Fatalf("virtual fan-out must target exactly 2 endpoints, got %v", sent)
	}
}

func TestUVMForbiddenVirtualL1DataTLB(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	// Virtual-caching has no fabricated L1V/L1S TLB endpoints: the fan-out may
	// only reach the private L1I and the shared L2 TLB.
	l1i := mockTLBPort(h.ctrl, "L1I")
	l2 := mockTLBPort(h.ctrl, "L2")
	h.cp.TLBs = []sim.Port{l1i, l2}

	req := &protocol.UVMTLBInvalidateReq{PID: 1, StartVA: 0x10000, Size: 64 * 1024}
	req.ID = "inv-3"
	req.Src = sim.RemotePort("Driver")
	req.Dst = sim.RemotePort("ToDriver")

	sent := map[sim.RemotePort]bool{}
	h.toTLBs.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		inv := msg.(*tlb.UVMTLBInvalidateReq)
		sent[inv.Dst] = true
		return nil
	}).Times(2)
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processTLBInvalidateReq(req) {
		t.Fatal("the TLB invalidate must be accepted")
	}
	for dst := range sent {
		if dst == "L1V0" || dst == "L1V1" || dst == "L1S" {
			t.Fatalf("virtual fan-out must not target a data TLB endpoint %s", dst)
		}
	}
	if len(sent) != 2 {
		t.Fatalf("virtual fan-out must target exactly L1I and L2, got %v", sent)
	}
}

func TestUVMCacheDataOnlyFanout(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	l1v := mockTLBPort(h.ctrl, "L1VCache")
	l1s := mockTLBPort(h.ctrl, "L1SCache")
	l1i := mockTLBPort(h.ctrl, "L1ICache")
	l2 := mockTLBPort(h.ctrl, "L2Cache")
	h.cp.L1VCaches = []sim.Port{l1v}
	h.cp.L1SCaches = []sim.Port{l1s}
	h.cp.L1ICaches = []sim.Port{l1i}
	h.cp.L2Caches = []sim.Port{l2}

	req := &protocol.UVMCacheRangeFlushReq{
		Operation:     cache.UVMCacheRangeFlushWritebackInvalidate,
		PID:           1,
		VABase:        0x10000,
		ValidPageMask: 0xFFFF,
	}
	req.ID = "flush-1"
	req.Src = sim.RemotePort("Driver")
	req.Dst = sim.RemotePort("ToDriver")

	sent := map[sim.RemotePort]bool{}
	h.toCaches.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		flush := msg.(*cache.UVMCacheRangeFlushReq)
		sent[flush.Dst] = true
		return nil
	}).Times(3)
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processCacheRangeFlushReq(req) {
		t.Fatal("the cache range flush must be accepted")
	}
	for _, want := range []sim.RemotePort{"L1VCache", "L1SCache", "L2Cache"} {
		if !sent[want] {
			t.Fatalf("data-cache fan-out must include %s, got %v", want, sent)
		}
	}
	if sent["L1ICache"] {
		t.Fatal("L1I must be excluded from data-cache WB+INV")
	}
	if len(sent) != 3 {
		t.Fatalf("data-cache fan-out must target exactly 3 endpoints, got %v", sent)
	}
}

func TestUVMCounterResetBarrier(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	reset := &CounterResetReq{}
	reset.ID = "reset-1"
	reset.Src = sim.RemotePort("Driver")
	reset.Dst = sim.RemotePort("ToDriver")

	var forwarded *CounterResetReq
	h.toAccessCounter.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		forwarded = msg.(*CounterResetReq)
		return nil
	})
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processCounterResetReq(reset) {
		t.Fatal("the counter reset must be accepted")
	}
	if forwarded == nil {
		t.Fatal("the reset must be forwarded to the GPU-wide AccessCounter")
	}
	if !h.cp.counterResetPending {
		t.Fatal("the reset barrier must be pending until the ack")
	}

	// A kernel launch must be held while the reset is pending.
	launch := &protocol.LaunchKernelReq{}
	launch.ID = "launch-1"
	if h.cp.middleware.processLaunchKernelReq(launch) {
		t.Fatal("kernel dispatch must be held while the counter reset is pending")
	}

	// The AccessCounter ack releases the barrier.
	h.toAccessCounter.EXPECT().RetrieveIncoming()
	if !h.mw.processCounterResetRsp(&CounterResetRsp{}) {
		t.Fatal("the counter reset ack must be accepted")
	}
	if h.cp.counterResetPending {
		t.Fatal("the counter reset barrier must clear after the ack")
	}

	// The held kernel launch may now dispatch.
	dispatcher := NewMockDispatcher(h.ctrl)
	h.cp.Dispatchers = []dispatching.Dispatcher{dispatcher}
	dispatcher.EXPECT().IsDispatching().Return(false)
	dispatcher.EXPECT().StartDispatching(launch)
	h.toDriver.EXPECT().RetrieveIncoming()
	if !h.cp.middleware.processLaunchKernelReq(launch) {
		t.Fatal("kernel dispatch must proceed after the counter reset ack")
	}
}

func TestUVMDMAOverlap(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	mig1 := &protocol.MigrationReq{PID: 1, GPU: 0, VAddr: 0x10000, Size: 64 * 1024,
		Direction: protocol.MigrationCPUToGPU}
	mig1.ID = "mig-1"
	mig1.Src = sim.RemotePort("Driver")
	mig1.Dst = sim.RemotePort("ToDriver")
	mig2 := &protocol.MigrationReq{PID: 1, GPU: 0, VAddr: 0x20000, Size: 64 * 1024,
		Direction: protocol.MigrationCPUToGPU}
	mig2.ID = "mig-2"
	mig2.Src = sim.RemotePort("Driver")
	mig2.Dst = sim.RemotePort("ToDriver")

	sent := map[uint64]bool{}
	h.toDMA.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		copyReq := msg.(*protocol.MemCopyH2DReq)
		sent[copyReq.DstAddress] = true
		return nil
	}).Times(2)
	h.toDriver.EXPECT().RetrieveIncoming().Times(2)

	if !h.mw.processMigrationReq(mig1) {
		t.Fatal("the first migration must be accepted")
	}
	if !h.mw.processMigrationReq(mig2) {
		t.Fatal("the second migration must be accepted")
	}
	// Independent migration DMA transfers overlap: both are in flight.
	if !sent[0x10000] || !sent[0x20000] {
		t.Fatalf("independent migration DMA must overlap, got %v", sent)
	}
}

func TestUVMReplayRoute(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	replayReq := &protocol.UVMFaultReplayReq{
		PID:         1,
		GPU:         0,
		StartVA:     0x10000,
		Size:        64 * 1024,
		ReplayToken: 9,
	}
	replayReq.ID = "replay-1"
	replayReq.Src = sim.RemotePort("Driver")
	replayReq.Dst = sim.RemotePort("ToDriver")

	var forwarded *vm.ReplayRange
	h.toGMMU.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		forwarded = msg.(*vm.ReplayRange)
		return nil
	})
	h.toDriver.EXPECT().RetrieveIncoming()

	if !h.mw.processFaultReplayReq(replayReq) {
		t.Fatal("the replay command must be accepted")
	}
	if forwarded == nil || forwarded.PID != 1 || forwarded.GPU != 0 ||
		forwarded.StartVA != 0x10000 || forwarded.Size != 64*1024 ||
		forwarded.ReplayToken != 9 {
		t.Fatalf("the ReplayRange must carry the replay contract: %+v", forwarded)
	}

	ack := &vm.ReplayAck{RspTo: forwarded.ID, ReplayToken: 9}
	ack.ID = "replay-ack-1"
	ack.Src = sim.RemotePort("ToGMMU")
	ack.Dst = sim.RemotePort("ToGMMU")

	var rsp *protocol.UVMFaultReplayRsp
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		rsp = msg.(*protocol.UVMFaultReplayRsp)
		return nil
	})
	if !h.mw.processReplayAck(ack) {
		t.Fatal("the replay ack must be accepted")
	}
	if rsp == nil || rsp.RspTo != replayReq.ID {
		t.Fatalf("the replay response must correlate to the driver command: %+v", rsp)
	}
}

func TestUVMAckCorrelation(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	h.cp.UVMGateIDs = []uint64{1}

	blockA := &vm.BlockRange{CommandID: 400, PID: 1, StartVA: 0x10000, Size: 64 * 1024}
	blockA.ID = "block-400"
	blockA.Src = sim.RemotePort("Driver")
	blockA.Dst = sim.RemotePort("ToDriver")
	blockB := &vm.BlockRange{CommandID: 500, PID: 1, StartVA: 0x20000, Size: 64 * 1024}
	blockB.ID = "block-500"
	blockB.Src = sim.RemotePort("Driver")
	blockB.Dst = sim.RemotePort("ToDriver")

	h.toGMMU.EXPECT().Send(gomock.Any()).Return(nil).Times(2)
	h.toDriver.EXPECT().RetrieveIncoming().Times(2)
	if !h.mw.processBlockRange(blockA) || !h.mw.processBlockRange(blockB) {
		t.Fatal("both block commands must be accepted")
	}

	// An ack for command A must complete only A and leave B pending.
	var completedA *sim.GeneralRsp
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		completedA = msg.(*sim.GeneralRsp)
		return nil
	})
	if !h.mw.processBlockAck(&vm.BlockAck{CommandID: 400, GateID: 1, Watermark: 1}) {
		t.Fatal("the ack for command A must be accepted")
	}
	if completedA == nil || completedA.OriginalReq != blockA {
		t.Fatal("the completion must correlate to command A")
	}
	if _, ok := h.cp.activeUVMBlocks[500]; !ok {
		t.Fatal("command B must remain pending after command A completes")
	}

	// The ack for command B then completes it.
	var completedB *sim.GeneralRsp
	h.toGMMU.EXPECT().RetrieveIncoming()
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		completedB = msg.(*sim.GeneralRsp)
		return nil
	})
	if !h.mw.processBlockAck(&vm.BlockAck{CommandID: 500, GateID: 1, Watermark: 2}) {
		t.Fatal("the ack for command B must be accepted")
	}
	if completedB == nil || completedB.OriginalReq != blockB {
		t.Fatal("the completion must correlate to command B")
	}
}
