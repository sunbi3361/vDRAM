package tlb

// sbin_codex: UVM range TLB invalidation contract tests (plan todo 14 of
// mgpusim-uvm-manager). These plain Go tests drive the TLB control middleware
// with real sets and assert: overlap invalidation scoped by PID/ASID + aligned
// 64 KB region, exactly one aggregated response per request, no pause, no MSHR
// reset, and unrelated entries/progress preserved (uvm-manager.md §21.1).

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// uvmInvalidateHarness builds a TLB with mocked ports and a single real set so
// the control middleware can be driven deterministically.
type uvmInvalidateHarness struct {
	ctrl          *gomock.Controller
	tlb           *Comp
	mw            *tlbMiddleware
	ctrlMw        *ctrlMiddleware
	set           internal.Set
	topPort       *MockPort
	bottomPort    *MockPort
	controlPort   *MockPort
	addressMapper *MockAddressToPortMapper
}

func newUVMInvalidateHarness(t *testing.T) *uvmInvalidateHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)
	set := internal.NewSet(8)
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
		Build("TLB")
	tlb.topPort = topPort
	tlb.bottomPort = bottomPort
	tlb.controlPort = controlPort
	tlb.sets = []internal.Set{set}
	tlb.state = tlbStateEnable

	mw := tlb.Middlewares()[1].(*tlbMiddleware)
	ctrlMw := tlb.Middlewares()[0].(*ctrlMiddleware)

	return &uvmInvalidateHarness{
		ctrl:          ctrl,
		tlb:           tlb,
		mw:            mw,
		ctrlMw:        ctrlMw,
		set:           set,
		topPort:       topPort,
		bottomPort:    bottomPort,
		controlPort:   controlPort,
		addressMapper: addressMapper,
	}
}

// seed inserts a valid page into the harness set at the given way.
func (h *uvmInvalidateHarness) seed(wayID int, page vm.Page) {
	page.Valid = true
	h.set.Update(wayID, page)
}

// valid reports whether the seeded entry for pid/vAddr is still valid.
func (h *uvmInvalidateHarness) valid(pid vm.PID, vAddr uint64) bool {
	_, page, found := h.set.Lookup(pid, vAddr)
	return found && page.Valid
}

// deliverInvalidate injects a UVMTLBInvalidateReq into the control port and
// captures the exactly-one response.
func (h *uvmInvalidateHarness) deliverInvalidate(
	pid vm.PID, startVA, size uint64,
) (*UVMTLBInvalidateReq, *UVMTLBInvalidateRsp) {
	req := &UVMTLBInvalidateReq{PID: pid, StartVA: startVA, Size: size}
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = sim.RemotePort("CP")
	req.Dst = sim.RemotePort("ControlPort")

	var rsp *UVMTLBInvalidateRsp
	h.controlPort.EXPECT().PeekIncoming().Return(req)
	h.controlPort.EXPECT().CanSend().Return(true)
	h.controlPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(r *UVMTLBInvalidateRsp) error {
			rsp = r
			return nil
		})
	h.controlPort.EXPECT().RetrieveIncoming()

	if !h.ctrlMw.handleIncomingCommands() {
		panic("handleIncomingCommands must consume the invalidation request")
	}
	return req, rsp
}

// TestUVMRangeInvalidate proves the TLB invalidates exactly the entries whose
// PID matches and whose covered VA range overlaps the requested 64 KB region,
// and replies with exactly one response. Unrelated entries survive.
func TestUVMRangeInvalidate(t *testing.T) {
	h := newUVMInvalidateHarness(t)
	defer h.ctrl.Finish()

	// Entries inside the region [0x10000, 0x20000): both must be invalidated.
	h.seed(0, vm.Page{PID: 1, VAddr: 0x10000, PAddr: 0x1000, PageSize: 4096})
	h.seed(1, vm.Page{PID: 1, VAddr: 0x18000, PAddr: 0x2000, PageSize: 4096})
	// A 2 MB entry whose covered range [0, 0x200000) overlaps the region.
	h.seed(2, vm.Page{PID: 1, VAddr: 0x00000, PAddr: 0x3000, PageSize: 2 * 1024 * 1024})
	// A 4 KB entry ending exactly at the region start: no overlap.
	h.seed(3, vm.Page{PID: 1, VAddr: 0x0F000, PAddr: 0x4000, PageSize: 4096})
	// An entry starting exactly at the region end: no overlap.
	h.seed(4, vm.Page{PID: 1, VAddr: 0x20000, PAddr: 0x5000, PageSize: 4096})
	// A different PID with an overlapping VA: preserved.
	h.seed(5, vm.Page{PID: 2, VAddr: 0x10000, PAddr: 0x6000, PageSize: 4096})
	// An unrelated VA in the same PID: preserved.
	h.seed(6, vm.Page{PID: 1, VAddr: 0x40000, PAddr: 0x7000, PageSize: 4096})

	req, rsp := h.deliverInvalidate(1, 0x10000, 64*1024)
	if rsp == nil {
		t.Fatal("the TLB must reply with exactly one response")
	}
	if rsp.RspTo != req.ID {
		t.Fatalf("the response must correlate to the request %s, got %s",
			req.ID, rsp.RspTo)
	}
	if rsp.Dst != sim.RemotePort("CP") {
		t.Fatalf("the response must be addressed back to the requester, got %s", rsp.Dst)
	}

	if h.valid(1, 0x10000) {
		t.Fatal("entry 0x10000 must be invalidated")
	}
	if h.valid(1, 0x18000) {
		t.Fatal("entry 0x18000 must be invalidated")
	}
	if h.valid(1, 0x00000) {
		t.Fatal("the 2 MB entry overlapping the region must be invalidated")
	}
	if !h.valid(1, 0x0F000) {
		t.Fatal("an entry ending at the region start must not be invalidated")
	}
	if !h.valid(1, 0x20000) {
		t.Fatal("an entry starting at the region end must not be invalidated")
	}
	if !h.valid(2, 0x10000) {
		t.Fatal("an entry of another PID must not be invalidated")
	}
	if !h.valid(1, 0x40000) {
		t.Fatal("an unrelated entry of the same PID must not be invalidated")
	}
}

// TestUVMNoPause proves the invalidation never pauses the TLB, never resets
// MSHRs, and unrelated in-flight progress completes after the invalidation.
func TestUVMNoPause(t *testing.T) {
	h := newUVMInvalidateHarness(t)
	defer h.ctrl.Finish()

	// An overlapping entry and an unrelated in-flight translation request.
	h.seed(0, vm.Page{PID: 1, VAddr: 0x10000, PAddr: 0x1000, PageSize: 4096})
	h.seed(1, vm.Page{PID: 1, VAddr: 0x40000, PAddr: 0x2000, PageSize: 4096})

	// The unrelated request misses (the real set has no entry at 0x30000) and
	// fetches from the bottom, creating an MSHR entry.
	h.addressMapper.EXPECT().
		Find(uint64(0x30000)).
		Return(sim.RemotePort("Low")).
		AnyTimes()
	var fetch *vm.TranslationReq
	h.bottomPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(req *vm.TranslationReq) error {
			fetch = req
			return nil
		})
	req := vm.TranslationReqBuilder{}.
		WithSrc(sim.RemotePort("Agent")).
		WithPID(1).
		WithVAddr(0x30000).
		WithDeviceID(1).
		Build()
	if !h.mw.lookup(req) {
		t.Fatal("the unrelated request must miss into the MSHR")
	}
	entry := h.tlb.mshr.GetEntry(1, 0x30000)
	if entry == nil || len(entry.Requests) != 1 {
		t.Fatalf("the unrelated request must be retained in the MSHR, got %+v", entry)
	}

	// The invalidation lands while the request is in flight.
	_, rsp := h.deliverInvalidate(1, 0x10000, 64*1024)
	if rsp == nil {
		t.Fatal("the invalidation must be acknowledged")
	}
	if h.tlb.state != tlbStateEnable {
		t.Fatalf("the TLB must not be paused, got state %d", h.tlb.state)
	}
	entry = h.tlb.mshr.GetEntry(1, 0x30000)
	if entry == nil || len(entry.Requests) != 1 {
		t.Fatal("the MSHR must not be reset by the invalidation")
	}
	if h.valid(1, 0x10000) {
		t.Fatal("the overlapping entry must be invalidated")
	}
	if !h.valid(1, 0x40000) {
		t.Fatal("the unrelated entry must survive")
	}

	// The unrelated translation completes after the invalidation: the set
	// receives the valid page and the waiter is answered.
	validPage := vm.Page{PID: 1, VAddr: 0x30000, PAddr: 0x9000, PageSize: 4096}
	validRsp := vm.TranslationRspBuilder{}.
		WithSrc(sim.RemotePort("Low")).
		WithRspTo(fetch.ID).
		WithPage(validPage).
		Build()
	h.bottomPort.EXPECT().PeekIncoming().Return(validRsp)
	h.bottomPort.EXPECT().RetrieveIncoming()
	if !h.mw.parseBottom() {
		t.Fatal("parseBottom must consume the valid response")
	}
	h.topPort.EXPECT().
		Send(gomock.Any()).
		DoAndReturn(func(r *vm.TranslationRsp) error {
			if r.RespondTo != req.ID {
				t.Fatalf("the response must answer the unrelated waiter, got %s", r.RespondTo)
			}
			return nil
		})
	if !h.mw.respondMSHREntry() {
		t.Fatal("the unrelated waiter must be answered after the invalidation")
	}
	if !h.tlb.mshr.IsEmpty() {
		t.Fatal("the MSHR must be drained by the unrelated completion")
	}
}