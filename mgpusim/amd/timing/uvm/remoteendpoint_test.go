package uvm

// sbin_codex: CPU-remote memory endpoint contract tests (plan todo 11 of
// mgpusim-uvm-manager). These plain Go tests drive the remote endpoint with
// the package mocks and assert: the CPU_REMOTE translation's CPU PA is
// consumed exactly and forwarded over modeled PCIe (RDMA) to global storage,
// the returned data reaches the original GPU requester, the region access
// counter increments once per served read, and INVALID / GPU_LOCAL / missing
// translations, unsupported atomics, and normal remote writes cause zero host
// mutation.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// atomicReq models an unsupported remote atomic: the selected MGPUSim
// memory-request protocol has no atomic request type (uvm-manager.md §15.1),
// so any AccessReq that is neither a read nor a write is an unsupported
// atomic.
type atomicReq struct {
	sim.MsgMeta
	addr uint64
	pid  vm.PID
}

func (r *atomicReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

func (r *atomicReq) Clone() sim.Msg {
	clone := *r
	clone.ID = sim.GetIDGenerator().Generate()
	return &clone
}

func (r *atomicReq) GetAddress() uint64  { return r.addr }
func (r *atomicReq) GetByteSize() uint64 { return 4 }
func (r *atomicReq) GetPID() vm.PID      { return r.pid }

// remoteEndpointHarness builds a RemoteEndpoint whose ToGPU and ToRDMA ports
// are mocks, wired to a real AccessCounter.
type remoteEndpointHarness struct {
	ctrl    *gomock.Controller
	e       *RemoteEndpoint
	toGPU   *MockPort
	toRDMA  *MockPort
	counter *AccessCounter
}

func newRemoteEndpointHarness(t *testing.T) *remoteEndpointHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	engine := NewMockEngine(ctrl)

	toGPU := NewMockPort(ctrl)
	toGPU.EXPECT().AsRemote().Return(sim.RemotePort("ToGPU")).AnyTimes()
	toRDMA := NewMockPort(ctrl)
	toRDMA.EXPECT().AsRemote().Return(sim.RemotePort("ToRDMA")).AnyTimes()

	counter := MakeAccessCounterBuilder().
		WithEngine(engine).
		WithFreq(1).
		Build("AccessCounter")

	e := MakeRemoteEndpointBuilder().
		WithEngine(engine).
		WithFreq(1).
		WithGPU(0).
		WithAccessCounter(counter).
		Build("RemoteEndpoint")
	e.ToGPU = toGPU
	e.ToRDMA = toRDMA

	return &remoteEndpointHarness{
		ctrl:    ctrl,
		e:       e,
		toGPU:   toGPU,
		toRDMA:  toRDMA,
		counter: counter,
	}
}

func TestUVMRemoteEndpointCPUAddress(t *testing.T) {
	h := newRemoteEndpointHarness(t)
	defer h.ctrl.Finish()

	original := mem.ReadReqBuilder{}.
		WithSrc(sim.RemotePort("Requester")).
		WithAddress(0x10000). // VA region base
		WithByteSize(64).
		WithPID(1).
		Build()
	original.Info = &RemoteAccessAnnotation{
		Location: vm.MemoryLocationCPU_REMOTE,
		PAddr:    0xDEAD0000, // exact CPU backing PA
	}

	var forwarded *mem.ReadReq
	h.toGPU.EXPECT().PeekIncoming().Return(original)
	h.toRDMA.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		forwarded = msg.(*mem.ReadReq)
		return nil
	})
	h.toGPU.EXPECT().RetrieveIncoming()
	h.toRDMA.EXPECT().PeekIncoming().Return(nil)
	if !h.e.Tick() {
		t.Fatal("the endpoint must make progress on the remote read")
	}
	if forwarded == nil {
		t.Fatal("the remote read must be forwarded over modeled PCIe")
	}
	if forwarded.Address != 0xDEAD0000 {
		t.Fatalf("the endpoint must consume the exact CPU PA, got 0x%x", forwarded.Address)
	}
	if forwarded.AccessByteSize != 64 || forwarded.PID != 1 {
		t.Fatalf("the forwarded read must preserve size and PID: %+v", forwarded)
	}
	if got := h.counter.Count(1, 0, 0x10000); got != 1 {
		t.Fatalf("each served remote read must increment the region counter once, got %d", got)
	}

	// The modeled-PCIe response returns the host data to the original GPU
	// requester.
	data := []byte{1, 2, 3, 4}
	rsp := mem.DataReadyRspBuilder{}.
		WithSrc(sim.RemotePort("ToRDMA")).
		WithDst(sim.RemotePort("ToRDMA")).
		WithRspTo(forwarded.ID).
		WithData(data).
		Build()
	var rspToGPU *mem.DataReadyRsp
	h.toGPU.EXPECT().PeekIncoming().Return(nil)
	h.toRDMA.EXPECT().PeekIncoming().Return(rsp)
	h.toGPU.EXPECT().Send(gomock.Any()).DoAndReturn(func(msg sim.Msg) *sim.SendError {
		rspToGPU = msg.(*mem.DataReadyRsp)
		return nil
	})
	h.toRDMA.EXPECT().RetrieveIncoming()
	if !h.e.Tick() {
		t.Fatal("the endpoint must make progress on the data response")
	}
	if rspToGPU == nil {
		t.Fatal("the host data must be returned to the GPU requester")
	}
	if rspToGPU.RespondTo != original.ID || rspToGPU.Dst != sim.RemotePort("Requester") {
		t.Fatalf("the response must target the original requester: %+v", rspToGPU)
	}
	if string(rspToGPU.Data) != string(data) {
		t.Fatal("the response must carry the host data")
	}
}

func TestUVMRejectInvalidRemoteAddress(t *testing.T) {
	h := newRemoteEndpointHarness(t)
	defer h.ctrl.Finish()

	cases := []struct {
		name string
		info interface{}
	}{
		{"invalid translation", &RemoteAccessAnnotation{
			Location: vm.MemoryLocationINVALID,
		}},
		{"gpu-local translation", &RemoteAccessAnnotation{
			Location: vm.MemoryLocationGPU_LOCAL,
			PAddr:    0x80000000,
		}},
		{"missing translation", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mem.ReadReqBuilder{}.
				WithAddress(0x10000).
				WithByteSize(4).
				WithPID(1).
				Build()
			req.Info = tc.info

			h.toGPU.EXPECT().PeekIncoming().Return(req)
			h.toGPU.EXPECT().RetrieveIncoming()
			h.toRDMA.EXPECT().PeekIncoming().Return(nil)
			// No ToRDMA.Send expectation: any host traffic fails the test.
			if !h.e.Tick() {
				t.Fatal("the endpoint must consume the rejected request")
			}
			if got := h.counter.Count(1, 0, 0x10000); got != 0 {
				t.Fatalf("a rejected translation must not increment the counter, got %d", got)
			}
		})
	}
}

func TestUVMRejectAtomic(t *testing.T) {
	h := newRemoteEndpointHarness(t)
	defer h.ctrl.Finish()

	atomic := &atomicReq{addr: 0x10000, pid: 1}
	atomic.ID = "atomic-1"
	atomic.Src = sim.RemotePort("Requester")

	h.toGPU.EXPECT().PeekIncoming().Return(atomic)
	h.toGPU.EXPECT().RetrieveIncoming()
	h.toRDMA.EXPECT().PeekIncoming().Return(nil)
	// No ToRDMA.Send expectation: an unsupported atomic must never be
	// forwarded to host memory.
	if !h.e.Tick() {
		t.Fatal("the endpoint must consume the atomic")
	}
	if got := h.e.RejectedAtomicCount(); got != 1 {
		t.Fatalf("the unsupported atomic must be explicitly rejected, got %d", got)
	}
	if got := h.counter.Count(1, 0, 0x10000); got != 0 {
		t.Fatalf("a rejected atomic must not increment the access counter, got %d", got)
	}
}

func TestUVMRejectRemoteWrite(t *testing.T) {
	h := newRemoteEndpointHarness(t)
	defer h.ctrl.Finish()

	write := mem.WriteReqBuilder{}.
		WithAddress(0x10000).
		WithPID(1).
		WithData([]byte{1, 2, 3, 4}).
		WithDirtyMask([]bool{true, true, true, true}).
		Build()
	write.Info = &RemoteAccessAnnotation{
		Location: vm.MemoryLocationCPU_REMOTE,
		PAddr:    0xDEAD0000,
	}

	h.toGPU.EXPECT().PeekIncoming().Return(write)
	h.toGPU.EXPECT().RetrieveIncoming()
	h.toRDMA.EXPECT().PeekIncoming().Return(nil)
	// No ToRDMA.Send expectation: a normal remote write is parked and never
	// committed to host memory (uvm-manager.md §15).
	if !h.e.Tick() {
		t.Fatal("the endpoint must consume the parked write")
	}
	if got := h.counter.Count(1, 0, 0x10000); got != 0 {
		t.Fatalf("a parked remote write must not increment the access counter, got %d", got)
	}
}
