package gmmu

// sbin_codex: UVM range TLB invalidation coordination contract tests (plan
// todo 14 of mgpusim-uvm-manager). These plain Go tests drive the GMMU
// invalidation coordinator with mocked TLB endpoints and assert: the exact
// baseline/virtual endpoint sets (no fabricated virtual L1V/L1S TLB
// endpoints), one broadcast per topology-present TLB, out-of-order ack
// aggregation, deterministic missing/duplicate/unknown ack handling, and
// exactly one completion response (uvm-manager.md §21.1).

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// uvmInvalidateGMMUHarness builds a GMMU coordinator with mocked ports and
// mocked TLB endpoints so the broadcast/ack/completion protocol can be driven
// deterministically.
type uvmInvalidateGMMUHarness struct {
	ctrl        *gomock.Controller
	comp        *Comp
	ctrlMw      *ctrlMiddleware
	controlPort *MockPort
	broadcasts  map[string]*tlb.UVMTLBInvalidateReq
	completions []*tlb.UVMTLBInvalidateRsp
}

func newUVMInvalidateGMMUHarness(
	t *testing.T, endpointNames ...string,
) *uvmInvalidateGMMUHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)

	controlPort := NewMockPort(ctrl)
	controlPort.EXPECT().AsRemote().Return(sim.RemotePort("ControlPort")).AnyTimes()
	controlPort.EXPECT().Name().Return("ControlPort").AnyTimes()

	endpoints := make([]sim.Port, 0, len(endpointNames))
	for _, name := range endpointNames {
		endpoints = append(endpoints, mockEndpoint(ctrl, name))
	}

	comp := &Comp{
		state:                gmmuStateEnable,
		deviceID:             1,
		tlbEndpoints:         endpoints,
		activeTLBInvalidates: make(map[string]*tlbInvalidateCommand),
	}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUTest", engine, sim.GHz, nil)
	comp.controlPort = controlPort

	ctrlMw := &ctrlMiddleware{Comp: comp}

	return &uvmInvalidateGMMUHarness{
		ctrl:        ctrl,
		comp:        comp,
		ctrlMw:      ctrlMw,
		controlPort: controlPort,
		broadcasts:  make(map[string]*tlb.UVMTLBInvalidateReq),
	}
}

// mockEndpoint creates a mock TLB endpoint port with the given name.
func mockEndpoint(ctrl *gomock.Controller, name string) *MockPort {
	p := NewMockPort(ctrl)
	p.EXPECT().AsRemote().Return(sim.RemotePort(name)).AnyTimes()
	p.EXPECT().Name().Return(name).AnyTimes()
	return p
}

// deliverReq injects a UVMTLBInvalidateReq into the control port and captures
// the broadcast to every registered endpoint.
func (h *uvmInvalidateGMMUHarness) deliverReq(pid vm.PID, startVA, size uint64) *tlb.UVMTLBInvalidateReq {
	req := &tlb.UVMTLBInvalidateReq{PID: pid, StartVA: startVA, Size: size}
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = sim.RemotePort("CP")
	req.Dst = sim.RemotePort("ControlPort")

	h.controlPort.EXPECT().PeekIncoming().Return(req)
	h.controlPort.EXPECT().RetrieveIncoming()
	h.controlPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(inv *tlb.UVMTLBInvalidateReq) error {
			h.broadcasts[inv.ID] = inv
			return nil
		}).
		Times(len(h.comp.tlbEndpoints))

	if !h.ctrlMw.handleIncomingCommands() {
		panic("handleIncomingCommands must consume the invalidation request")
	}
	return req
}

// deliverAck injects a UVMTLBInvalidateRsp correlated to the given broadcast
// request ID into the control port.
func (h *uvmInvalidateGMMUHarness) deliverAck(rspTo string) {
	ack := tlb.UVMTLBInvalidateRspBuilder{}.
		WithSrc(sim.RemotePort("TLB")).
		WithDst(sim.RemotePort("ControlPort")).
		WithRspTo(rspTo).
		Build()
	h.controlPort.EXPECT().PeekIncoming().Return(ack)
	h.controlPort.EXPECT().RetrieveIncoming()
	if !h.ctrlMw.handleIncomingCommands() {
		panic("handleIncomingCommands must consume the acknowledgement")
	}
}

// expectCompletion captures the next completion response send.
func (h *uvmInvalidateGMMUHarness) expectCompletion() {
	h.controlPort.EXPECT().CanSend().Return(true)
	h.controlPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(rsp *tlb.UVMTLBInvalidateRsp) error {
			h.completions = append(h.completions, rsp)
			return nil
		})
	h.ctrlMw.tryCompleteTLBInvalidates()
}

// TestUVMBaselineEndpointSet proves the coordinator broadcasts to every
// baseline endpoint (all private L1V/L1S/L1I TLBs plus the shared L2 TLB) and
// returns exactly one completion after all acknowledgements arrive.
func TestUVMBaselineEndpointSet(t *testing.T) {
	h := newUVMInvalidateGMMUHarness(t, "L1V", "L1S", "L1I", "L2")
	defer h.ctrl.Finish()

	req := h.deliverReq(1, 0x10000, 64*1024)
	if len(h.broadcasts) != 4 {
		t.Fatalf("baseline must broadcast to exactly 4 endpoints, got %d",
			len(h.broadcasts))
	}
	want := map[sim.RemotePort]bool{
		"L1V": false, "L1S": false, "L1I": false, "L2": false,
	}
	for _, inv := range h.broadcasts {
		if inv.PID != 1 || inv.StartVA != 0x10000 || inv.Size != 64*1024 {
			t.Fatalf("the broadcast must carry PID/StartVA/Size, got %+v", inv)
		}
		if inv.Src != sim.RemotePort("ControlPort") {
			t.Fatalf("the broadcast must originate from the GMMU control port, got %s",
				inv.Src)
		}
		if _, ok := want[inv.Dst]; !ok {
			t.Fatalf("baseline broadcast must not target %s", inv.Dst)
		}
		want[inv.Dst] = true
	}
	for dst, seen := range want {
		if !seen {
			t.Fatalf("baseline broadcast must include %s", dst)
		}
	}

	// Out-of-order acknowledgements still complete exactly once: the first
	// three acks never complete, the final ack fires the single completion.
	ids := make([]string, 0, len(h.broadcasts))
	for id := range h.broadcasts {
		ids = append(ids, id)
	}
	for _, id := range ids[:len(ids)-1] {
		h.deliverAck(id)
		h.ctrlMw.tryCompleteTLBInvalidates()
	}
	if len(h.completions) != 0 {
		t.Fatal("partial acknowledgements must not complete the invalidation")
	}
	h.deliverAck(ids[len(ids)-1])
	h.expectCompletion()
	if len(h.completions) != 1 {
		t.Fatalf("the coordinator must return exactly one completion, got %d",
			len(h.completions))
	}
	if h.completions[0].RspTo != req.ID {
		t.Fatalf("the completion must correlate to the original request %s, got %s",
			req.ID, h.completions[0].RspTo)
	}
	if h.completions[0].Dst != sim.RemotePort("CP") {
		t.Fatalf("the completion must be addressed back to the requester, got %s",
			h.completions[0].Dst)
	}
	if len(h.comp.activeTLBInvalidates) != 0 {
		t.Fatal("the command must be retired after completion")
	}
}

// TestUVMVirtualEndpointSet proves the virtual-caching topology broadcasts to
// the private L1I and the shared L2 TLB only.
func TestUVMVirtualEndpointSet(t *testing.T) {
	h := newUVMInvalidateGMMUHarness(t, "L1I", "L2")
	defer h.ctrl.Finish()

	req := h.deliverReq(1, 0x10000, 64*1024)
	if len(h.broadcasts) != 2 {
		t.Fatalf("virtual must broadcast to exactly 2 endpoints, got %d",
			len(h.broadcasts))
	}
	want := map[sim.RemotePort]bool{"L1I": false, "L2": false}
	for _, inv := range h.broadcasts {
		if _, ok := want[inv.Dst]; !ok {
			t.Fatalf("virtual broadcast must not target %s", inv.Dst)
		}
		want[inv.Dst] = true
	}
	for dst, seen := range want {
		if !seen {
			t.Fatalf("virtual broadcast must include %s", dst)
		}
	}

	ids := make([]string, 0, len(h.broadcasts))
	for id := range h.broadcasts {
		ids = append(ids, id)
	}
	for _, id := range ids[:len(ids)-1] {
		h.deliverAck(id)
		h.ctrlMw.tryCompleteTLBInvalidates()
	}
	if len(h.completions) != 0 {
		t.Fatal("partial acknowledgements must not complete the invalidation")
	}
	h.deliverAck(ids[len(ids)-1])
	h.expectCompletion()
	if len(h.completions) != 1 {
		t.Fatalf("the coordinator must return exactly one completion, got %d",
			len(h.completions))
	}
	if h.completions[0].RspTo != req.ID {
		t.Fatalf("the completion must correlate to the original request, got %s",
			h.completions[0].RspTo)
	}
}

// TestUVMForbiddenEndpoint proves the virtual-caching endpoint set contains no
// fabricated L1V/L1S TLB endpoints: the registered set and every broadcast
// destination are exactly the private L1I and the shared L2 TLB.
func TestUVMForbiddenEndpoint(t *testing.T) {
	h := newUVMInvalidateGMMUHarness(t, "L1I", "L2")
	defer h.ctrl.Finish()

	registered := map[sim.RemotePort]bool{}
	for _, endpoint := range h.comp.tlbEndpoints {
		registered[endpoint.AsRemote()] = true
	}
	for _, forbidden := range []sim.RemotePort{"L1V", "L1S", "L1V0", "L1S0"} {
		if registered[forbidden] {
			t.Fatalf("the virtual endpoint set must not contain %s", forbidden)
		}
	}
	if len(registered) != 2 || !registered["L1I"] || !registered["L2"] {
		t.Fatalf("the virtual endpoint set must be exactly {L1I, L2}, got %v",
			registered)
	}

	h.deliverReq(1, 0x10000, 64*1024)
	for _, inv := range h.broadcasts {
		if inv.Dst == "L1V" || inv.Dst == "L1S" {
			t.Fatalf("virtual broadcast must not target a fabricated data TLB %s",
				inv.Dst)
		}
	}
	if len(h.broadcasts) != 2 {
		t.Fatalf("virtual broadcast must reach exactly L1I and L2, got %d",
			len(h.broadcasts))
	}
}

// TestUVMAckCorrelation proves deterministic ack handling: out-of-order acks
// aggregate, duplicate and unknown acks are rejected without completing, a
// missing ack never completes, and the completion fires exactly once.
func TestUVMAckCorrelation(t *testing.T) {
	h := newUVMInvalidateGMMUHarness(t, "L1I", "L2")
	defer h.ctrl.Finish()

	req := h.deliverReq(1, 0x10000, 64*1024)
	ids := make([]string, 0, 2)
	for id := range h.broadcasts {
		ids = append(ids, id)
	}

	// Out-of-order: the second broadcast acknowledges first; no completion.
	h.deliverAck(ids[1])
	h.ctrlMw.tryCompleteTLBInvalidates()
	if len(h.completions) != 0 {
		t.Fatal("a single ack must not complete a two-endpoint invalidation")
	}

	// Duplicate ack for the same broadcast: rejected, no completion.
	h.deliverAck(ids[1])
	h.ctrlMw.tryCompleteTLBInvalidates()
	if len(h.completions) != 0 {
		t.Fatal("a duplicate ack must be rejected and never complete")
	}

	// Unknown ack: rejected, no completion.
	h.deliverAck("unknown-broadcast-id")
	h.ctrlMw.tryCompleteTLBInvalidates()
	if len(h.completions) != 0 {
		t.Fatal("an unknown ack must be rejected and never complete")
	}

	// The final ack completes exactly once.
	h.deliverAck(ids[0])
	h.expectCompletion()
	if len(h.completions) != 1 {
		t.Fatalf("the completion must fire exactly once after all acks, got %d",
			len(h.completions))
	}
	if h.completions[0].RspTo != req.ID {
		t.Fatalf("the completion must correlate to the original request, got %s",
			h.completions[0].RspTo)
	}
	if len(h.comp.activeTLBInvalidates) != 0 {
		t.Fatal("the command must be retired after completion")
	}

	// Missing ack: a fresh invalidation that never receives one ack stays
	// pending forever; no completion is ever emitted.
	h2 := newUVMInvalidateGMMUHarness(t, "L1I", "L2")
	h2.deliverReq(1, 0x10000, 64*1024)
	for id := range h2.broadcasts {
		h2.deliverAck(id)
		break
	}
	for i := 0; i < 3; i++ {
		h2.ctrlMw.tryCompleteTLBInvalidates()
	}
	if len(h2.completions) != 0 {
		t.Fatal("a missing ack must leave the command pending without completion")
	}
	if len(h2.comp.activeTLBInvalidates) != 1 {
		t.Fatal("a missing ack must leave the command registered")
	}
}