package uvm

// sbin_codex: GPU-wide AccessCounter contract tests (plan todo 11 of
// mgpusim-uvm-manager). These plain Go tests drive the counter with the
// package mocks and assert: per-(PID, GPU, 64 KB region) accounting with CU
// aggregation, the immediate equality notification at the threshold (exactly
// one per residency episode), notification suppression during
// migration/prefetch, GPU-resident regions not counting, the kernel-launch
// reset (both the API and the acknowledged CounterResetReq/CounterResetRsp
// barrier), and per-region independence.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
	"go.uber.org/mock/gomock"
)

// accessCounterHarness builds an AccessCounter whose ToCP port is a mock so
// the reset barrier and the notifications can be driven deterministically.
type accessCounterHarness struct {
	ctrl *gomock.Controller
	c    *AccessCounter
	toCP *MockPort
}

func newAccessCounterHarness(t *testing.T) *accessCounterHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)

	toCP := NewMockPort(ctrl)
	toCP.EXPECT().AsRemote().Return(sim.RemotePort("ToCP")).AnyTimes()

	c := MakeAccessCounterBuilder().
		WithEngine(engine).
		WithFreq(1).
		Build("AccessCounter")
	c.ToCP = toCP

	return &accessCounterHarness{ctrl: ctrl, c: c, toCP: toCP}
}

// tick runs one counter tick with no reset in flight.
func (h *accessCounterHarness) tick() {
	h.toCP.EXPECT().PeekIncoming().Return(nil)
	h.c.Tick()
}

func TestUVMAccessCounter(t *testing.T) {
	h := newAccessCounterHarness(t)
	defer h.ctrl.Finish()

	// Eight remote reads of one 64 KB region yield exactly one immediate
	// equality notification at the threshold (default 8).
	for i := 0; i < 8; i++ {
		h.c.RecordRemoteAccess(1, 0, 0x10000+uint64(i)*4)
	}

	var notif *protocol.AccessCounterNotification
	h.toCP.EXPECT().PeekIncoming().Return(nil)
	h.toCP.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		notif = msg.(*protocol.AccessCounterNotification)
		return nil
	})
	h.c.Tick()

	if notif == nil {
		t.Fatal("the threshold crossing must send an immediate notification")
	}
	if notif.PID != 1 || notif.GPU != 0 || notif.VAddr != 0x10000 ||
		notif.AccessCount != 8 || notif.AccessKind != vm.AccessKindRead {
		t.Fatalf("notification fields mismatch: %+v", notif)
	}

	// A ninth access must not generate a duplicate notification.
	h.c.RecordRemoteAccess(1, 0, 0x10000)
	h.tick()

	// CU aggregation: accesses at different addresses inside the same 64 KB
	// region share one counter; a different region has its own counter.
	if got := h.c.Count(1, 0, 0x10000); got != 9 {
		t.Fatalf("region 0x10000 must aggregate 9 accesses, got %d", got)
	}
	h.c.RecordRemoteAccess(1, 0, 0x10040)
	if got := h.c.Count(1, 0, 0x10000); got != 10 {
		t.Fatalf("a same-region access must aggregate into the region counter, got %d", got)
	}
	if got := h.c.Count(1, 0, 0x20000); got != 0 {
		t.Fatalf("a different region must have its own counter, got %d", got)
	}

	// A different region reaches the threshold independently.
	for i := 0; i < 8; i++ {
		h.c.RecordRemoteAccess(1, 0, 0x20000+uint64(i)*4)
	}
	var notif2 *protocol.AccessCounterNotification
	h.toCP.EXPECT().PeekIncoming().Return(nil)
	h.toCP.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		notif2 = msg.(*protocol.AccessCounterNotification)
		return nil
	})
	h.c.Tick()
	if notif2 == nil || notif2.VAddr != 0x20000 || notif2.AccessCount != 8 {
		t.Fatalf("the second region must notify independently: %+v", notif2)
	}

	// Suppression: while a region is in FAULT_PENDING/FAULT_HANDLING/
	// MIGRATING_TO_GPU/PREFETCHING_TO_GPU its accesses never notify, and the
	// suppressed threshold crossing does not fire retroactively.
	h.c.Suppress(1, 0, 0x30000)
	for i := 0; i < 8; i++ {
		h.c.RecordRemoteAccess(1, 0, 0x30000+uint64(i)*4)
	}
	if got := h.c.Count(1, 0, 0x30000); got != 8 {
		t.Fatalf("suppressed accesses must still increment, got %d", got)
	}
	h.tick()
	h.c.Unsuppress(1, 0, 0x30000)
	h.c.RecordRemoteAccess(1, 0, 0x30000)
	h.tick()
	if got := h.c.Count(1, 0, 0x30000); got != 9 {
		t.Fatalf("post-unsuppress accesses must increment, got %d", got)
	}

	// GPU-resident regions no longer count until they return to CPU-resident
	// (uvm-manager.md §31.1).
	h.c.MarkGPUResident(1, 0, 0x40000)
	for i := 0; i < 8; i++ {
		h.c.RecordRemoteAccess(1, 0, 0x40000+uint64(i)*4)
	}
	if got := h.c.Count(1, 0, 0x40000); got != 0 {
		t.Fatalf("a GPU-resident region must not count remote accesses, got %d", got)
	}
	h.c.MarkCPUResident(1, 0, 0x40000)
	for i := 0; i < 8; i++ {
		h.c.RecordRemoteAccess(1, 0, 0x40000+uint64(i)*4)
	}
	var notif3 *protocol.AccessCounterNotification
	h.toCP.EXPECT().PeekIncoming().Return(nil)
	h.toCP.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		notif3 = msg.(*protocol.AccessCounterNotification)
		return nil
	})
	h.c.Tick()
	if notif3 == nil || notif3.VAddr != 0x40000 {
		t.Fatalf("a CPU-resident episode must notify again: %+v", notif3)
	}

	// Kernel-launch reset: the acknowledged CounterResetReq clears every
	// counter and the episode flags, and the ack returns to the CP seam.
	reset := &cp.CounterResetReq{}
	reset.ID = "reset-1"
	reset.Src = sim.RemotePort("ToAccessCounter")
	reset.Dst = sim.RemotePort("ToAccessCounter")

	var rsp *cp.CounterResetRsp
	h.toCP.EXPECT().PeekIncoming().Return(reset)
	h.toCP.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		rsp = msg.(*cp.CounterResetRsp)
		return nil
	})
	h.toCP.EXPECT().RetrieveIncoming()
	h.c.Tick()

	if rsp == nil {
		t.Fatal("the counter must acknowledge the reset")
	}
	if rsp.Dst != sim.RemotePort("ToAccessCounter") {
		t.Fatalf("the reset ack must return to the CP seam, got %s", rsp.Dst)
	}
	if got := h.c.Count(1, 0, 0x10000); got != 0 {
		t.Fatalf("the kernel-launch reset must clear the counters, got %d", got)
	}

	// After the reset the region counts from zero again and notifies again.
	for i := 0; i < 8; i++ {
		h.c.RecordRemoteAccess(1, 0, 0x10000+uint64(i)*4)
	}
	var notif4 *protocol.AccessCounterNotification
	h.toCP.EXPECT().PeekIncoming().Return(nil)
	h.toCP.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		notif4 = msg.(*protocol.AccessCounterNotification)
		return nil
	})
	h.c.Tick()
	if notif4 == nil || notif4.AccessCount != 8 {
		t.Fatalf("a reset region must notify on its next threshold crossing: %+v", notif4)
	}
}
