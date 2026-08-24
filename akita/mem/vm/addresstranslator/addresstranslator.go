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
	// sbin_codex: UVM access-gate state (todo 9 of mgpusim-uvm-manager).
	// sequence is the monotonic ingress sequence the gate assigned at
	// admission; disposition records how the request was resolved relative to
	// a block barrier; disposed marks a terminal disposition so a barrier
	// never counts the request twice.
	sequence    uint64
	disposition disposition
	disposed    bool
}

type reqToBottom struct {
	reqFromTop  mem.AccessReq
	reqToBottom mem.AccessReq
}

// sbin_codex: disposition classifies how an admitted request was resolved
// relative to a block barrier (todo 9 of mgpusim-uvm-manager).
type disposition int

const (
	dispositionNone disposition = iota
	// dispositionDownstreamVisible marks a request whose translated access was
	// sent to the downstream data cache.
	dispositionDownstreamVisible
	// dispositionRetained marks a request retained in the gate for a fault
	// replay or a parked remote write.
	dispositionRetained
	// dispositionRemoteCommitted marks an old remote read committed to the CPU
	// endpoint.
	dispositionRemoteCommitted
)

// sbin_codex: a blockCommand is an active BlockRange on the access gate.
// pendingDisposals counts the matching admitted requests with
// sequence<=watermark that are not yet disposed; the ack fires only when it
// reaches zero.
type blockCommand struct {
	commandID        uint64
	pid              vm.PID
	startVA          uint64
	size             uint64
	watermark        uint64
	src              sim.RemotePort
	pendingDisposals int
	acked            bool
	parked           []*parkedRequest
}

// sbin_codex: a parkedRequest is a request that arrived after a block closed
// admission. It retains its ingress sequence (above the watermark) until the
// unblock releases it.
type parkedRequest struct {
	sequence uint64
	req      mem.AccessReq
}

// Comp is an AddressTranslator that forwards the read/write requests with
// the address translated from virtual to physical.
type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	topPort         sim.Port
	bottomPort      sim.Port
	translationPort sim.Port
	ctrlPort        sim.Port

	log2PageSize               uint64
	deviceID                   uint64
	numReqPerCycle             int
	memoryPortMapper           mem.AddressToPortMapper
	translationPortMapper      mem.AddressToPortMapper
	physicalAddressPassthrough bool // sbin_codex: PID zero is already physical only at an explicit boundary.

	isFlushing bool

	transactions        []*transaction
	inflightReqToBottom []reqToBottom

	// sbin_codex: UVM access-gate state (todo 9 of mgpusim-uvm-manager). The
	// gate assigns a monotonic ingress sequence to every admitted request and
	// snapshots the local watermark when a block closes admission.
	gateID               uint64
	lastAssignedSequence uint64
	activeBlocks         []*blockCommand
	pendingAdmissions    []*parkedRequest
}

// SetUVMGateID enables the UVM access gate on the address translator with the
// given gate ID. A zero gate ID disables the gate. // sbin_codex
func (c *Comp) SetUVMGateID(gateID uint64) {
	c.gateID = gateID
}

// GetUVMGateID returns the UVM access-gate ID of the address translator. A
// zero value means the gate is disabled. // sbin_codex
func (c *Comp) GetUVMGateID() uint64 {
	return c.gateID
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
	// sbin_codex: retry released requests before new admissions (todo 9).
	if len(m.pendingAdmissions) > 0 {
		parked := m.pendingAdmissions[0]
		if m.startTranslation(parked.req, parked.sequence) {
			m.pendingAdmissions = m.pendingAdmissions[1:]
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
		pageBase := req.GetAddress() &^ ((uint64(1) << m.log2PageSize) - 1)
		forwarded := m.createTranslatedReq(req, vm.Page{
			PAddr:    pageBase,
			VAddr:    pageBase,
			PageSize: 1 << m.log2PageSize,
		}) // sbin_codex: preserve physical L1I page base and apply offset once.
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

	// sbin_codex: UVM access-gate admission (todo 9 of mgpusim-uvm-manager).
	// Each admitted request receives a monotonic ingress sequence; a request
	// arriving while a matching block is closed gets a sequence above the
	// watermark and parks.
	if m.gateID != 0 {
		m.lastAssignedSequence++
		sequence := m.lastAssignedSequence
		if block := m.matchingClosedBlock(req); block != nil {
			block.parked = append(block.parked, &parkedRequest{
				sequence: sequence,
				req:      req,
			})
			m.topPort.RetrieveIncoming()
			return true
		}
		if !m.startTranslation(req, sequence) {
			return false
		}
		m.topPort.RetrieveIncoming()
		return true
	}

	vAddr := req.GetAddress()
	vPageID := m.addrToPageID(vAddr)

	transReq := vm.TranslationReqBuilder{}.
		WithSrc(m.translationPort.AsRemote()).
		WithDst(m.translationPortMapper.Find(vAddr)).
		WithPID(req.GetPID()).
		WithVAddr(vPageID).
		WithDeviceID(m.deviceID).
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

	// sbin_codex: UVM access-gate classification (todo 9 of
	// mgpusim-uvm-manager). A gate-enabled translator classifies every
	// translation response by location and access kind; a disabled translator
	// keeps the legacy forward-to-cache path.
	if m.gateID != 0 {
		if m.classifyAtGate(transaction, transRsp) {
			m.translationPort.RetrieveIncoming()
			return true
		}
		return false
	}

	transaction.translationRsp = transRsp
	transaction.translationDone = true

	reqFromTop := transaction.incomingReqs[0]
	translatedReq := m.createTranslatedReq(
		reqFromTop,
		transaction.translationRsp.Page)

	err := m.bottomPort.Send(translatedReq)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(translatedReq, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.bottomPort.Name(),
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
	rsp := m.bottomPort.PeekIncoming()
	if rsp == nil {
		return false
	}

	var (
		reqFromTop       mem.AccessReq
		reqToBottomCombo reqToBottom
		rspToTop         mem.AccessRsp
	)

	reqInBottom := false

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

	m.bottomPort.RetrieveIncoming()

	return true
}

func (m *middleware) createTranslatedReq(
	req mem.AccessReq,
	page vm.Page,
) mem.AccessReq {
	switch req := req.(type) {
	case *mem.ReadReq:
		return m.createTranslatedReadReq(req, page)
	case *mem.WriteReq:
		return m.createTranslatedWriteReq(req, page)
	default:
		log.Panicf("cannot translate request of type %s", reflect.TypeOf(req))
		return nil
	}
}

func (m *middleware) createTranslatedReadReq(
	req *mem.ReadReq,
	page vm.Page,
) *mem.ReadReq {
	offset := req.Address % (1 << m.log2PageSize)
	addr := page.PAddr + offset
	clone := mem.ReadReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.memoryPortMapper.Find(addr)).
		WithAddress(addr).
		WithByteSize(req.AccessByteSize).
		WithPID(0).
		WithInfo(req.Info).
		Build()
	clone.CanWaitForCoalesce = req.CanWaitForCoalesce

	return clone
}

func (m *middleware) createTranslatedWriteReq(
	req *mem.WriteReq,
	page vm.Page,
) *mem.WriteReq {
	offset := req.Address % (1 << m.log2PageSize)
	addr := page.PAddr + offset
	clone := mem.WriteReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.memoryPortMapper.Find(addr)).
		WithData(req.Data).
		WithDirtyMask(req.DirtyMask).
		WithAddress(addr).
		WithPID(0).
		WithInfo(req.Info).
		Build()
	clone.CanWaitForCoalesce = req.CanWaitForCoalesce

	return clone
}

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

	switch msg := req.(type) {
	case *mem.ControlMsg:
		if msg.DiscardTransations {
			return m.handleFlushReq(msg)
		} else if msg.Restart {
			return m.handleRestartReq(msg)
		}
		panic("never")
	case *vm.BlockRange: // sbin_codex: UVM mapping-transition block (todo 9).
		return m.handleBlockRange(msg)
	case *vm.UnblockRange: // sbin_codex: UVM mapping-transition unblock (todo 9).
		return m.handleUnblockRange(msg)
	default:
		panic("never")
	}
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

	for m.translationPort.RetrieveIncoming() != nil {
	}

	m.isFlushing = false

	m.ctrlPort.RetrieveIncoming()

	return true
}

// sbin_codex: startTranslation sends the translation request for an admitted
// request and records the transaction with its ingress sequence (todo 9 of
// mgpusim-uvm-manager).
func (m *middleware) startTranslation(
	req mem.AccessReq,
	sequence uint64,
) bool {
	vAddr := req.GetAddress()
	vPageID := m.addrToPageID(vAddr)

	transReq := vm.TranslationReqBuilder{}.
		WithSrc(m.translationPort.AsRemote()).
		WithDst(m.translationPortMapper.Find(vAddr)).
		WithPID(req.GetPID()).
		WithVAddr(vPageID).
		WithDeviceID(m.deviceID).
		WithAccessKind(m.accessKindOf(req)).
		Build()

	err := m.translationPort.Send(transReq)
	if err != nil {
		return false
	}

	translation := &transaction{
		incomingReqs:   []mem.AccessReq{req},
		translationReq: transReq,
		sequence:       sequence,
	}
	m.transactions = append(m.transactions, translation)

	tracing.TraceReqReceive(req, m.Comp)
	tracing.TraceReqInitiate(
		transReq,
		m.Comp,
		tracing.MsgIDAtReceiver(req, m.Comp),
	)

	return true
}

func (m *middleware) accessKindOf(req mem.AccessReq) vm.AccessKind {
	if _, isWrite := req.(*mem.WriteReq); isWrite {
		return vm.AccessKindWrite
	}
	return vm.AccessKindRead
}

// sbin_codex: classifyAtGate consumes a translation response according to the
// UVM access-gate rules (todo 9 of mgpusim-uvm-manager). It returns true when
// the response was fully consumed; false when it stalled on bottom-port
// backpressure and must be retried.
func (m *middleware) classifyAtGate(
	trans *transaction,
	rsp *vm.TranslationRsp,
) bool {
	trans.translationRsp = rsp
	trans.translationDone = true

	reqFromTop := trans.incomingReqs[0]

	// A fault-pending / INVALID translation retains the request for replay.
	if rsp.FaultPendingToken != 0 || rsp.Page.Location == vm.MemoryLocationINVALID {
		m.markDisposed(trans, reqFromTop, dispositionRetained)
		return true
	}

	switch rsp.Page.Location {
	case vm.MemoryLocationUNMANAGED, vm.MemoryLocationGPU_LOCAL:
		// Local or unmanaged: forward to the data cache with the HBM PA.
		translatedReq := m.createTranslatedReq(reqFromTop, rsp.Page)
		if err := m.bottomPort.Send(translatedReq); err != nil {
			return false
		}
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(translatedReq, m.Comp),
			tracing.MilestoneKindNetworkBusy,
			m.bottomPort.Name(),
			m.Comp.Name(),
			m.Comp,
		)
		m.inflightReqToBottom = append(m.inflightReqToBottom,
			reqToBottom{
				reqFromTop:  reqFromTop,
				reqToBottom: translatedReq,
			})
		trans.incomingReqs = trans.incomingReqs[1:]
		if len(trans.incomingReqs) == 0 {
			m.removeExistingTranslation(trans)
		}
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(reqFromTop, m.Comp),
			tracing.MilestoneKindTranslation,
			"translation",
			m.Comp.Name(),
			m.Comp,
		)
		tracing.TraceReqInitiate(translatedReq, m.Comp,
			tracing.MsgIDAtReceiver(reqFromTop, m.Comp))
		m.markDisposed(trans, reqFromTop, dispositionDownstreamVisible)
		return true
	case vm.MemoryLocationCPU_REMOTE:
		if _, isWrite := reqFromTop.(*mem.WriteReq); isWrite {
			// Remote write: park (never commit to host; uvm-manager.md §15).
			m.markDisposed(trans, reqFromTop, dispositionRetained)
			return true
		}
		// Remote read: the CPU PA is carried only to the remote endpoint,
		// never inserted into the GPU data cache (§13.2).
		trans.incomingReqs = trans.incomingReqs[1:]
		if len(trans.incomingReqs) == 0 {
			m.removeExistingTranslation(trans)
		}
		m.markDisposed(trans, reqFromTop, dispositionRemoteCommitted)
		return true
	default:
		log.Panicf("unknown MemoryLocation %d", uint8(rsp.Page.Location))
		return false
	}
}

// sbin_codex: handleBlockRange atomically closes matching admission on the
// access gate, snapshots the local watermark, and acknowledges the command
// once every matching request with sequence<=watermark is disposed (todo 9 of
// mgpusim-uvm-manager). A duplicate command is rejected.
func (m *middleware) handleBlockRange(msg *vm.BlockRange) bool {
	for _, block := range m.activeBlocks {
		if block.commandID == msg.CommandID {
			m.ctrlPort.RetrieveIncoming()
			return true
		}
	}

	block := &blockCommand{
		commandID: msg.CommandID,
		pid:       msg.PID,
		startVA:   msg.StartVA,
		size:      msg.Size,
		watermark: m.lastAssignedSequence,
		src:       msg.Src,
	}
	for _, trans := range m.transactions {
		if trans.disposed || len(trans.incomingReqs) == 0 {
			continue
		}
		if trans.sequence <= block.watermark && block.matches(trans.incomingReqs[0]) {
			block.pendingDisposals++
		}
	}
	m.activeBlocks = append(m.activeBlocks, block)

	m.ctrlPort.RetrieveIncoming()
	m.trySendBlockAcks()

	return true
}

// sbin_codex: handleUnblockRange releases the parked requests of the matching
// block and acknowledges the command. An unknown command is rejected.
func (m *middleware) handleUnblockRange(msg *vm.UnblockRange) bool {
	for i, block := range m.activeBlocks {
		if block.commandID != msg.CommandID {
			continue
		}

		m.activeBlocks = append(m.activeBlocks[:i], m.activeBlocks[i+1:]...)
		m.releaseParked(block)

		m.ctrlPort.RetrieveIncoming()
		m.sendUnblockAck(msg)

		return true
	}

	m.ctrlPort.RetrieveIncoming()
	return true
}

// sbin_codex: releaseParked re-admits the parked requests of a released block.
// A request that matches another still-closed block stays parked there; a
// request that cannot yet be re-admitted waits in pendingAdmissions.
func (m *middleware) releaseParked(block *blockCommand) {
	remaining := block.parked[:0]
	for _, parked := range block.parked {
		if next := m.matchingClosedBlock(parked.req); next != nil {
			next.parked = append(next.parked, parked)
			continue
		}
		if !m.startTranslation(parked.req, parked.sequence) {
			remaining = append(remaining, parked)
		}
	}
	if len(remaining) > 0 {
		m.pendingAdmissions = append(m.pendingAdmissions, remaining...)
	}
	block.parked = nil
}

func (m *middleware) sendUnblockAck(msg *vm.UnblockRange) bool {
	if !m.ctrlPort.CanSend() {
		return false
	}
	ack := &vm.UnblockAck{CommandID: msg.CommandID}
	ack.ID = sim.GetIDGenerator().Generate()
	ack.Src = m.ctrlPort.AsRemote()
	ack.Dst = msg.Src
	if err := m.ctrlPort.Send(ack); err != nil {
		return false
	}
	return true
}

// sbin_codex: matchingClosedBlock returns the first active block whose range
// matches the request. A request admitted while such a block is closed parks
// instead of starting a translation.
func (m *middleware) matchingClosedBlock(req mem.AccessReq) *blockCommand {
	for _, block := range m.activeBlocks {
		if block.matches(req) {
			return block
		}
	}
	return nil
}

// sbin_codex: markDisposed records how an admitted request was resolved and
// decrements every active block that was waiting on it. A block whose
// pending-disposal count reaches zero becomes ackable.
func (m *middleware) markDisposed(
	trans *transaction,
	req mem.AccessReq,
	d disposition,
) {
	if trans.disposed {
		return
	}
	trans.disposition = d
	trans.disposed = true
	for _, block := range m.activeBlocks {
		if block.acked {
			continue
		}
		if trans.sequence <= block.watermark && block.matches(req) {
			block.pendingDisposals--
		}
	}
	m.trySendBlockAcks()
}

// sbin_codex: trySendBlockAcks sends the exactly-one BlockAck per block once
// every matching request with sequence<=watermark is disposed.
func (m *middleware) trySendBlockAcks() bool {
	madeProgress := false
	for _, block := range m.activeBlocks {
		if block.acked || block.pendingDisposals > 0 {
			continue
		}
		if !m.ctrlPort.CanSend() {
			continue
		}
		ack := &vm.BlockAck{
			CommandID: block.commandID,
			GateID:    m.gateID,
			Watermark: block.watermark,
		}
		ack.ID = sim.GetIDGenerator().Generate()
		ack.Src = m.ctrlPort.AsRemote()
		ack.Dst = block.src
		if err := m.ctrlPort.Send(ack); err != nil {
			continue
		}
		block.acked = true
		madeProgress = true
	}
	return madeProgress
}

// sbin_codex: matches reports whether the block range covers the request.
func (b *blockCommand) matches(req mem.AccessReq) bool {
	return req.GetPID() == b.pid &&
		req.GetAddress() >= b.startVA &&
		req.GetAddress() < b.startVA+b.size
}
