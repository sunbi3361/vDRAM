package idealtlb

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/tracing"
)

// transMiddleware resolves translation requests directly from the page table.
type transMiddleware struct {
	*Comp
}

// Tick processes incoming translation requests.
func (m *transMiddleware) Tick() bool {
	return m.parseFromTop()
}

func (m *transMiddleware) parseFromTop() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		req := m.topPort.PeekIncoming()
		if req == nil {
			break
		}

		if !m.topPort.CanSend() {
			break
		}

		m.topPort.RetrieveIncoming()
		tracing.TraceReqReceive(req, m.Comp)

		switch req := req.(type) {
		case *vm.TranslationReq:
			madeProgress = m.handleTranslation(req) || madeProgress
		default:
			// sbin_codex: mirror gmmuMiddleware.go panic for unknown types.
			log.Panicf("idealtlb cannot handle request of type %s", reflect.TypeOf(req))
		}
	}

	return madeProgress
}

func (m *transMiddleware) handleTranslation(req *vm.TranslationReq) bool {
	page, found := m.pageTable.Find(req.PID, req.VAddr)
	if !found {
		// sbin_codex: ideal TLB panics exactly like the GMMU on a missing page.
		panic("page not found")
	}

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		Build()

	m.topPort.Send(rsp)
	tracing.TraceReqComplete(req, m.Comp)

	return true
}
