package cp

// sbin_codex: kernel-launch AccessCounter reset barrier and threshold
// notification routing contract tests (plan todo 11 of mgpusim-uvm-manager).
// The acknowledged barrier is Driver -> CP -> GPU-wide AccessCounter -> CP ack
// -> kernel dispatch: no workgroup may dispatch before the ack, and a
// duplicate reset while one is pending is rejected. The counter's threshold
// notification is routed to the UVM driver like a fault notification.

import (
	"testing"

	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp/internal/dispatching"
	"go.uber.org/mock/gomock"
)

func TestUVMKernelResetBarrier(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	// Driver -> CP: the acknowledged reset raises the kernel-dispatch barrier.
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

	// A duplicate reset while one is pending is rejected: it is consumed but
	// never forwarded again.
	h.toDriver.EXPECT().RetrieveIncoming()
	if !h.mw.processCounterResetReq(&CounterResetReq{}) {
		t.Fatal("a duplicate reset must be consumed")
	}

	// No workgroup may dispatch while the barrier is pending.
	launch := &protocol.LaunchKernelReq{}
	launch.ID = "launch-1"
	if h.cp.middleware.processLaunchKernelReq(launch) {
		t.Fatal("kernel dispatch must be held while the counter reset is pending")
	}

	// CP ack: the AccessCounter acknowledges the reset and releases the
	// barrier.
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

func TestUVMRouteAccessCounterNotification(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	notif := &protocol.AccessCounterNotification{
		PID:         1,
		GPU:         0,
		VAddr:       0x10000,
		AccessKind:  0, // read
		AccessCount: 8,
	}
	notif.ID = "notif-1"
	notif.Src = sim.RemotePort("ToAccessCounter")
	notif.Dst = sim.RemotePort("ToAccessCounter")

	var routed *protocol.AccessCounterNotification
	h.toAccessCounter.EXPECT().PeekIncoming().Return(notif)
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		routed = msg.(*protocol.AccessCounterNotification)
		return nil
	})
	h.toAccessCounter.EXPECT().RetrieveIncoming()

	if !h.mw.processRspFromAccessCounter() {
		t.Fatal("the counter notification must be routed to the driver")
	}
	if routed == nil {
		t.Fatal("no AccessCounterNotification was routed to the driver")
	}
	if routed.PID != 1 || routed.GPU != 0 || routed.VAddr != 0x10000 ||
		routed.AccessCount != 8 {
		t.Fatalf("the routed notification must carry the counter contract: %+v", routed)
	}
	if routed.Dst != sim.RemotePort("Driver") {
		t.Fatalf("the notification must target the UVM driver, got %s", routed.Dst)
	}
}
