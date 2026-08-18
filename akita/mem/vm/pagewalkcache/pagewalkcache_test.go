package pagewalkcache

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
)

func TestLookupReturnsMissWithoutWaitingForFill(t *testing.T) {
	h := newTestHarness(0)

	miss := lookupReq(h.topPort, vm.PID(1), 0x1000, 0)
	if err := h.topPort.Deliver(miss); err != nil {
		t.Fatal(err)
	}
	if !h.cache.Tick() {
		t.Fatal("miss lookup made no progress")
	}

	rsp, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok {
		t.Fatalf("response type = %T, want *LookupRsp", h.topPort.PeekOutgoing())
	}
	if rsp.Hit {
		t.Fatal("empty cache returned a hit")
	}
	if rsp.RspTo != miss.ID {
		t.Fatalf("response correlation = %q, want %q", rsp.RspTo, miss.ID)
	}

	fill := FillReqBuilder{}.
		WithSrc("GMMU").
		WithDst(h.topPort.AsRemote()).
		WithPID(vm.PID(1)).
		WithVAddr(0x1000).
		WithLevel(0).
		Build()
	h.topPort.Deliver(fill)
	next := lookupReq(h.topPort, vm.PID(1), 0x1000, 0)
	h.topPort.Deliver(next)
	if !h.cache.Tick() {
		t.Fatal("fill and next lookup made no progress")
	}

	hit, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok || !hit.Hit {
		t.Fatalf("post-fill response = %#v, want hit", hit)
	}
}

func TestFillIsScopedByLevelAndPID(t *testing.T) {
	h := newTestHarness(0)

	fill := FillReqBuilder{}.
		WithSrc("GMMU").
		WithDst(h.topPort.AsRemote()).
		WithPID(vm.PID(7)).
		WithVAddr(0x4000).
		WithLevel(1).
		Build()
	h.topPort.Deliver(fill)
	h.cache.Tick()

	levelMiss := lookupReq(h.topPort, vm.PID(7), 0x4000, 0)
	pidMiss := lookupReq(h.topPort, vm.PID(8), 0x4000, 1)
	h.topPort.Deliver(levelMiss)
	h.topPort.Deliver(pidMiss)
	h.cache.Tick()

	levelRsp := h.topPort.RetrieveOutgoing().(*LookupRsp)
	pidRsp := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if levelRsp.Hit || pidRsp.Hit {
		t.Fatal("entry leaked across level or PID")
	}
}

func TestMissDoesNotBlockFollowingLookups(t *testing.T) {
	h := newTestHarness(2)
	first := lookupReq(h.topPort, vm.PID(1), 0x1000, 0)
	second := lookupReq(h.topPort, vm.PID(1), 0x2000, 0)
	h.topPort.Deliver(first)
	h.topPort.Deliver(second)

	for cycle := 0; cycle < 3; cycle++ {
		h.cache.Tick()
		if h.topPort.PeekOutgoing() != nil {
			t.Fatalf("miss response appeared at cycle %d", cycle)
		}
	}
	h.cache.Tick()

	firstRsp, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok || firstRsp.Hit {
		t.Fatalf("first response = %#v, want miss", firstRsp)
	}
	secondRsp, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok || secondRsp.Hit {
		t.Fatalf("second response = %#v, want miss", secondRsp)
	}
	if firstRsp.RspTo != first.ID || secondRsp.RspTo != second.ID {
		t.Fatal("lookup responses lost request correlation")
	}
}

func TestInvalidLevelReturnsMiss(t *testing.T) {
	h := newTestHarness(0)
	req := lookupReq(h.topPort, vm.PID(1), 0x1000, 99)
	h.topPort.Deliver(req)
	h.cache.Tick()

	rsp, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok || rsp.Hit {
		t.Fatalf("invalid-level response = %#v, want miss", rsp)
	}
}
