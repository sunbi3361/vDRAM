package addresstranslator

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type transaction struct {
	incomingReqs    []mem.AccessReq
	translationReq  *vm.TranslationReq
	translationRsp  *vm.TranslationRsp
	translationDone bool
}

type reqToBottom struct {
	reqFromTop  mem.AccessReq
	reqToBottom mem.AccessReq
}

// Comp is an AddressTranslator that forwards the read/write requests with
// the address translated from virtual to physical.
type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	topPort          sim.Port
	bottomPort       sim.Port
	remoteBottomPort sim.Port // sbin_codex
	translationPort  sim.Port
	ctrlPort         sim.Port

	log2PageSize                 uint64
	deviceID                     uint64
	numReqPerCycle               int
	memoryPortMapper             mem.AddressToPortMapper
	remoteMemoryPortMapper       mem.AddressToPortMapper // sbin_codex
	translationPortMapper        mem.AddressToPortMapper
	physicalAddressPassthrough   bool // sbin_codex: PID zero is already physical only at an explicit boundary.
	virtualAddressForLocalMemory bool // sbin_codex

	isFlushing bool

	transactions        []*transaction
	inflightReqToBottom []reqToBottom

	// sbin_codex: requests the UVM access counter refused to perform remotely.
	// They are translated again, which now yields a GPU-local mapping.
	retryReqs []mem.AccessReq
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

type middleware struct {
	*Comp
}

// Tick updates state at each cycle.
func (m *middleware) Tick() bool {
	madeProgress := false

	if !m.isFlushing {
		madeProgress = m.runPipeline()
	} else {
		for i := 0; i < m.numReqPerCycle; i++ {
			madeProgress = m.parseTranslation() || madeProgress
		}
	}

	madeProgress = m.handleCtrlRequest() || madeProgress

	return madeProgress
}

func (m *middleware) runPipeline() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.respond() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseTranslation() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.translate() || madeProgress
	}

	return madeProgress
}

func (m *middleware) translate() bool {
	// sbin_codex: a UVM retry is re-translated ahead of new top traffic so a
	// stalled write is released as soon as its page becomes GPU-local.
	if len(m.retryReqs) > 0 {
		if m.retranslate(m.retryReqs[0]) {
			m.retryReqs = m.retryReqs[1:]
			return true
		}

		return false
	}

	item := m.topPort.PeekIncoming()
	if item == nil {
		return false
	}

	req := item.(mem.AccessReq)
	if m.physicalAddressPassthrough && req.GetPID() == 0 {
		// pageBase := req.GetAddress() &^ ((uint64(1) << m.log2PageSize) - 1) // sbin_codex: pre-edit helper input.
		// forwarded := m.createTranslatedReq(req, vm.Page{PAddr: pageBase, VAddr: pageBase, PageSize: 1 << m.log2PageSize}) // sbin_codex: pre-edit helper signature.
		forwarded := m.createTranslatedReq(req, translationRoute{ // sbin_codex
			port: m.bottomPort, mapper: m.memoryPortMapper,
			address: req.GetAddress(), pid: req.GetPID(),
		})
		if err := m.bottomPort.Send(forwarded); err != nil {
			return false
		}
		m.inflightReqToBottom = append(m.inflightReqToBottom, reqToBottom{
			reqFromTop:  req,
			reqToBottom: forwarded,
		})
		m.topPort.RetrieveIncoming()
		return true
	}
	vAddr := req.GetAddress()
	vPageID := m.addrToPageID(vAddr)

	_, isWrite := req.(*mem.WriteReq) // sbin_codex: UVM write-immediate migration.
	transReq := vm.TranslationReqBuilder{}.
		WithSrc(m.translationPort.AsRemote()).
		WithDst(m.translationPortMapper.Find(vAddr)).
		WithPID(req.GetPID()).
		WithVAddr(vPageID).
		WithDeviceID(m.deviceID).
		WithIsWrite(isWrite).
		Build()

	err := m.translationPort.Send(transReq)
	if err != nil {
		return false
	}

	translation := &transaction{
		incomingReqs:   []mem.AccessReq{req},
		translationReq: transReq,
	}
	m.transactions = append(m.transactions, translation)

	tracing.TraceReqReceive(req, m.Comp)
	tracing.TraceReqInitiate(
		transReq,
		m.Comp,
		tracing.MsgIDAtReceiver(req, m.Comp),
	)

	m.topPort.RetrieveIncoming()

	return true
}

// retranslate re-issues the translation of a request that must not use its
// previous mapping. // sbin_codex
func (m *middleware) retranslate(req mem.AccessReq) bool {
	_, isWrite := req.(*mem.WriteReq)
	transReq := vm.TranslationReqBuilder{}.
		WithSrc(m.translationPort.AsRemote()).
		WithDst(m.translationPortMapper.Find(req.GetAddress())).
		WithPID(req.GetPID()).
		WithVAddr(m.addrToPageID(req.GetAddress())).
		WithDeviceID(m.deviceID).
		WithIsWrite(isWrite).
		Build()

	if err := m.translationPort.Send(transReq); err != nil {
		return false
	}

	m.transactions = append(m.transactions, &transaction{
		incomingReqs:   []mem.AccessReq{req},
		translationReq: transReq,
	})

	return true
}

// handleRemoteRetry moves a refused remote request back to the retry queue.
// sbin_codex
func (m *middleware) handleRemoteRetry(rsp *vm.UVMRemoteRetryRsp) bool {
	if !m.isReqInBottomByID(rsp.RespondTo) {
		m.remoteBottomPort.RetrieveIncoming()
		return true
	}

	combo := m.findReqToBottomByID(rsp.RespondTo)
	m.retryReqs = append(m.retryReqs, combo.reqFromTop)
	m.removeReqToBottomByID(rsp.RespondTo)
	m.remoteBottomPort.RetrieveIncoming()

	return true
}

func (m *middleware) parseTranslation() bool {
	rsp := m.translationPort.PeekIncoming()
	if rsp == nil {
		return false
	}

	transRsp := rsp.(*vm.TranslationRsp)
	transaction := m.findTranslationByReqID(transRsp.RespondTo)

	if transaction == nil {
		m.translationPort.RetrieveIncoming()
		return true
	}

	transaction.translationRsp = transRsp
	transaction.translationDone = true

	reqFromTop := transaction.incomingReqs[0]
	// translatedReq := m.createTranslatedReq(reqFromTop, transaction.translationRsp.Page) // sbin_codex: pre-edit local-only route.
	// err := m.bottomPort.Send(translatedReq)
	route := m.routeTranslation(reqFromTop, transaction.translationRsp.Page) // sbin_codex
	translatedReq := m.createTranslatedReq(reqFromTop, route)
	err := route.port.Send(translatedReq)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(translatedReq, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		// m.bottomPort.Name(), // sbin_codex: pre-edit local-only milestone.
		route.port.Name(), // sbin_codex
		m.Comp.Name(),
		m.Comp,
	)

	m.inflightReqToBottom = append(m.inflightReqToBottom,
		reqToBottom{
			reqFromTop:  reqFromTop,
			reqToBottom: translatedReq,
		})
	transaction.incomingReqs = transaction.incomingReqs[1:]

	if len(transaction.incomingReqs) == 0 {
		m.removeExistingTranslation(transaction)
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(reqFromTop, m.Comp),
		tracing.MilestoneKindTranslation,
		"translation",
		m.Comp.Name(),
		m.Comp,
	)

	m.translationPort.RetrieveIncoming()

	tracing.TraceReqFinalize(transaction.translationReq, m.Comp)
	tracing.TraceReqInitiate(translatedReq, m.Comp,
		tracing.MsgIDAtReceiver(reqFromTop, m.Comp))

	return true
}

//nolint:funlen,gocyclo
func (m *middleware) respond() bool {
	// rsp := m.bottomPort.PeekIncoming() // sbin_codex: pre-edit local-only response.
	rspPort := m.bottomPort // sbin_codex
	rsp := rspPort.PeekIncoming()
	if rsp == nil && m.remoteBottomPort != nil { // sbin_codex
		rspPort = m.remoteBottomPort
		rsp = rspPort.PeekIncoming()
	}
	if rsp == nil {
		return false
	}

	var (
		reqFromTop       mem.AccessReq
		reqToBottomCombo reqToBottom
		rspToTop         mem.AccessRsp
	)

	reqInBottom := false

	if retry, ok := rsp.(*vm.UVMRemoteRetryRsp); ok { // sbin_codex
		return m.handleRemoteRetry(retry)
	}

	switch rsp := rsp.(type) {
	case *mem.DataReadyRsp:
		reqInBottom = m.isReqInBottomByID(rsp.RespondTo)
		if reqInBottom {
			reqToBottomCombo = m.findReqToBottomByID(rsp.RespondTo)
			reqFromTop = reqToBottomCombo.reqFromTop
			drToTop := mem.DataReadyRspBuilder{}.
				WithSrc(m.topPort.AsRemote()).
				WithDst(reqFromTop.Meta().Src).
				WithRspTo(reqFromTop.Meta().ID).
				WithData(rsp.Data).
				Build()
			rspToTop = drToTop
			tracing.AddMilestone(
				tracing.MsgIDAtReceiver(reqFromTop, m.Comp),
				tracing.MilestoneKindData,
				"data",
				m.Comp.Name(),
				m.Comp,
			)
		}
	case *mem.WriteDoneRsp:
		reqInBottom = m.isReqInBottomByID(rsp.RespondTo)
		if reqInBottom {
			reqToBottomCombo = m.findReqToBottomByID(rsp.RespondTo)
			reqFromTop = reqToBottomCombo.reqFromTop
			rspToTop = mem.WriteDoneRspBuilder{}.
				WithSrc(m.topPort.AsRemote()).
				WithDst(reqFromTop.Meta().Src).
				WithRspTo(reqFromTop.Meta().ID).
				Build()
			tracing.AddMilestone(
				tracing.MsgIDAtReceiver(reqFromTop, m.Comp),
				tracing.MilestoneKindSubTask,
				"subtask",
				m.Comp.Name(),
				m.Comp,
			)
		}
	default:
		log.Panicf("cannot handle respond of type %s", reflect.TypeOf(rsp))
	}

	if reqInBottom {
		err := m.topPort.Send(rspToTop)
		if err != nil {
			return false
		}

		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(reqFromTop, m.Comp),
			tracing.MilestoneKindNetworkBusy,
			m.topPort.Name(),
			m.Comp.Name(),
			m.Comp,
		)

		m.removeReqToBottomByID(rsp.(mem.AccessRsp).GetRspTo())

		tracing.TraceReqFinalize(reqToBottomCombo.reqToBottom, m.Comp)
		tracing.TraceReqComplete(reqToBottomCombo.reqFromTop, m.Comp)
	}

	// m.bottomPort.RetrieveIncoming() // sbin_codex: pre-edit local-only dequeue.
	rspPort.RetrieveIncoming() // sbin_codex

	return true
}

// sbin_codex: pre-edit createTranslatedReq/read/write helpers moved to routing.go
// so dual-egress route selection remains isolated from pipeline control.

func (m *middleware) addrToPageID(addr uint64) uint64 {
	return (addr >> m.log2PageSize) << m.log2PageSize
}

func (m *middleware) findTranslationByReqID(id string) *transaction {
	for _, t := range m.transactions {
		if t.translationReq.ID == id {
			return t
		}
	}

	return nil
}

func (m *middleware) removeExistingTranslation(trans *transaction) {
	for i, tr := range m.transactions {
		if tr == trans {
			m.transactions = append(m.transactions[:i], m.transactions[i+1:]...)
			return
		}
	}

	panic("translation not found")
}

func (m *middleware) isReqInBottomByID(id string) bool {
	for _, r := range m.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return true
		}
	}

	return false
}

func (m *middleware) findReqToBottomByID(id string) reqToBottom {
	for _, r := range m.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return r
		}
	}

	panic("req to bottom not found")
}

func (m *middleware) removeReqToBottomByID(id string) {
	for i, r := range m.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			m.inflightReqToBottom = append(
				m.inflightReqToBottom[:i],
				m.inflightReqToBottom[i+1:]...)

			return
		}
	}

	panic("req to bottom not found")
}

func (m *middleware) handleCtrlRequest() bool {
	req := m.ctrlPort.PeekIncoming()
	if req == nil {
		return false
	}

	if drain, ok := req.(*vm.UVMDrainRangeReq); ok { // sbin_codex
		return m.handleDrainRange(drain)
	}

	msg := req.(*mem.ControlMsg)

	if msg.DiscardTransations {
		return m.handleFlushReq(msg)
	} else if msg.Restart {
		return m.handleRestartReq(msg)
	}

	panic("never")
}

// handleDrainRange answers once nothing this translator issued for the region
// is still outstanding. Unrelated traffic keeps flowing while it waits.
// sbin_codex
func (m *middleware) handleDrainRange(req *vm.UVMDrainRangeReq) bool {
	if m.hasOutstandingInRange(req.PID, req.StartVA, req.Size) {
		return false
	}

	rsp := vm.NewUVMDrainRangeRsp(m.ctrlPort.AsRemote(), req.Src, req.ID)
	if err := m.ctrlPort.Send(rsp); err != nil {
		return false
	}

	m.ctrlPort.RetrieveIncoming()

	return true
}

// hasOutstandingInRange reports whether a request that already holds a mapping
// for the region is still travelling toward the caches.
//
// Only resolved requests count. One still waiting for a translation cannot
// reach a cache without a mapping, and the driver has already parked the
// region's page-table entries, so it will fault and wait in the GMMU replay
// queue until this eviction completes. Waiting for it here would deadlock the
// eviction against its own invalidation. // sbin_codex
func (m *middleware) hasOutstandingInRange(
	pid vm.PID,
	startVA, size uint64,
) bool {
	for _, trans := range m.transactions {
		if !trans.translationDone {
			continue
		}

		for _, req := range trans.incomingReqs {
			if inVARange(req, pid, startVA, size) {
				return true
			}
		}
	}

	for _, entry := range m.inflightReqToBottom {
		if inVARange(entry.reqFromTop, pid, startVA, size) {
			return true
		}
	}

	return false
}

func inVARange(
	req mem.AccessReq,
	pid vm.PID,
	startVA, size uint64,
) bool {
	if req.GetPID() != pid {
		return false
	}

	addr := req.GetAddress()

	return addr >= startVA && addr < startVA+size
}

func (m *middleware) handleFlushReq(
	req *mem.ControlMsg,
) bool {
	rsp := mem.ControlMsgBuilder{}.
		WithSrc(m.ctrlPort.AsRemote()).
		WithDst(req.Src).
		ToNotifyDone().
		Build()

	err := m.ctrlPort.Send(rsp)
	if err != nil {
		return false
	}

	m.ctrlPort.RetrieveIncoming()

	m.transactions = nil
	m.inflightReqToBottom = nil
	m.isFlushing = true

	return true
}

func (m *middleware) handleRestartReq(
	req *mem.ControlMsg,
) bool {
	rsp := mem.ControlMsgBuilder{}.
		WithSrc(m.ctrlPort.AsRemote()).
		WithDst(req.Src).
		ToNotifyDone().
		Build()

	err := m.ctrlPort.Send(rsp)
	if err != nil {
		return false
	}

	for m.topPort.RetrieveIncoming() != nil {
	}

	for m.bottomPort.RetrieveIncoming() != nil {
	}
	if m.remoteBottomPort != nil { // sbin_codex
		for m.remoteBottomPort.RetrieveIncoming() != nil {
		}
	}

	for m.translationPort.RetrieveIncoming() != nil {
	}

	m.isFlushing = false

	m.ctrlPort.RetrieveIncoming()

	return true
}
