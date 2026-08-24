package shaderarray

// sbin_codex: virtual-caching L1V/L1S UVM access gate (plan todo 10 of
// mgpusim-uvm-manager). Virtual-caching has no leaf data TLB, so each gate is
// the leaf data translation point: it probes the shared L2 TLB before cache
// admission, classifies the response by location, stamps GPU_LOCAL requests
// with the typed annotation (PID, VA page, HBM PA, location, generation), and
// implements the Todo 8 block/watermark/ack protocol. CPU_REMOTE requests are
// never annotated (their CPU PA is routed only to the remote endpoint,
// uvm-manager.md §13.2) and INVALID requests carry no PA.
//
// The gate is inert when its gate ID is zero (passthrough), so a topology that
// does not configure the gate preserves the legacy ROB->cache path.

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

// GenerationProvider supplies the current UVM manager generation. The manager
// increments the generation before publications; a gate stamps it into
// GPU_LOCAL annotations and retries probes that went stale across a
// publication. // sbin_codex
type GenerationProvider interface {
	Generation() uint64
}

// VirtualAccessGateWaiterCounter is the waiter-count reporting seam for
// virtual-caching L1V/L1S access gates (plan todo 7 of mgpusim-uvm-manager).
// Virtual-caching has no leaf data TLB, so each gate is the leaf data
// translation point: it records every original request that reaches it and
// reports raw = unique + coalesced (uvm-manager.md §8.4). // sbin_codex
type VirtualAccessGateWaiterCounter struct {
	unique    uint32
	coalesced uint32
}

// RecordOriginalRequest records one original request that reached the gate.
// unique marks the first request for its 64 KB fault-service region; duplicate
// waiters for an already-pending region are coalesced. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) RecordOriginalRequest(unique bool) {
	if unique {
		c.unique++
	} else {
		c.coalesced++
	}
}

// Raw returns the raw fault count: unique + coalesced. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) Raw() uint32 {
	return c.unique + c.coalesced
}

// Unique returns the number of unique original requests recorded. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) Unique() uint32 {
	return c.unique
}

// Coalesced returns the number of coalesced (duplicate) waiters recorded. // sbin_codex
func (c *VirtualAccessGateWaiterCounter) Coalesced() uint32 {
	return c.coalesced
}

// gateDisposition classifies how an admitted request was resolved relative to
// a block barrier. // sbin_codex
type gateDisposition int

const (
	gateDispositionNone gateDisposition = iota
	// gateDispositionDownstreamVisible marks a request whose access was sent
	// to the downstream data cache.
	gateDispositionDownstreamVisible
	// gateDispositionRetained marks a request retained in the gate for a
	// fault replay or a parked remote write.
	gateDispositionRetained
	// gateDispositionRemoteCommitted marks an old remote read committed to the
	// CPU endpoint.
	gateDispositionRemoteCommitted
)

// virtualBlockCommand is an active BlockRange on the gate. pendingDisposals
// counts the matching admitted requests with sequence<=watermark that are not
// yet disposed; the ack fires only when it reaches zero. // sbin_codex
type virtualBlockCommand struct {
	commandID        uint64
	pid              vm.PID
	startVA          uint64
	size             uint64
	watermark        uint64
	src              sim.RemotePort
	pendingDisposals int
	acked            bool
	parked           []*virtualParkedRequest
}

// virtualParkedRequest is a request that arrived after a block closed
// admission. It retains its ingress sequence (above the watermark) until the
// unblock releases it. // sbin_codex
type virtualParkedRequest struct {
	sequence uint64
	req      mem.AccessReq
}

// virtualProbe is an in-flight translation probe of the shared L2 TLB. The
// probe retains the original request, its ingress sequence, and the generation
// stamped at admission so a stale response can be retried. // sbin_codex
type virtualProbe struct {
	sequence    uint64
	req         mem.AccessReq
	generation  uint64
	transReq    *vm.TranslationReq
	disposition gateDisposition
	disposed    bool
}

// VirtualAccessGate is the virtual-caching L1V/L1S UVM access gate. It sits
// between the ROB and the data cache: Top receives requests from the ROB,
// Bottom forwards admitted accesses to the cache and receives cache responses,
// Translation probes the shared L2 TLB, and Control receives block/unblock
// commands from the CP. // sbin_codex
type VirtualAccessGate struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	topPort         sim.Port
	bottomPort      sim.Port
	translationPort sim.Port
	ctrlPort        sim.Port

	log2PageSize          uint64
	memoryPortMapper      mem.AddressToPortMapper
	translationPortMapper mem.AddressToPortMapper
	deviceID              uint64

	gateID             uint64
	generationProvider GenerationProvider
	waiter             VirtualAccessGateWaiterCounter

	isFlushing bool

	lastAssignedSequence uint64
	activeBlocks         []*virtualBlockCommand
	pendingAdmissions    []*virtualParkedRequest
	probes               []*virtualProbe
	inflightReqToBottom  []reqToBottom
	pendingRegions       map[uint64]int
}

// reqToBottom tracks a request forwarded to the cache so the matching
// response can be routed back to the original requester. // sbin_codex
type reqToBottom struct {
	reqFromTop  mem.AccessReq
	reqToBottom mem.AccessReq
}

// SetUVMGateID enables the UVM access gate with the given gate ID. A zero
// gate ID disables the gate (passthrough). // sbin_codex
func (g *VirtualAccessGate) SetUVMGateID(gateID uint64) {
	g.gateID = gateID
}

// GetUVMGateID returns the UVM access-gate ID. A zero value means the gate is
// disabled. // sbin_codex
func (g *VirtualAccessGate) GetUVMGateID() uint64 {
	return g.gateID
}

// SetGenerationProvider wires the generation source the gate stamps into
// annotations and compares for stale retries. // sbin_codex
func (g *VirtualAccessGate) SetGenerationProvider(provider GenerationProvider) {
	g.generationProvider = provider
}

// GetGenerationProvider returns the wired generation provider. // sbin_codex
func (g *VirtualAccessGate) GetGenerationProvider() GenerationProvider {
	return g.generationProvider
}

// WaiterCounts returns the recorded unique and coalesced original-request
// counts. // sbin_codex
func (g *VirtualAccessGate) WaiterCounts() (unique, coalesced uint32) {
	return g.waiter.Unique(), g.waiter.Coalesced()
}

// Tick drives the gate pipeline. // sbin_codex
func (g *VirtualAccessGate) Tick() bool {
	return g.MiddlewareHolder.Tick()
}

type gateMiddleware struct {
	*VirtualAccessGate
}

// Tick updates the gate state at each cycle. // sbin_codex
func (m *gateMiddleware) Tick() bool {
	madeProgress := false

	if !m.isFlushing {
		madeProgress = m.runPipeline()
	} else {
		for i := 0; i < 32; i++ {
			madeProgress = m.parseTranslation() || madeProgress
		}
	}

	madeProgress = m.handleCtrlRequest() || madeProgress

	return madeProgress
}

func (m *gateMiddleware) runPipeline() bool {
	madeProgress := false

	for i := 0; i < 32; i++ {
		madeProgress = m.respond() || madeProgress
	}

	for i := 0; i < 32; i++ {
		madeProgress = m.parseTranslation() || madeProgress
	}

	for i := 0; i < 32; i++ {
		madeProgress = m.translate() || madeProgress
	}

	return madeProgress
}

// translate admits one request per call: it retries released requests first,
// then admits a new request from the ROB. A disabled gate passes the request
// straight through to the cache. // sbin_codex
func (m *gateMiddleware) translate() bool {
	if m.gateID == 0 {
		item := m.topPort.PeekIncoming()
		if item == nil {
			return false
		}
		req := item.(mem.AccessReq)
		if !m.forwardToCache(req, nil) {
			return false
		}
		m.topPort.RetrieveIncoming()
		return true
	}

	if len(m.pendingAdmissions) > 0 {
		parked := m.pendingAdmissions[0]
		if m.startProbe(parked.req, parked.sequence) {
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
	m.lastAssignedSequence++
	sequence := m.lastAssignedSequence
	if block := m.matchingClosedBlock(req); block != nil {
		block.parked = append(block.parked, &virtualParkedRequest{
			sequence: sequence,
			req:      req,
		})
		m.topPort.RetrieveIncoming()
		return true
	}
	if !m.startProbe(req, sequence) {
		return false
	}
	m.topPort.RetrieveIncoming()
	return true
}

// startProbe sends the leaf translation probe for an admitted request and
// records the probe with its ingress sequence and the current generation. // sbin_codex
func (m *gateMiddleware) startProbe(req mem.AccessReq, sequence uint64) bool {
	vAddr := req.GetAddress()
	vPageID := m.addrToPageID(vAddr)

	transReq := vm.TranslationReqBuilder{}.
		WithSrc(m.translationPort.AsRemote()).
		WithDst(m.translationPortMapper.Find(vAddr)).
		WithPID(req.GetPID()).
		WithVAddr(vPageID).
		WithDeviceID(m.deviceID()).
		WithAccessKind(m.accessKindOf(req)).
		WithWaiterDelta(vm.WaiterDelta{InitialWaiters: 1}).
		Build()

	if err := m.translationPort.Send(transReq); err != nil {
		return false
	}

	generation := uint64(0)
	if m.generationProvider != nil {
		generation = m.generationProvider.Generation()
	}
	region := m.regionBaseOf(vAddr)
	if m.pendingRegions[region] == 0 {
		m.waiter.RecordOriginalRequest(true)
	} else {
		m.waiter.RecordOriginalRequest(false)
	}
	m.pendingRegions[region]++

	m.probes = append(m.probes, &virtualProbe{
		sequence:   sequence,
		req:        req,
		generation: generation,
		transReq:   transReq,
	})

	tracing.TraceReqReceive(req, m.VirtualAccessGate)
	tracing.TraceReqInitiate(
		transReq,
		m.VirtualAccessGate,
		tracing.MsgIDAtReceiver(req, m.VirtualAccessGate),
	)

	return true
}

// parseTranslation consumes one translation response from the L2 TLB. A
// stale-generation response is retried with a fresh probe; every other
// response is classified by location. // sbin_codex
func (m *gateMiddleware) parseTranslation() bool {
	rsp := m.translationPort.PeekIncoming()
	if rsp == nil {
		return false
	}

	transRsp := rsp.(*vm.TranslationRsp)
	probe := m.findProbeByReqID(transRsp.RespondTo)

	if probe == nil {
		m.translationPort.RetrieveIncoming()
		return true
	}

	if m.generationProvider != nil &&
		probe.generation != m.generationProvider.Generation() {
		// The mapping may have changed since admission: retry the probe with
		// the current generation.
		if !m.startProbe(probe.req, probe.sequence) {
			return false
		}
		m.removeProbe(probe)
		m.translationPort.RetrieveIncoming()
		return true
	}

	m.classifyAtGate(probe, transRsp)
	m.translationPort.RetrieveIncoming()

	return true
}

// classifyAtGate consumes a translation response according to the UVM access
// gate rules. // sbin_codex
func (m *gateMiddleware) classifyAtGate(
	probe *virtualProbe,
	rsp *vm.TranslationRsp,
) {
	req := probe.req

	if rsp.FaultPendingToken != 0 || rsp.Page.Location == vm.MemoryLocationINVALID {
		m.markDisposed(probe, gateDispositionRetained)
		return
	}

	switch rsp.Page.Location {
	case vm.MemoryLocationUNMANAGED:
		m.forwardToCache(req, nil)
		m.markDisposed(probe, gateDispositionDownstreamVisible)
		m.removeProbe(probe)
	case vm.MemoryLocationGPU_LOCAL:
		ann := &cache.VirtualAccessAnnotation{
			PID:        req.GetPID(),
			VAPage:     m.addrToPageID(req.GetAddress()),
			HBMPA:      rsp.Page.PAddr,
			Location:   vm.MemoryLocationGPU_LOCAL,
			Generation: probe.generation,
		}
		m.forwardToCache(req, ann)
		m.markDisposed(probe, gateDispositionDownstreamVisible)
		m.removeProbe(probe)
	case vm.MemoryLocationCPU_REMOTE:
		if _, isWrite := req.(*mem.WriteReq); isWrite {
			// Remote write: park (never commit to host; uvm-manager.md §15).
			m.markDisposed(probe, gateDispositionRetained)
			return
		}
		// Remote read: the CPU PA is carried only to the remote endpoint,
		// never inserted into the GPU data cache (§13.2).
		m.markDisposed(probe, gateDispositionRemoteCommitted)
		m.removeProbe(probe)
	default:
		log.Panicf("unknown MemoryLocation %d", uint8(rsp.Page.Location))
	}
}

// forwardToCache sends the request to the data cache with the VA retained and
// the optional annotation attached. // sbin_codex
func (m *gateMiddleware) forwardToCache(
	req mem.AccessReq,
	ann *cache.VirtualAccessAnnotation,
) bool {
	forwarded := m.createForwardedReq(req)
	cache.Annotate(forwarded, ann)
	if err := m.bottomPort.Send(forwarded); err != nil {
		return false
	}
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(forwarded, m.VirtualAccessGate),
		tracing.MilestoneKindNetworkBusy,
		m.bottomPort.Name(),
		m.VirtualAccessGate.Name(),
		m.VirtualAccessGate,
	)
	m.inflightReqToBottom = append(m.inflightReqToBottom, reqToBottom{
		reqFromTop:  req,
		reqToBottom: forwarded,
	})
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.VirtualAccessGate),
		tracing.MilestoneKindTranslation,
		"translation",
		m.VirtualAccessGate.Name(),
		m.VirtualAccessGate,
	)
	tracing.TraceReqInitiate(forwarded, m.VirtualAccessGate,
		tracing.MsgIDAtReceiver(req, m.VirtualAccessGate))
	return true
}

// createForwardedReq clones the request for the cache, keeping the virtual
// address and the PID so the virtually tagged cache indexes correctly. // sbin_codex
func (m *gateMiddleware) createForwardedReq(req mem.AccessReq) mem.AccessReq {
	switch req := req.(type) {
	case *mem.ReadReq:
		clone := mem.ReadReqBuilder{}.
			WithSrc(m.bottomPort.AsRemote()).
			WithDst(m.memoryPortMapper.Find(req.Address)).
			WithAddress(req.Address).
			WithByteSize(req.AccessByteSize).
			WithPID(req.PID).
			WithInfo(req.Info).
			Build()
		clone.CanWaitForCoalesce = req.CanWaitForCoalesce
		return clone
	case *mem.WriteReq:
		clone := mem.WriteReqBuilder{}.
			WithSrc(m.bottomPort.AsRemote()).
			WithDst(m.memoryPortMapper.Find(req.Address)).
			WithData(req.Data).
			WithDirtyMask(req.DirtyMask).
			WithAddress(req.Address).
			WithPID(req.PID).
			WithInfo(req.Info).
			Build()
		clone.CanWaitForCoalesce = req.CanWaitForCoalesce
		return clone
	default:
		log.Panicf("cannot forward request of type %s", reflect.TypeOf(req))
		return nil
	}
}

// respond routes a cache response back to the original requester. // sbin_codex
func (m *gateMiddleware) respond() bool {
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
				tracing.MsgIDAtReceiver(reqFromTop, m.VirtualAccessGate),
				tracing.MilestoneKindData,
				"data",
				m.VirtualAccessGate.Name(),
				m.VirtualAccessGate,
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
				tracing.MsgIDAtReceiver(reqFromTop, m.VirtualAccessGate),
				tracing.MilestoneKindSubTask,
				"subtask",
				m.VirtualAccessGate.Name(),
				m.VirtualAccessGate,
			)
		}
	default:
		log.Panicf("cannot handle respond of type %s", reflect.TypeOf(rsp))
	}

	if reqInBottom {
		if err := m.topPort.Send(rspToTop); err != nil {
			return false
		}
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(reqFromTop, m.VirtualAccessGate),
			tracing.MilestoneKindNetworkBusy,
			m.topPort.Name(),
			m.VirtualAccessGate.Name(),
			m.VirtualAccessGate,
		)
		m.removeReqToBottomByID(rsp.(mem.AccessRsp).GetRspTo())
		tracing.TraceReqFinalize(reqToBottomCombo.reqToBottom, m.VirtualAccessGate)
		tracing.TraceReqComplete(reqToBottomCombo.reqFromTop, m.VirtualAccessGate)
	}

	m.bottomPort.RetrieveIncoming()

	return true
}

// handleCtrlRequest dispatches control messages on the gate control port. // sbin_codex
func (m *gateMiddleware) handleCtrlRequest() bool {
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
	case *vm.BlockRange:
		return m.handleBlockRange(msg)
	case *vm.UnblockRange:
		return m.handleUnblockRange(msg)
	default:
		panic("never")
	}
}

// handleFlushReq discards the gate state and acknowledges the flush. // sbin_codex
func (m *gateMiddleware) handleFlushReq(req *mem.ControlMsg) bool {
	rsp := mem.ControlMsgBuilder{}.
		WithSrc(m.ctrlPort.AsRemote()).
		WithDst(req.Src).
		ToNotifyDone().
		Build()

	if err := m.ctrlPort.Send(rsp); err != nil {
		return false
	}

	m.ctrlPort.RetrieveIncoming()

	m.probes = nil
	m.inflightReqToBottom = nil
	m.pendingRegions = make(map[uint64]int)
	m.isFlushing = true

	return true
}

// handleRestartReq clears the gate buffers and resumes admission. // sbin_codex
func (m *gateMiddleware) handleRestartReq(req *mem.ControlMsg) bool {
	rsp := mem.ControlMsgBuilder{}.
		WithSrc(m.ctrlPort.AsRemote()).
		WithDst(req.Src).
		ToNotifyDone().
		Build()

	if err := m.ctrlPort.Send(rsp); err != nil {
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

// handleBlockRange atomically closes matching admission on the gate,
// snapshots the local watermark, and acknowledges the command once every
// matching request with sequence<=watermark is disposed. A duplicate command
// is rejected. // sbin_codex
func (m *gateMiddleware) handleBlockRange(msg *vm.BlockRange) bool {
	for _, block := range m.activeBlocks {
		if block.commandID == msg.CommandID {
			m.ctrlPort.RetrieveIncoming()
			return true
		}
	}

	block := &virtualBlockCommand{
		commandID: msg.CommandID,
		pid:       msg.PID,
		startVA:   msg.StartVA,
		size:      msg.Size,
		watermark: m.lastAssignedSequence,
		src:       msg.Src,
	}
	for _, probe := range m.probes {
		if probe.disposed {
			continue
		}
		if probe.sequence <= block.watermark && block.matches(probe.req) {
			block.pendingDisposals++
		}
	}
	m.activeBlocks = append(m.activeBlocks, block)

	m.ctrlPort.RetrieveIncoming()
	m.trySendBlockAcks()

	return true
}

// handleUnblockRange releases the parked requests of the matching block and
// acknowledges the command. An unknown command is rejected. // sbin_codex
func (m *gateMiddleware) handleUnblockRange(msg *vm.UnblockRange) bool {
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

// releaseParked re-admits the parked requests of a released block. A request
// that matches another still-closed block stays parked there; a request that
// cannot yet be re-admitted waits in pendingAdmissions. // sbin_codex
func (m *gateMiddleware) releaseParked(block *virtualBlockCommand) {
	remaining := block.parked[:0]
	for _, parked := range block.parked {
		if next := m.matchingClosedBlock(parked.req); next != nil {
			next.parked = append(next.parked, parked)
			continue
		}
		if !m.startProbe(parked.req, parked.sequence) {
			remaining = append(remaining, parked)
		}
	}
	if len(remaining) > 0 {
		m.pendingAdmissions = append(m.pendingAdmissions, remaining...)
	}
	block.parked = nil
}

func (m *gateMiddleware) sendUnblockAck(msg *vm.UnblockRange) bool {
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

// matchingClosedBlock returns the first active block whose range matches the
// request. A request admitted while such a block is closed parks instead of
// starting a probe. // sbin_codex
func (m *gateMiddleware) matchingClosedBlock(req mem.AccessReq) *virtualBlockCommand {
	for _, block := range m.activeBlocks {
		if block.matches(req) {
			return block
		}
	}
	return nil
}

// markDisposed records how an admitted request was resolved and decrements
// every active block that was waiting on it. A block whose pending-disposal
// count reaches zero becomes ackable. // sbin_codex
func (m *gateMiddleware) markDisposed(
	probe *virtualProbe,
	d gateDisposition,
) {
	if probe.disposed {
		return
	}
	probe.disposition = d
	probe.disposed = true
	for _, block := range m.activeBlocks {
		if block.acked {
			continue
		}
		if probe.sequence <= block.watermark && block.matches(probe.req) {
			block.pendingDisposals--
		}
	}
	m.trySendBlockAcks()
}

// trySendBlockAcks sends the exactly-one BlockAck per block once every
// matching request with sequence<=watermark is disposed. // sbin_codex
func (m *gateMiddleware) trySendBlockAcks() bool {
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

// matches reports whether the block range covers the request. // sbin_codex
func (b *virtualBlockCommand) matches(req mem.AccessReq) bool {
	return req.GetPID() == b.pid &&
		req.GetAddress() >= b.startVA &&
		req.GetAddress() < b.startVA+b.size
}

func (m *gateMiddleware) addrToPageID(addr uint64) uint64 {
	return (addr >> m.log2PageSize) << m.log2PageSize
}

func (m *gateMiddleware) regionBaseOf(addr uint64) uint64 {
	return (addr >> 16) << 16
}

func (m *gateMiddleware) accessKindOf(req mem.AccessReq) vm.AccessKind {
	if _, isWrite := req.(*mem.WriteReq); isWrite {
		return vm.AccessKindWrite
	}
	return vm.AccessKindRead
}

func (m *gateMiddleware) deviceID() uint64 {
	return m.VirtualAccessGate.deviceID
}

func (m *gateMiddleware) findProbeByReqID(id string) *virtualProbe {
	for _, probe := range m.probes {
		if probe.transReq.ID == id {
			return probe
		}
	}
	return nil
}

func (m *gateMiddleware) removeProbe(probe *virtualProbe) {
	for i, p := range m.probes {
		if p == probe {
			m.probes = append(m.probes[:i], m.probes[i+1:]...)
			break
		}
	}
	region := m.regionBaseOf(probe.req.GetAddress())
	if m.pendingRegions[region] > 0 {
		m.pendingRegions[region]--
	}
}

func (m *gateMiddleware) isReqInBottomByID(id string) bool {
	for _, r := range m.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return true
		}
	}
	return false
}

func (m *gateMiddleware) findReqToBottomByID(id string) reqToBottom {
	for _, r := range m.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return r
		}
	}
	panic("req to bottom not found")
}

func (m *gateMiddleware) removeReqToBottomByID(id string) {
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
