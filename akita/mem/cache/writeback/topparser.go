package writeback

import (
	"github.com/sarchlab/akita/v4/mem/cache" // sbin_codex: UVM range admission gate (plan todo 13).
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type topParser struct {
	cache *Comp
}

func (p *topParser) Tick() bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	req := p.cache.topPort.PeekIncoming()
	if req == nil {
		return false
	}

	// sbin_codex: block new matching admissions while a UVM range flush is
	// active so drained lines are not repopulated (plan todo 13 of
	// mgpusim-uvm-manager, uvm-manager.md section 19.1). Unrelated traffic
	// keeps flowing.
	if p.cache.uvmRangeFlusher.active != nil {
		access, ok := req.(mem.AccessReq)
		if ok && p.cache.uvmRangeFlusher.active.matcher.MatchAccess(
			access.GetPID(), access.GetAddress(), cache.ResolveAnnotation(access)) {
			return false
		}
	}

	if !p.cache.dirStageBuffer.CanPush() {
		return false
	}

	trans := &transaction{
		id: sim.GetIDGenerator().Generate(),
	}

	switch req := req.(type) {
	case *mem.ReadReq:
		trans.read = req
	case *mem.WriteReq:
		trans.write = req
	}

	p.cache.dirStageBuffer.Push(trans)

	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)

	tracing.TraceReqReceive(req, p.cache)

	p.cache.topPort.RetrieveIncoming()

	return true
}
