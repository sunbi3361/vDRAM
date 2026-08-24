package cp

// sbin_codex: CP UVM routing middleware (todo 12 of mgpusim-uvm-manager).
//
// The uvmMiddleware dispatches range-scoped UVM operations between the UVM
// driver, the GMMU, the data caches, the TLBs, the GPU-wide AccessCounter,
// and the DMA engine (uvm-manager.md §21.1, §21.6, §23.1):
//
//   - fault notifications: GMMU -> CP -> driver (typed PageFaultReq),
//   - replay: driver -> CP -> GMMU (ReplayRange) and GMMU -> CP -> driver
//     (ReplayAck -> UVMFaultReplayRsp),
//   - block/unblock: pre-register the topology gateID set by commandID,
//     forward to the GMMU, and aggregate the Todo-8 {commandID, gateID,
//     watermark} acks. Duplicate, unknown, wrong-command, or changed-watermark
//     acks are rejected and never satisfy completion; the command completes
//     only after the exact gateID set is exhausted,
//   - range TLB invalidation: fan out to the topology-present TLB endpoints
//     (baseline: private L1V/L1S/L1I + shared L2; virtual-caching: private L1I
//     + shared L2 only, with no fabricated L1V/L1S TLB endpoints),
//   - range cache control: fan out to the data caches only (L1V/L1S/L2, L1I
//     excluded from WB+INV),
//   - counter reset: driver -> CP -> GPU-wide AccessCounter -> CP ack -> kernel
//     dispatch (the counter itself is todo 11; this middleware routes the
//     command and holds kernel dispatch until the ack),
//   - migration DMA: driver -> CP -> DMA engine through the existing
//     MemCopyH2DReq / MemCopyD2HReq flow.
//
// The middleware never uses the global controls (flushAll / restartAll) or the
// Page Migration Controller.

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// CounterResetReq asks the CP to route an acknowledged reset of the GPU-wide
// Access Counter (uvm-manager.md §14.2). The counter itself is todo 11; this
// envelope is the routing contract between the UVM driver, the CP, and the
// counter. // sbin_codex
type CounterResetReq struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (m *CounterResetReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the CounterResetReq with a different ID.
func (m *CounterResetReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()
	return &cloneMsg
}

// CounterResetRsp is the acknowledged completion of a CounterResetReq. // sbin_codex
type CounterResetRsp struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (m *CounterResetRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the CounterResetRsp with a different ID.
func (m *CounterResetRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()
	return &cloneMsg
}

// uvmBlockState tracks a block/unblock command's gateID aggregation. The CP
// pre-registers the exact topology gateID set for the commandID, accepts one
// matching ack per gate, and completes only after the set is exhausted. // sbin_codex
type uvmBlockState struct {
	commandID  uint64
	isBlock    bool
	expected   map[uint64]bool
	watermarks map[uint64]uint64
	original   sim.Msg
	src        sim.RemotePort
}

// uvmTLBInvalidateState tracks a range TLB invalidation's endpoint
// aggregation. // sbin_codex
type uvmTLBInvalidateState struct {
	req     *protocol.UVMTLBInvalidateReq
	pending int
}

// uvmCacheFlushState tracks a range cache flush's endpoint aggregation. // sbin_codex
type uvmCacheFlushState struct {
	req     *protocol.UVMCacheRangeFlushReq
	pending int
}

type uvmMiddleware struct {
	*CommandProcessor
}

func (m *uvmMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.Handle() || madeProgress
	madeProgress = m.HandleInternal() || madeProgress
	return madeProgress
}

// Handle processes UVM commands from the driver on the ToDriver port.
func (m *uvmMiddleware) Handle() bool {
	msg := m.ToDriver.PeekIncoming()

	switch req := msg.(type) {
	case *vm.BlockRange:
		return m.processBlockRange(req)
	case *vm.UnblockRange:
		return m.processUnblockRange(req)
	case *protocol.UVMFaultReplayReq:
		return m.processFaultReplayReq(req)
	case *protocol.UVMTLBInvalidateReq:
		return m.processTLBInvalidateReq(req)
	case *protocol.UVMCacheRangeFlushReq:
		return m.processCacheRangeFlushReq(req)
	case *protocol.MigrationReq:
		return m.processMigrationReq(req)
	case *CounterResetReq:
		return m.processCounterResetReq(req)
	}

	return false
}

// HandleInternal processes UVM responses from the GMMU and the AccessCounter.
// The data-cache and TLB range responses are owned by the ctrlMiddleware,
// which delegates them to this middleware.
func (m *uvmMiddleware) HandleInternal() bool {
	madeProgress := false
	madeProgress = m.processRspFromGMMU() || madeProgress
	madeProgress = m.processRspFromAccessCounter() || madeProgress
	return madeProgress
}

func (m *uvmMiddleware) processRspFromGMMU() bool {
	msg := m.ToGMMU.PeekIncoming()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *vm.FaultNotification:
		return m.processFaultNotification(req)
	case *vm.BlockAck:
		return m.processBlockAck(req)
	case *vm.UnblockAck:
		return m.processUnblockAck(req)
	case *vm.ReplayAck:
		return m.processReplayAck(req)
	}

	// A CP->GMMU command (e.g. a ReplayRange that looped back through the
	// shared ToGMMU seam) is consumed by the GMMU, not by this middleware.
	return false
}

func (m *uvmMiddleware) processRspFromAccessCounter() bool {
	msg := m.ToAccessCounter.PeekIncoming()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *CounterResetRsp:
		return m.processCounterResetRsp(req)
	}

	return false
}

// processBlockRange pre-registers the topology gateID set for the commandID,
// forwards the block to the GMMU, and aggregates the per-gate acks. A
// duplicate command is rejected. // sbin_codex
func (m *uvmMiddleware) processBlockRange(req *vm.BlockRange) bool {
	if _, exists := m.activeUVMBlocks[req.CommandID]; exists {
		m.ToDriver.RetrieveIncoming()
		return true
	}

	state := m.newUVMBlockState(req.CommandID, true, req, req.Src)
	m.activeUVMBlocks[req.CommandID] = state

	cloned := req.Clone().(*vm.BlockRange)
	cloned.Src = m.ToGMMU.AsRemote()
	cloned.Dst = m.GMMUControl
	if err := m.ToGMMU.Send(cloned); err != nil {
		delete(m.activeUVMBlocks, req.CommandID)
		return false
	}

	m.ToDriver.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.CommandProcessor)

	return true
}

// processUnblockRange mirrors processBlockRange for the unblock command. // sbin_codex
func (m *uvmMiddleware) processUnblockRange(req *vm.UnblockRange) bool {
	if _, exists := m.activeUVMBlocks[req.CommandID]; exists {
		m.ToDriver.RetrieveIncoming()
		return true
	}

	state := m.newUVMBlockState(req.CommandID, false, req, req.Src)
	m.activeUVMBlocks[req.CommandID] = state

	cloned := req.Clone().(*vm.UnblockRange)
	cloned.Src = m.ToGMMU.AsRemote()
	cloned.Dst = m.GMMUControl
	if err := m.ToGMMU.Send(cloned); err != nil {
		delete(m.activeUVMBlocks, req.CommandID)
		return false
	}

	m.ToDriver.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.CommandProcessor)

	return true
}

// newUVMBlockState builds the pre-registered gateID set for a block/unblock
// command from the topology-registered UVMGateIDs. // sbin_codex
func (m *uvmMiddleware) newUVMBlockState(
	commandID uint64,
	isBlock bool,
	original sim.Msg,
	src sim.RemotePort,
) *uvmBlockState {
	state := &uvmBlockState{
		commandID:  commandID,
		isBlock:    isBlock,
		expected:   make(map[uint64]bool),
		watermarks: make(map[uint64]uint64),
		original:   original,
		src:        src,
	}
	for _, gateID := range m.UVMGateIDs {
		state.expected[gateID] = true
	}
	return state
}

// processBlockAck validates and aggregates one {commandID, gateID, watermark}
// ack. Duplicate, unknown-command, unknown-gate, wrong-command, and
// changed-watermark acks are rejected and never satisfy completion. // sbin_codex
func (m *uvmMiddleware) processBlockAck(ack *vm.BlockAck) bool {
	state, ok := m.activeUVMBlocks[ack.CommandID]
	if !ok {
		// Unknown command: reject.
		m.ToGMMU.RetrieveIncoming()
		return true
	}
	if !state.isBlock {
		// Wrong command: a BlockAck for an unblock command.
		m.ToGMMU.RetrieveIncoming()
		return true
	}
	if !state.expected[ack.GateID] {
		// Unknown gate or duplicate gate: reject. A gate already satisfied is
		// no longer in the expected set, so a duplicate is rejected here.
		m.ToGMMU.RetrieveIncoming()
		return true
	}
	if wm, seen := state.watermarks[ack.GateID]; seen && wm != ack.Watermark {
		// Changed watermark: reject.
		m.ToGMMU.RetrieveIncoming()
		return true
	}

	state.watermarks[ack.GateID] = ack.Watermark
	delete(state.expected, ack.GateID)
	m.ToGMMU.RetrieveIncoming()

	if len(state.expected) == 0 {
		m.completeUVMBlock(state)
	}

	return true
}

// processUnblockAck aggregates the UnblockAck responses. UnblockAck carries no
// gateID, so the CP counts one ack per pre-registered gate. // sbin_codex
func (m *uvmMiddleware) processUnblockAck(ack *vm.UnblockAck) bool {
	state, ok := m.activeUVMBlocks[ack.CommandID]
	if !ok {
		// Unknown command: reject.
		m.ToGMMU.RetrieveIncoming()
		return true
	}
	if state.isBlock {
		// Wrong command: an UnblockAck for a block command.
		m.ToGMMU.RetrieveIncoming()
		return true
	}
	if len(state.expected) == 0 {
		// More acks than pre-registered gates: reject.
		m.ToGMMU.RetrieveIncoming()
		return true
	}

	for gateID := range state.expected {
		delete(state.expected, gateID)
		break
	}
	m.ToGMMU.RetrieveIncoming()

	if len(state.expected) == 0 {
		m.completeUVMBlock(state)
	}

	return true
}

// completeUVMBlock responds to the driver once the exact gateID set is
// exhausted. // sbin_codex
func (m *uvmMiddleware) completeUVMBlock(state *uvmBlockState) {
	delete(m.activeUVMBlocks, state.commandID)

	rsp := sim.GeneralRspBuilder{}.
		WithSrc(m.ToDriver.AsRemote()).
		WithDst(state.src).
		WithOriginalReq(state.original).
		Build()
	m.ToDriver.Send(rsp)

	tracing.TraceReqComplete(state.original, m.CommandProcessor)
}

// processFaultNotification translates a GMMU-issued typed fault into the
// driver PageFaultReq envelope and routes it to the UVM driver. // sbin_codex
func (m *uvmMiddleware) processFaultNotification(notif *vm.FaultNotification) bool {
	req := protocol.PageFaultReqBuilder{}.
		WithSrc(m.ToDriver.AsRemote()).
		WithDst(m.Driver.AsRemote()).
		WithPID(notif.PID).
		WithGPU(int(notif.GPU)).
		WithVAddr(notif.VAddr).
		WithAccessType(notif.AccessKind).
		WithFaultPendingToken(notif.FaultPendingToken).
		Build()

	if err := m.ToDriver.Send(req); err != nil {
		return false
	}

	m.ToGMMU.RetrieveIncoming()
	tracing.TraceReqReceive(notif, m.CommandProcessor)

	return true
}

// processFaultReplayReq translates the driver replay command into the
// GMMU-owned ReplayRange and routes it to the GMMU through the shared ToGMMU
// seam. // sbin_codex
func (m *uvmMiddleware) processFaultReplayReq(req *protocol.UVMFaultReplayReq) bool {
	replay := &vm.ReplayRange{
		PID:         req.PID,
		GPU:         uint64(req.GPU),
		StartVA:     req.StartVA,
		Size:        req.Size,
		ReplayToken: req.ReplayToken,
	}
	replay.ID = sim.GetIDGenerator().Generate()
	replay.Src = m.ToGMMU.AsRemote()
	replay.Dst = m.ToGMMU.AsRemote()
	m.uvmReplayRangeIDToDriverReqID[replay.ID] = req.ID

	if err := m.ToGMMU.Send(replay); err != nil {
		delete(m.uvmReplayRangeIDToDriverReqID, replay.ID)
		return false
	}

	m.ToDriver.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.CommandProcessor)

	return true
}

// processReplayAck translates the GMMU ReplayAck into the driver replay
// response, correlating the ack to the original driver command. // sbin_codex
func (m *uvmMiddleware) processReplayAck(ack *vm.ReplayAck) bool {
	driverReqID, ok := m.uvmReplayRangeIDToDriverReqID[ack.RspTo]
	if !ok {
		// Unknown replay ack: reject.
		m.ToGMMU.RetrieveIncoming()
		return true
	}
	delete(m.uvmReplayRangeIDToDriverReqID, ack.RspTo)

	rsp := protocol.UVMFaultReplayRspBuilder{}.
		WithSrc(m.ToDriver.AsRemote()).
		WithDst(m.Driver.AsRemote()).
		WithRspTo(driverReqID).
		Build()
	if err := m.ToDriver.Send(rsp); err != nil {
		return false
	}

	m.ToGMMU.RetrieveIncoming()

	return true
}

// processTLBInvalidateReq fans out a range TLB invalidation to the
// topology-present TLB endpoints (uvm-manager.md §21.1). The endpoint set is
// the CP's TLBs, which the topology builder populates per configuration:
// baseline = private L1V/L1S/L1I + shared L2; virtual-caching = private L1I +
// shared L2 only (no fabricated L1V/L1S TLB endpoints). // sbin_codex
func (m *uvmMiddleware) processTLBInvalidateReq(req *protocol.UVMTLBInvalidateReq) bool {
	if len(m.TLBs) == 0 {
		return m.respondTLBInvalidate(req, req.ID)
	}

	state := &uvmTLBInvalidateState{req: req, pending: len(m.TLBs)}
	m.activeUVMTLBInvalidates[req.ID] = state

	for _, port := range m.TLBs {
		inv := tlb.UVMTLBInvalidateReqBuilder{}.
			WithSrc(m.ToTLBs.AsRemote()).
			WithDst(port.AsRemote()).
			WithPID(req.PID).
			WithStartVA(req.StartVA).
			WithSize(req.Size).
			Build()
		m.uvmTLBInvalidateFanout[inv.ID] = req.ID
		if err := m.ToTLBs.Send(inv); err != nil {
			panic(err)
		}
	}

	m.ToDriver.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.CommandProcessor)

	return true
}

// processTLBInvalidateRsp aggregates one endpoint response per fan-out and
// completes the driver request once every endpoint has acknowledged. // sbin_codex
func (m *uvmMiddleware) processTLBInvalidateRsp(rsp *tlb.UVMTLBInvalidateRsp) bool {
	driverReqID, ok := m.uvmTLBInvalidateFanout[rsp.RspTo]
	if !ok {
		// Unknown response: reject.
		m.ToTLBs.RetrieveIncoming()
		return true
	}
	delete(m.uvmTLBInvalidateFanout, rsp.RspTo)

	state, ok := m.activeUVMTLBInvalidates[driverReqID]
	if !ok {
		m.ToTLBs.RetrieveIncoming()
		return true
	}

	state.pending--
	if state.pending > 0 {
		m.ToTLBs.RetrieveIncoming()
		return true
	}

	delete(m.activeUVMTLBInvalidates, driverReqID)
	return m.respondTLBInvalidate(state.req, driverReqID)
}

// respondTLBInvalidate sends the aggregated UVMTLBInvalidateRsp to the driver.
// // sbin_codex
func (m *uvmMiddleware) respondTLBInvalidate(
	req *protocol.UVMTLBInvalidateReq,
	driverReqID string,
) bool {
	rsp := protocol.UVMTLBInvalidateRspBuilder{}.
		WithSrc(m.ToDriver.AsRemote()).
		WithDst(m.Driver.AsRemote()).
		WithRspTo(driverReqID).
		Build()
	if err := m.ToDriver.Send(rsp); err != nil {
		return false
	}

	m.ToTLBs.RetrieveIncoming()
	tracing.TraceReqComplete(req, m.CommandProcessor)

	return true
}

// processCacheRangeFlushReq fans out a range cache writeback/invalidate to the
// data caches only (L1V/L1S/L2). L1I is excluded from data-cache WB+INV. // sbin_codex
func (m *uvmMiddleware) processCacheRangeFlushReq(req *protocol.UVMCacheRangeFlushReq) bool {
	endpoints := m.dataCacheEndpoints()
	if len(endpoints) == 0 {
		return m.respondCacheFlush(req, req.ID)
	}

	state := &uvmCacheFlushState{req: req, pending: len(endpoints)}
	m.activeUVMCacheFlushes[req.ID] = state

	for _, port := range endpoints {
		flush := cache.UVMCacheRangeFlushReqBuilder{}.
			WithSrc(m.ToCaches.AsRemote()).
			WithDst(port.AsRemote()).
			WithOperation(req.Operation).
			WithPID(req.PID).
			WithVABase(req.VABase).
			WithValidPageMask(req.ValidPageMask).
			WithPhysicalRuns(req.PhysicalRuns).
			Build()
		m.uvmCacheFlushFanout[flush.ID] = req.ID
		if err := m.ToCaches.Send(flush); err != nil {
			panic(err)
		}
	}

	m.ToDriver.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.CommandProcessor)

	return true
}

// dataCacheEndpoints returns the data-cache endpoints for range WB+INV: L1V,
// L1S, and L2. L1I is never included. // sbin_codex
func (m *uvmMiddleware) dataCacheEndpoints() []sim.Port {
	endpoints := make([]sim.Port, 0,
		len(m.L1VCaches)+len(m.L1SCaches)+len(m.L2Caches))
	endpoints = append(endpoints, m.L1VCaches...)
	endpoints = append(endpoints, m.L1SCaches...)
	endpoints = append(endpoints, m.L2Caches...)
	return endpoints
}

// processCacheRangeFlushRsp aggregates one endpoint response per fan-out and
// completes the driver request once every data cache has acknowledged. // sbin_codex
func (m *uvmMiddleware) processCacheRangeFlushRsp(rsp *cache.UVMCacheRangeFlushRsp) bool {
	driverReqID, ok := m.uvmCacheFlushFanout[rsp.RspTo]
	if !ok {
		// Unknown response: reject.
		m.ToCaches.RetrieveIncoming()
		return true
	}
	delete(m.uvmCacheFlushFanout, rsp.RspTo)

	state, ok := m.activeUVMCacheFlushes[driverReqID]
	if !ok {
		m.ToCaches.RetrieveIncoming()
		return true
	}

	state.pending--
	if state.pending > 0 {
		m.ToCaches.RetrieveIncoming()
		return true
	}

	delete(m.activeUVMCacheFlushes, driverReqID)
	return m.respondCacheFlush(state.req, driverReqID)
}

// respondCacheFlush sends the aggregated UVMCacheRangeFlushRsp to the driver.
// // sbin_codex
func (m *uvmMiddleware) respondCacheFlush(
	req *protocol.UVMCacheRangeFlushReq,
	driverReqID string,
) bool {
	rsp := protocol.UVMCacheRangeFlushRspBuilder{}.
		WithSrc(m.ToDriver.AsRemote()).
		WithDst(m.Driver.AsRemote()).
		WithRspTo(driverReqID).
		Build()
	if err := m.ToDriver.Send(rsp); err != nil {
		return false
	}

	m.ToCaches.RetrieveIncoming()
	tracing.TraceReqComplete(req, m.CommandProcessor)

	return true
}

// processCounterResetReq routes the acknowledged counter reset to the
// GPU-wide AccessCounter and raises the kernel-dispatch barrier until the ack
// returns (uvm-manager.md §14.2). // sbin_codex
func (m *uvmMiddleware) processCounterResetReq(req *CounterResetReq) bool {
	if m.counterResetPending {
		// A reset is already in flight: reject the duplicate.
		m.ToDriver.RetrieveIncoming()
		return true
	}

	cloned := req.Clone().(*CounterResetReq)
	cloned.Src = m.ToAccessCounter.AsRemote()
	cloned.Dst = m.ToAccessCounter.AsRemote()
	if err := m.ToAccessCounter.Send(cloned); err != nil {
		return false
	}

	m.counterResetPending = true
	m.ToDriver.RetrieveIncoming()
	tracing.TraceReqReceive(req, m.CommandProcessor)

	return true
}

// processCounterResetRsp clears the kernel-dispatch barrier once the
// AccessCounter acknowledges the reset. // sbin_codex
func (m *uvmMiddleware) processCounterResetRsp(rsp *CounterResetRsp) bool {
	m.counterResetPending = false
	m.ToAccessCounter.RetrieveIncoming()
	return true
}

// processMigrationReq routes a UVM migration through the existing DMA engine
// by translating the MigrationReq into the MemCopyH2DReq / MemCopyD2HReq flow
// (uvm-manager.md §23.1). The DMA engine performs the timed transfer; the CP
// never serializes independent migration transfers. // sbin_codex
func (m *uvmMiddleware) processMigrationReq(req *protocol.MigrationReq) bool {
	switch req.Direction {
	case protocol.MigrationCPUToGPU:
		memCopy := &protocol.MemCopyH2DReq{
			SrcBuffer:  []byte{},
			DstAddress: req.VAddr,
		}
		memCopy.ID = sim.GetIDGenerator().Generate()
		memCopy.Src = m.ToDriver.AsRemote()
		memCopy.Dst = m.DMAEngine.AsRemote()
		return m.processMemCopyReq(memCopy)
	case protocol.MigrationGPUToCPU:
		memCopy := &protocol.MemCopyD2HReq{
			SrcAddress: req.VAddr,
			DstBuffer:  []byte{},
		}
		memCopy.ID = sim.GetIDGenerator().Generate()
		memCopy.Src = m.ToDriver.AsRemote()
		memCopy.Dst = m.DMAEngine.AsRemote()
		return m.processMemCopyReq(memCopy)
	}

	panic("unknown migration direction")
}
