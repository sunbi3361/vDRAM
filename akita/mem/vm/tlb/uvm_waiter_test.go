package tlb

// sbin_codex: UVM fault-pending waiter accounting contract tests (plan todo 7
// of mgpusim-uvm-manager). These plain Go tests drive the TLB middleware with
// the package mocks and assert the exact raw-fault waiter equations
// (raw = unique + coalesced), token propagation, no-double-counting at the
// shared L2, no negative caching, and exactly-once replay responses.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// uvmTLBHarness builds a TLB with mocked ports and a single mocked set so the
// middleware can be driven deterministically.
type uvmTLBHarness struct {
	ctrl          *gomock.Controller
	tlb           *Comp
	mw            *tlbMiddleware
	set           *MockSet
	topPort       *MockPort
	bottomPort    *MockPort
	controlPort   *MockPort
	addressMapper *MockAddressToPortMapper
}

func newUVMTLBHarness(t *testing.T, isLeaf bool) *uvmTLBHarness {
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)
	set := NewMockSet(ctrl)
	topPort := NewMockPort(ctrl)
	topPort.EXPECT().
		AsRemote().
		Return(sim.RemotePort("TopPort")).
		AnyTimes()
	topPort.EXPECT().
		Name().
		Return("TopPort").
		AnyTimes()
	bottomPort := NewMockPort(ctrl)
	bottomPort.EXPECT().
		AsRemote().
		Return(sim.RemotePort("BottomPort")).
		AnyTimes()
	bottomPort.EXPECT().
		Name().
		Return("BottomPort").
		AnyTimes()
	controlPort := NewMockPort(ctrl)
	controlPort.EXPECT().
		AsRemote().
		Return(sim.RemotePort("ControlPort")).
		AnyTimes()
	controlPort.EXPECT().
		Name().
		Return("ControlPort").
		AnyTimes()
	addressMapper := NewMockAddressToPortMapper(ctrl)

	tlb := MakeBuilder().
		WithEngine(engine).
		WithTranslationProviderMapper(addressMapper).
		WithIsLeafDataTranslationPoint(isLeaf).
		Build("TLB")
	tlb.topPort = topPort
	tlb.bottomPort = bottomPort
	tlb.controlPort = controlPort
	tlb.sets = []internal.Set{set}
	tlb.state = tlbStateEnable

	mw := tlb.Middlewares()[1].(*tlbMiddleware)

	return &uvmTLBHarness{
		ctrl:          ctrl,
		tlb:           tlb,
		mw:            mw,
		set:           set,
		topPort:       topPort,
		bottomPort:    bottomPort,
		controlPort:   controlPort,
		addressMapper: addressMapper,
	}
}

// expectMiss sets up the set lookup miss and the bottom mapper for vAddr.
func (h *uvmTLBHarness) expectMiss(vAddr uint64) {
	h.set.EXPECT().
		Lookup(vm.PID(1), vAddr).
		Return(0, vm.Page{}, false).
		AnyTimes()
	h.addressMapper.EXPECT().
		Find(vAddr).
		Return(sim.RemotePort("Low")).
		AnyTimes()
}

// captureSend records the next bottom-port translation request.
func (h *uvmTLBHarness) captureBottomSend() *vm.TranslationReq {
	captured := &vm.TranslationReq{}
	h.bottomPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(req *vm.TranslationReq) error {
			*captured = *req
			return nil
		})
	return captured
}

// deliverBottomRsp injects a translation response into the bottom port.
func (h *uvmTLBHarness) deliverBottomRsp(rsp *vm.TranslationRsp) {
	h.bottomPort.EXPECT().PeekIncoming().Return(rsp)
	h.bottomPort.EXPECT().RetrieveIncoming()
}

// faultPendingRsp builds a fault-pending response for the given token.
func faultPendingRsp(rspTo string, token vm.FaultPendingToken) *vm.TranslationRsp {
	page := vm.Page{
		PID:      1,
		VAddr:    0x100,
		Valid:    false,
		Managed:  true,
		Location: vm.MemoryLocationINVALID,
	}
	return vm.TranslationRspBuilder{}.
		WithSrc(sim.RemotePort("Low")).
		WithRspTo(rspTo).
		WithPage(page).
		WithLocation(vm.MemoryLocationINVALID).
		WithFaultPendingToken(token).
		Build()
}

func TestUVMFaultToken(t *testing.T) {
	h := newUVMTLBHarness(t, true)
	defer h.ctrl.Finish()
	h.expectMiss(0x100)

	// The first request misses and fetches from the bottom with no token yet.
	f1 := h.captureBottomSend()
	req1 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req1) {
		t.Fatal("lookup of the first request must succeed")
	}
	if f1.FaultPendingToken != 0 {
		t.Fatalf("fetchBottom must carry no token before the GMMU assigns one, got %d",
			f1.FaultPendingToken)
	}

	// The GMMU fault-pending response assigns token 7; the TLB propagates it
	// up and retains the MSHR entry with the recorded token.
	h.deliverBottomRsp(faultPendingRsp(f1.ID, 7))
	var upRsp *vm.TranslationRsp
	h.topPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(rsp *vm.TranslationRsp) error {
			upRsp = rsp
			return nil
		})
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the fault-pending response")
	}
	if upRsp == nil || upRsp.FaultPendingToken != 7 {
		t.Fatalf("the fault-pending response must propagate the GMMU token 7, got %+v",
			upRsp)
	}
	if upRsp.RespondTo != req1.ID {
		t.Fatalf("the fault-pending response must reply to the first waiter, got %s",
			upRsp.RespondTo)
	}
	entry := h.tlb.mshr.GetEntry(1, 0x100)
	if entry == nil {
		t.Fatal("the MSHR entry must be retained while fault-pending")
	}
	if !entry.faultPending {
		t.Fatal("the MSHR entry must be marked fault-pending")
	}
	if entry.faultPendingToken != 7 {
		t.Fatalf("the MSHR entry must record the GMMU token, got %d",
			entry.faultPendingToken)
	}

	// A re-injected request that already carries a token propagates its own.
	f2 := h.captureBottomSend()
	req2 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		WithFaultPendingToken(9).
		Build()
	if !h.mw.lookup(req2) {
		t.Fatal("lookup of the token-carrying request must succeed")
	}
	if f2.FaultPendingToken != 9 {
		t.Fatalf("the re-fetch must carry the request's own token 9, got %d",
			f2.FaultPendingToken)
	}

	// A request without a token re-fetches with the token recorded on the
	// entry.
	f3 := h.captureBottomSend()
	req3 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req3) {
		t.Fatal("lookup of the token-less request must succeed")
	}
	if f3.FaultPendingToken != 7 {
		t.Fatalf("the re-fetch must carry the entry's recorded token 7, got %d",
			f3.FaultPendingToken)
	}
}

func TestUVMLeafWaiterDelta(t *testing.T) {
	h := newUVMTLBHarness(t, true)
	defer h.ctrl.Finish()
	h.expectMiss(0x100)

	// Phase 1: before fault-pending. The first request is the unique waiter;
	// a second request joining the MSHR is the first coalesced waiter.
	f1 := h.captureBottomSend()
	req1 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req1) {
		t.Fatal("lookup of the first request must succeed")
	}
	if f1.WaiterDelta != (vm.WaiterDelta{InitialWaiters: 1}) {
		t.Fatalf("phase 1: fetchBottom must report initial waiter count 1, got %+v",
			f1.WaiterDelta)
	}
	entry := h.tlb.mshr.GetEntry(1, 0x100)
	if entry.waiterDelta != (vm.WaiterDelta{InitialWaiters: 1}) {
		t.Fatalf("phase 1: the MSHR entry must report {1,0}, got %+v",
			entry.waiterDelta)
	}
	req2 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req2) {
		t.Fatal("lookup of the coalescing request must succeed")
	}
	if entry.waiterDelta != (vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 1}) {
		t.Fatalf("phase 1: raw = unique + coalesced = 1 + 1 = 2, got %+v",
			entry.waiterDelta)
	}

	// The fault-pending response reports the leaf's current counts.
	h.deliverBottomRsp(faultPendingRsp(f1.ID, 7))
	var upRsp *vm.TranslationRsp
	h.topPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(rsp *vm.TranslationRsp) error {
			upRsp = rsp
			return nil
		})
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the fault-pending response")
	}
	want := vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 1}
	if upRsp.WaiterDelta != want {
		t.Fatalf("phase 2: the fault-pending response must report {1,1}, got %+v",
			upRsp.WaiterDelta)
	}
	if raw := upRsp.WaiterDelta.InitialWaiters + upRsp.WaiterDelta.LateMSHRWaiters; raw != 2 {
		t.Fatalf("phase 2: raw = unique + coalesced must be 2, got %d", raw)
	}

	// Phase 3: during service. Each joining waiter increments the late delta
	// and re-fetches with the current counts.
	f3 := h.captureBottomSend()
	req3 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req3) {
		t.Fatal("lookup of the service-phase waiter must succeed")
	}
	if entry.waiterDelta != (vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 2}) {
		t.Fatalf("phase 3: raw = 1 + 2 = 3, got %+v", entry.waiterDelta)
	}
	if f3.WaiterDelta != (vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 2}) {
		t.Fatalf("phase 3: the re-fetch must carry the current counts, got %+v",
			f3.WaiterDelta)
	}

	// Phase 4: immediately before replay. The equation still holds.
	h.captureBottomSend()
	req4 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req4) {
		t.Fatal("lookup of the pre-replay waiter must succeed")
	}
	if entry.waiterDelta != (vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 3}) {
		t.Fatalf("phase 4: raw = 1 + 3 = 4, got %+v", entry.waiterDelta)
	}
	if raw := entry.waiterDelta.InitialWaiters + entry.waiterDelta.LateMSHRWaiters; raw != 4 {
		t.Fatalf("phase 4: raw = unique + coalesced must be 4, got %d", raw)
	}
	if entry.waiterDelta.InitialWaiters != 1 {
		t.Fatalf("phase 4: exactly one unique waiter, got %d",
			entry.waiterDelta.InitialWaiters)
	}
	if entry.waiterDelta.LateMSHRWaiters != 3 {
		t.Fatalf("phase 4: exactly three coalesced waiters, got %d",
			entry.waiterDelta.LateMSHRWaiters)
	}
}

func TestUVML2NoDoubleCount(t *testing.T) {
	h := newUVMTLBHarness(t, false)
	defer h.ctrl.Finish()
	h.expectMiss(0x100)

	// The L1's fetchBottom carries its own waiter snapshot; the shared L2 must
	// propagate it and never count its own forwarding request.
	f1 := h.captureBottomSend()
	req1 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("L1")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		WithWaiterDelta(vm.WaiterDelta{InitialWaiters: 1}).
		Build()
	if !h.mw.lookup(req1) {
		t.Fatal("lookup of the L1 request must succeed")
	}
	entry := h.tlb.mshr.GetEntry(1, 0x100)
	if entry.waiterDelta != (vm.WaiterDelta{InitialWaiters: 1}) {
		t.Fatalf("the L2 must propagate the L1 delta {1,0} without counting its own forwarding request, got %+v",
			entry.waiterDelta)
	}
	if f1.WaiterDelta != (vm.WaiterDelta{InitialWaiters: 1}) {
		t.Fatalf("the L2's own forwarding request must carry the L1 delta unchanged, got %+v",
			f1.WaiterDelta)
	}

	// A second L1 request joining the L2 MSHR adds no count.
	req2 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("L1")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		WithWaiterDelta(vm.WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 2}).
		Build()
	if !h.mw.lookup(req2) {
		t.Fatal("lookup of the second L1 request must succeed")
	}
	if entry.waiterDelta != (vm.WaiterDelta{InitialWaiters: 1}) {
		t.Fatalf("the L2 must never add counts for MSHR hits, got %+v",
			entry.waiterDelta)
	}

	// The fault-pending response propagates the received delta unchanged: the
	// L2's own forwarding request never appears in the count.
	h.deliverBottomRsp(faultPendingRsp(f1.ID, 7))
	var upRsp *vm.TranslationRsp
	h.topPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(rsp *vm.TranslationRsp) error {
			upRsp = rsp
			return nil
		})
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the fault-pending response")
	}
	if upRsp.WaiterDelta != (vm.WaiterDelta{InitialWaiters: 1}) {
		t.Fatalf("the L2 must propagate the received delta {1,0} without adding its own forwarding count, got %+v",
			upRsp.WaiterDelta)
	}
	if upRsp.WaiterDelta.InitialWaiters != 1 {
		t.Fatalf("the L2's own forwarding request must not be counted (initial would be 2), got %d",
			upRsp.WaiterDelta.InitialWaiters)
	}
}

func TestUVMLateReplayRace(t *testing.T) {
	h := newUVMTLBHarness(t, true)
	defer h.ctrl.Finish()
	h.expectMiss(0x100)

	// The first request misses and the translation becomes fault-pending.
	f1 := h.captureBottomSend()
	req1 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent1")).
		WithPID(1).
		WithVAddr(0x100).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req1) {
		t.Fatal("lookup of the first request must succeed")
	}
	h.deliverBottomRsp(faultPendingRsp(f1.ID, 7))
	h.topPort.EXPECT().Send(gomock.Any()).Return(nil)
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the fault-pending response")
	}

	// Late waiters and the replay re-injection all hit the retained MSHR
	// entry; each triggers a re-fetch.
	reqs := make([]*vm.TranslationReq, 0, 3)
	for i, src := range []string{"Agent2", "Agent3", "Agent4"} {
		h.captureBottomSend()
		req := vm.TranslationReqBuilder{}.
			WithSrc(sim.RemotePort(src)).
			WithPID(1).
			WithVAddr(0x100).
			WithDeviceID(1).
			Build()
		if !h.mw.lookup(req) {
			t.Fatalf("lookup of waiter %d must succeed", i)
		}
		reqs = append(reqs, req)
	}

	// The valid final response completes the translation: the set receives
	// only the valid page and the MSHR is drained.
	validPage := vm.Page{
		PID:    1,
		VAddr:  0x100,
		PAddr:  0x200,
		Valid:  true,
		DeviceID: 1,
	}
	validRsp := vm.TranslationRspBuilder{}.
		WithSrc(sim.RemotePort("Low")).
		WithRspTo(f1.ID).
		WithPage(validPage).
		Build()
	h.deliverBottomRsp(validRsp)
	h.set.EXPECT().Evict().Return(0, true)
	h.set.EXPECT().Update(0, validPage)
	h.set.EXPECT().Visit(0)
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the valid response")
	}

	// Replay responds exactly once per original request.
	responded := map[string]int{}
	h.topPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(rsp *vm.TranslationRsp) error {
			responded[rsp.RespondTo]++
			if !rsp.Page.Valid {
				t.Fatalf("replay must respond with a valid page, got %+v", rsp.Page)
			}
			return nil
		}).
		Times(4)
	for i := 0; i < 4; i++ {
		if !h.mw.respondMSHREntry() {
			t.Fatalf("respondMSHREntry call %d must succeed", i)
		}
	}
	if h.mw.respondMSHREntry() {
		t.Fatal("no extra response may be sent after the retained waiters")
	}
	allReqs := append([]*vm.TranslationReq{req1}, reqs...)
	for _, req := range allReqs {
		if responded[req.ID] != 1 {
			t.Fatalf("request %s must be responded exactly once, got %d",
				req.ID, responded[req.ID])
		}
	}
	if !h.tlb.mshr.IsEmpty() {
		t.Fatal("the MSHR must be empty after the replay completes")
	}

	// An invalid normal response (no fault-pending token) is rejected: no
	// negative entry is installed and the MSHR retains the original waiter.
	h.expectMiss(0x200)
	f5 := h.captureBottomSend()
	req5 := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent5")).
		WithPID(1).
		WithVAddr(0x200).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req5) {
		t.Fatal("lookup of the invalid-page request must succeed")
	}
	invalidPage := vm.Page{
		PID:      1,
		VAddr:    0x200,
		Valid:    false,
		Managed:  true,
		Location: vm.MemoryLocationINVALID,
	}
	invalidRsp := vm.TranslationRspBuilder{}.
		WithSrc(sim.RemotePort("Low")).
		WithRspTo(f5.ID).
		WithPage(invalidPage).
		WithLocation(vm.MemoryLocationINVALID).
		Build()
	h.deliverBottomRsp(invalidRsp)
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the invalid response")
	}
	// No set.Evict/Update/Visit and no topPort.Send expectations were set for
	// this phase: any fill or response would fail the mock controller.
	entry5 := h.tlb.mshr.GetEntry(1, 0x200)
	if entry5 == nil {
		t.Fatal("the MSHR must retain the original waiter after the invalid response")
	}
	if len(entry5.Requests) != 1 {
		t.Fatalf("the MSHR must retain exactly the original waiter, got %d",
			len(entry5.Requests))
	}
}