package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/pagewalkcache"
	"github.com/sarchlab/akita/v4/sim"
)

func newResponseTestMiddleware() *middleware {
	comp := &Comp{latency: 10}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUTest",
		sim.NewSerialEngine(),
		sim.GHz,
		nil,
	)
	return &middleware{Comp: comp}
}

func TestHandlePageWalkCacheResponseUsesDeepestHit(t *testing.T) {
	// sbin_codex: Given
	m := newResponseTestMiddleware()
	m.walkingTranslations = []transaction{{
		req:   &vm.TranslationReq{},
		level: pageTableLevels - 1,
		state: sentToPageWalkCache,
		msgID: "lookup",
	}}

	// sbin_codex: When
	madeProgress := m.handlePageWalkCacheResponse(&pagewalkcache.LookupRsp{
		RspTo: "lookup",
		Hit:   true,
		Level: 2,
	})

	// sbin_codex: Then
	if !madeProgress {
		t.Fatal("aggregate hit did not make progress")
	}
	trans := m.walkingTranslations[0]
	if trans.level != 1 {
		t.Fatalf("remaining walk level = %d, want 1", trans.level)
	}
	if trans.cycleLeft != 20 {
		t.Fatalf("remaining walk cycles = %d, want 20", trans.cycleLeft)
	}
	if trans.fillLevel != 1 {
		t.Fatalf("fill level = %d, want 1", trans.fillLevel)
	}
	if trans.state != pageWalkCacheDone {
		t.Fatalf("transaction state = %v, want pageWalkCacheDone", trans.state)
	}
}

func TestHandlePageWalkCacheResponseMissWalksAndFillsFromRoot(t *testing.T) {
	// sbin_codex: Given
	m := newResponseTestMiddleware()
	m.walkingTranslations = []transaction{{
		req:   &vm.TranslationReq{},
		level: pageTableLevels - 1,
		state: sentToPageWalkCache,
		msgID: "lookup",
	}}

	// sbin_codex: When
	madeProgress := m.handlePageWalkCacheResponse(&pagewalkcache.LookupRsp{
		RspTo: "lookup",
		Level: -1,
	})

	// sbin_codex: Then
	if !madeProgress {
		t.Fatal("aggregate miss did not make progress")
	}
	trans := m.walkingTranslations[0]
	// sbin_claude: the modeled page table is 4-level (target spec), so an
	// aggregate miss restarts at level 3 and costs 4 x 10 cycles.
	if trans.level != pageTableLevels-1 {
		t.Fatalf("miss walk level = %d, want 3", trans.level)
	}
	if trans.cycleLeft != 40 {
		t.Fatalf("miss walk cycles = %d, want 40", trans.cycleLeft)
	}
	if trans.fillLevel != pageTableLevels-1 {
		t.Fatalf("miss fill level = %d, want 3", trans.fillLevel)
	}
}

func TestHandlePageWalkCacheResponseAtLevelOneLeavesLeafUncached(t *testing.T) {
	// sbin_codex: Given
	m := newResponseTestMiddleware()
	m.walkingTranslations = []transaction{{
		req:   &vm.TranslationReq{},
		level: pageTableLevels - 1,
		state: sentToPageWalkCache,
		msgID: "lookup",
	}}

	// sbin_codex: When
	m.handlePageWalkCacheResponse(&pagewalkcache.LookupRsp{
		RspTo: "lookup",
		Hit:   true,
		Level: lowestPageWalkCacheLevel,
	})

	// sbin_codex: Then
	trans := m.walkingTranslations[0]
	if trans.level != 0 {
		t.Fatalf("remaining walk level = %d, want leaf level 0", trans.level)
	}
	if trans.fillLevel != 0 {
		t.Fatalf("fill level = %d, want leaf level 0 excluded from cache", trans.fillLevel)
	}
}
