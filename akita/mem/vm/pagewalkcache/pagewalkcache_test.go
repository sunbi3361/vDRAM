package pagewalkcache

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
)

func TestLookupReturnsMissWithoutWaitingForFill(t *testing.T) {
	h := newTestHarness(0)

	miss := lookupReq(h.topPort, vm.PID(1), 0x1000)
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
		WithLevel(1).
		Build()
	h.topPort.Deliver(fill)
	next := lookupReq(h.topPort, vm.PID(1), 0x1000)
	h.topPort.Deliver(next)
	if !h.cache.Tick() {
		t.Fatal("fill and next lookup made no progress")
	}

	hit, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok || !hit.Hit {
		t.Fatalf("post-fill response = %#v, want hit", hit)
	}
	if hit.Level != 1 {
		t.Fatalf("post-fill hit level = %d, want 1", hit.Level)
	}
}

func TestFillIsScopedByPID(t *testing.T) {
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

	levelHit := lookupReq(h.topPort, vm.PID(7), 0x4000)
	pidMiss := lookupReq(h.topPort, vm.PID(8), 0x4000)
	h.topPort.Deliver(levelHit)
	h.topPort.Deliver(pidMiss)
	h.cache.Tick()

	levelRsp := h.topPort.RetrieveOutgoing().(*LookupRsp)
	pidRsp := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !levelRsp.Hit || levelRsp.Level != 1 {
		t.Fatalf("same-level response = %#v, want level 1 hit", levelRsp)
	}
	if pidRsp.Hit {
		t.Fatal("entry leaked across PID")
	}
}

func TestMissDoesNotBlockFollowingLookups(t *testing.T) {
	h := newTestHarness(2)
	first := lookupReq(h.topPort, vm.PID(1), 0x1000)
	second := lookupReq(h.topPort, vm.PID(1), 0x2000)
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

func TestLevelZeroIsNeverCached(t *testing.T) {
	h := newTestHarness(0)
	fill := FillReqBuilder{}.
		WithSrc("GMMU").
		WithDst(h.topPort.AsRemote()).
		WithPID(vm.PID(1)).
		WithVAddr(0x1000).
		WithLevel(0).
		Build()
	h.topPort.Deliver(fill)
	h.cache.Tick()

	req := lookupReq(h.topPort, vm.PID(1), 0x1000)
	h.topPort.Deliver(req)
	h.cache.Tick()

	rsp, ok := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !ok || rsp.Hit {
		t.Fatalf("level-zero-fill response = %#v, want miss", rsp)
	}
	if rsp.Level != -1 {
		t.Fatalf("level-zero-fill response level = %d, want -1", rsp.Level)
	}
}

func TestLookupReturnsDeepestHitLevel(t *testing.T) {
	h := newTestHarness(0)
	for _, level := range []int{4, 2, 1} {
		fill := FillReqBuilder{}.
			WithSrc("GMMU").
			WithDst(h.topPort.AsRemote()).
			WithPID(vm.PID(1)).
			WithVAddr(0x123456789000).
			WithLevel(level).
			Build()
		h.topPort.Deliver(fill)
	}
	h.cache.Tick()

	h.topPort.Deliver(lookupReq(h.topPort, vm.PID(1), 0x123456789000))
	h.cache.Tick()

	rsp := h.topPort.RetrieveOutgoing().(*LookupRsp)
	if !rsp.Hit || rsp.Level != 1 {
		t.Fatalf("aggregate response = %#v, want deepest level 1 hit", rsp)
	}
}

func TestCacheHasEqualFullyAssociativeEntriesPerCacheableLevel(t *testing.T) {
	h := newTestHarness(0)

	for level := 1; level <= 4; level++ {
		if len(h.cache.sets[level].Blocks) != 2 {
			t.Fatalf("level %d entry count = %d, want 2", level, len(h.cache.sets[level].Blocks))
		}
		if len(h.cache.sets[level].LRU) != 2 {
			t.Fatalf("level %d associativity metadata = %d, want 2", level, len(h.cache.sets[level].LRU))
		}
	}
	if len(h.cache.sets[0].Blocks) != 0 {
		t.Fatalf("level 0 entry count = %d, want 0", len(h.cache.sets[0].Blocks))
	}
}

func TestPageTableSegmentUsesCumulativeUpperVPNPrefix(t *testing.T) {
	vpn := (uint64(1) << 36) | (uint64(2) << 27) |
		(uint64(3) << 18) | (uint64(4) << 9) | uint64(5)
	vAddr := vpn << 12

	tests := []struct {
		level int
		want  uint64
	}{
		{level: 4, want: 1},
		{level: 3, want: (uint64(1) << 9) | 2},
		{level: 2, want: (uint64(1) << 18) | (uint64(2) << 9) | 3},
		{level: 1, want: (uint64(1) << 27) | (uint64(2) << 18) | (uint64(3) << 9) | 4},
	}

	for _, tt := range tests {
		got := pageTableSegment(vAddr, 12, 9, 5, tt.level)
		if got != tt.want {
			t.Fatalf("level %d segment = %#x, want %#x", tt.level, got, tt.want)
		}
	}
}
