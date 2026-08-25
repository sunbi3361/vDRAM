package cp

// sbin_codex: the Command Processor is the GPU-side endpoint of the UVM
// control plane (spec 2.1). Page faults raised inside the GPU leave through
// here, and every driver command enters here and is dispatched to the GMMU or
// to the cache hierarchy.
//
// Nothing in this path quiesces the GPU. UVM migration must never reuse the
// heavyweight ShootDownCommand / GPURestartReq sequence, which stays reserved
// for the legacy unified-memory migration flow.

import (
	"fmt"
	"os"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

var uvmDbg = os.Getenv("UVM_DEBUG") != ""

func dbgc(format string, args ...interface{}) {
	if uvmDbg {
		fmt.Fprintf(os.Stderr, "[cpdbg] "+format+"\n", args...)
	}
}

type uvmMiddleware struct {
	*CommandProcessor
}

func (m *uvmMiddleware) Tick() bool {
	if m.ToUVMDriver == nil {
		return false
	}

	madeProgress := m.processFromDriver()
	madeProgress = m.processFromInternal() || madeProgress
	madeProgress = m.collectDrainAcks() || madeProgress
	madeProgress = m.issueCacheRangeFlush() || madeProgress
	madeProgress = m.completeCacheRangeFlush() || madeProgress

	return madeProgress
}

// processFromDriver dispatches a host UVM command into the GPU.
func (m *uvmMiddleware) processFromDriver() bool {
	msg := m.ToUVMDriver.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *vm.UVMTLBInvalidateReq:
		return m.forwardToGMMU(msg)
	case *vm.UVMFaultReplayReq:
		return m.forwardReplay(msg)
	case *vm.PageFaultRsp:
		return m.forwardToGMMU(msg)
	case *vm.AccessCounterResetReq:
		return m.forwardToAccessCounter(msg)
	case *protocol.UVMCacheRangeFlushReq:
		return m.startCacheRangeFlush(msg)
	case *protocol.UVMRemoteDrainReq:
		return m.startRemoteDrain(msg)
	default:
		return false
	}
}

// processFromInternal relays a GPU-originated UVM message to the host driver.
func (m *uvmMiddleware) processFromInternal() bool {
	if m.ToUVMInternal == nil {
		return false
	}

	msg := m.ToUVMInternal.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *vm.PageFaultReq,
		*vm.UVMTLBInvalidateRsp,
		*vm.AccessCounterNotifyReq:
		return m.forwardToDriver(msg)
	case *vm.UVMDrainRangeRsp:
		return m.completeRemoteDrain(msg)
	default:
		return false
	}
}

// forwardToGMMU relays a driver command without changing its ID, so the GMMU
// response correlates directly with the driver's outstanding request.
func (m *uvmMiddleware) forwardToGMMU(msg sim.Msg) bool {
	if m.ToUVMInternal == nil || m.GMMU == "" {
		return false
	}

	if !m.ToUVMInternal.CanSend() {
		return false
	}

	msg.Meta().Src = m.ToUVMInternal.AsRemote()
	msg.Meta().Dst = m.GMMU

	if err := m.ToUVMInternal.Send(msg); err != nil {
		return false
	}

	m.ToUVMDriver.RetrieveIncoming()

	return true
}

// forwardReplay delivers one region replay to both GPU-side owners of stalled
// requests: the GMMU, which holds faulted translations, and the access
// counter, which holds writes it refused to perform remotely.
func (m *uvmMiddleware) forwardReplay(req *vm.UVMFaultReplayReq) bool {
	if m.AccessCounter != "" {
		if !m.ToUVMInternal.CanSend() {
			return false
		}

		toCounter := req.Clone().(*vm.UVMFaultReplayReq)
		toCounter.Src = m.ToUVMInternal.AsRemote()
		toCounter.Dst = m.AccessCounter

		if err := m.ToUVMInternal.Send(toCounter); err != nil {
			return false
		}
	}

	return m.forwardToGMMU(req)
}

func (m *uvmMiddleware) forwardToAccessCounter(msg sim.Msg) bool {
	if m.ToUVMInternal == nil || m.AccessCounter == "" {
		// No GPU-side access counter is configured; drop the command rather
		// than blocking the control port.
		m.ToUVMDriver.RetrieveIncoming()
		return true
	}

	if !m.ToUVMInternal.CanSend() {
		return false
	}

	msg.Meta().Src = m.ToUVMInternal.AsRemote()
	msg.Meta().Dst = m.AccessCounter

	if err := m.ToUVMInternal.Send(msg); err != nil {
		return false
	}

	m.ToUVMDriver.RetrieveIncoming()

	return true
}

func (m *uvmMiddleware) forwardToDriver(msg sim.Msg) bool {
	if m.UVMDriverPort == "" {
		return false
	}

	if !m.ToUVMDriver.CanSend() {
		return false
	}

	msg.Meta().Src = m.ToUVMDriver.AsRemote()
	msg.Meta().Dst = m.UVMDriverPort

	if err := m.ToUVMDriver.Send(msg); err != nil {
		return false
	}

	m.ToUVMInternal.RetrieveIncoming()

	return true
}

// startCacheRangeFlush begins a region writeback+invalidate. It first asks
// every address translator to drain the region: the driver has already
// invalidated the region's translations, so once no translator has an
// outstanding request for it, every store to the victim has committed into the
// caches and the writeback that follows is exhaustive.
func (m *uvmMiddleware) startCacheRangeFlush(
	req *protocol.UVMCacheRangeFlushReq,
) bool {
	if m.currCacheRangeFlush != nil {
		return false
	}

	if !m.ToAddressTranslators.CanSend() {
		return false
	}

	for _, translator := range m.UVMTranslators {
		drain := vm.NewUVMDrainRangeReq(
			m.ToAddressTranslators.AsRemote(), translator)
		drain.PID = req.PID
		drain.StartVA = req.StartVAddr
		drain.Size = req.Size

		if err := m.ToAddressTranslators.Send(drain); err != nil {
			panic("CP could not fan out a UVM region drain")
		}

		m.numUVMDrainAck++
	}

	m.currCacheRangeFlush = req
	m.ToUVMDriver.RetrieveIncoming()
	dbgc("FLUSH-START region=%#x drainAcks=%d", req.StartVAddr, m.numUVMDrainAck)

	return true
}

// startRemoteDrain forwards a region drain to the GPU access counter, which
// owns the outstanding remote accesses. // sbin_codex
func (m *uvmMiddleware) startRemoteDrain(
	req *protocol.UVMRemoteDrainReq,
) bool {
	if m.AccessCounter == "" {
		// No counter means no remote access can be outstanding.
		if !m.ToUVMDriver.CanSend() {
			return false
		}

		rsp := protocol.NewUVMRemoteDrainRsp(
			m.ToUVMDriver.AsRemote(), req.Src, req.ID)
		if err := m.ToUVMDriver.Send(rsp); err != nil {
			return false
		}

		m.ToUVMDriver.RetrieveIncoming()

		return true
	}

	if m.currRemoteDrain != nil || !m.ToUVMInternal.CanSend() {
		return false
	}

	drain := vm.NewUVMDrainRangeReq(
		m.ToUVMInternal.AsRemote(), m.AccessCounter)
	drain.PID = req.PID
	drain.StartVA = req.StartVAddr
	drain.Size = req.Size

	if err := m.ToUVMInternal.Send(drain); err != nil {
		return false
	}

	m.currRemoteDrain = req
	m.remoteDrainID = drain.ID
	m.ToUVMDriver.RetrieveIncoming()

	return true
}

// completeRemoteDrain relays the counter's acknowledgement to the driver.
func (m *uvmMiddleware) completeRemoteDrain(rsp *vm.UVMDrainRangeRsp) bool {
	req := m.currRemoteDrain
	if req == nil || rsp.RespondTo != m.remoteDrainID {
		m.ToUVMInternal.RetrieveIncoming()
		return true
	}

	if !m.ToUVMDriver.CanSend() {
		return false
	}

	out := protocol.NewUVMRemoteDrainRsp(
		m.ToUVMDriver.AsRemote(), req.Src, req.ID)
	if err := m.ToUVMDriver.Send(out); err != nil {
		return false
	}

	m.ToUVMInternal.RetrieveIncoming()
	m.currRemoteDrain = nil
	m.remoteDrainID = ""

	return true
}

// collectDrainAcks consumes one translator drain acknowledgement.
func (m *uvmMiddleware) collectDrainAcks() bool {
	msg := m.ToAddressTranslators.PeekIncoming()
	if msg == nil {
		return false
	}

	if _, ok := msg.(*vm.UVMDrainRangeRsp); !ok {
		return false
	}

	m.ToAddressTranslators.RetrieveIncoming()

	if m.numUVMDrainAck > 0 {
		m.numUVMDrainAck--
	}

	return true
}

// issueCacheRangeFlush fans the writeback+invalidate out once every translator
// has drained the region.
func (m *uvmMiddleware) issueCacheRangeFlush() bool {
	req := m.currCacheRangeFlush
	if req == nil || m.numUVMDrainAck > 0 || m.cacheFlushIssued {
		return false
	}

	if !m.ToCaches.CanSend() {
		return false
	}

	targets := make([]sim.Port, 0,
		len(m.L1VCaches)+len(m.L1SCaches)+len(m.L2Caches))
	targets = append(targets, m.L1VCaches...)
	targets = append(targets, m.L1SCaches...)
	targets = append(targets, m.L2Caches...)

	for _, target := range targets {
		flush := cache.RangeFlushReqBuilder{}.
			WithSrc(m.ToCaches.AsRemote()).
			WithDst(target.AsRemote()).
			WithPID(req.PID).
			WithVirtualRange(req.StartVAddr, req.Size).
			WithPhysicalFrames(req.PAddrs, req.PageSize).
			Writeback().
			Invalidate().
			Build()

		if err := m.ToCaches.Send(flush); err != nil {
			panic("CP could not fan out a UVM cache range flush")
		}

		m.numCacheRangeFlushAck++
	}

	m.cacheFlushIssued = true
	dbgc("FLUSH-ISSUE region=%#x", req.StartVAddr)

	return true
}

func (m *uvmMiddleware) completeCacheRangeFlush() bool {
	req := m.currCacheRangeFlush
	if req == nil || !m.cacheFlushIssued || m.numCacheRangeFlushAck > 0 {
		return false
	}

	if !m.ToUVMDriver.CanSend() {
		return false
	}

	rsp := protocol.NewUVMCacheRangeFlushRsp(
		m.ToUVMDriver.AsRemote(), req.Src, req.ID)

	if err := m.ToUVMDriver.Send(rsp); err != nil {
		return false
	}

	m.currCacheRangeFlush = nil
	m.cacheFlushIssued = false

	return true
}

// processCacheRangeFlushRsp consumes one cache acknowledgement. It is called
// from the control middleware, which owns the ToCaches port.
func (m *ctrlMiddleware) processCacheRangeFlushRsp(
	rsp *cache.RangeFlushRsp,
) bool {
	if m.numCacheRangeFlushAck > 0 {
		m.numCacheRangeFlushAck--
	}

	m.ToCaches.RetrieveIncoming()
	_ = rsp

	return true
}
