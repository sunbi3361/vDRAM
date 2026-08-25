package uvm

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

// RemoteAccessAnnotation is the translation contract stamped by the access
// gates onto requests routed to the remote endpoint. Only the CPU_REMOTE
// translation's CPU PA is consumable by the endpoint
// (vm.ConsumableAddress(remote=true), uvm-manager.md §13.1); INVALID and
// GPU_LOCAL translations are rejected and never routed remotely.
//
// sbin_codex: the annotation rides the request Info slot; the gate-to-endpoint
// forwarding wiring is completed by the topology builders.
type RemoteAccessAnnotation struct {
	Location vm.MemoryLocation
	PAddr    uint64
}

// inflightRead tracks a remote read forwarded over modeled PCIe so the
// matching data response can be routed back to the original GPU requester.
type inflightRead struct {
	original  *mem.ReadReq
	forwarded *mem.ReadReq
}

// RemoteEndpoint is the CPU-remote memory endpoint (uvm-manager.md §13, §15).
// It consumes the CPU_REMOTE translation's CPU PA, forwards non-cacheable
// read traffic over modeled PCIe (RDMA) to global storage, returns the host
// data to the original requester, and reports each served read to the
// GPU-wide AccessCounter. Normal remote writes are parked and never committed
// to host memory (§15); unsupported atomics are rejected explicitly rather
// than silently treated as ordinary writes (§15.1).
//
// sbin_codex: the ToGPU and ToRDMA ports are shared with the access gates and
// the RDMA engine respectively, so requests and responses are addressed
// loop-back style (Src = Dst = the shared port's remote).
type RemoteEndpoint struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	// ToGPU receives remote accesses from the access gates and returns the
	// host data to the original requester.
	ToGPU sim.Port
	// ToRDMA is the modeled-PCIe seam: reads are forwarded to the RDMA engine
	// and data responses arrive back on the same port.
	ToRDMA sim.Port

	gpuID           int
	counter         *AccessCounter
	inflight        []inflightRead
	rejectedAtomics uint64
}

// Tick drives the endpoint pipeline.
func (e *RemoteEndpoint) Tick() bool {
	return e.MiddlewareHolder.Tick()
}

// RejectedAtomicCount returns the number of unsupported remote atomics that
// were explicitly rejected (uvm-manager.md §15.1).
func (e *RemoteEndpoint) RejectedAtomicCount() uint64 {
	return e.rejectedAtomics
}

// remoteEndpointMiddleware drives the endpoint's request and response
// stages.
type remoteEndpointMiddleware struct {
	*RemoteEndpoint
}

// Tick admits one remote access and returns one data response per call.
func (m *remoteEndpointMiddleware) Tick() bool {
	madeProgress := false
	madeProgress = m.processRemoteAccess() || madeProgress
	madeProgress = m.processDataRsp() || madeProgress
	return madeProgress
}

// processRemoteAccess consumes one remote access from the gates. Reads are
// served over modeled PCIe, writes are parked (§15), and any other request
// type is an unsupported atomic that is rejected explicitly (§15.1).
func (m *remoteEndpointMiddleware) processRemoteAccess() bool {
	msg := m.ToGPU.PeekIncoming()
	if msg == nil {
		return false
	}
	req, ok := msg.(mem.AccessReq)
	if !ok {
		log.Panicf("cannot process request of type %s", reflect.TypeOf(msg))
	}
	switch req := req.(type) {
	case *mem.ReadReq:
		return m.serveRemoteRead(req)
	case *mem.WriteReq:
		// A normal remote write is parked: it is never committed to host
		// memory through the remote-access path (uvm-manager.md §15).
		m.ToGPU.RetrieveIncoming()
		return true
	default:
		// The selected memory-request protocol cannot represent remote
		// atomic semantics: reject explicitly instead of silently treating
		// the atomic as an ordinary write (§15.1).
		m.rejectedAtomics++
		m.ToGPU.RetrieveIncoming()
		return true
	}
}

// serveRemoteRead resolves the consumable CPU PA of the CPU_REMOTE
// translation, forwards the non-cacheable read over modeled PCIe to global
// storage, and reports the access to the GPU-wide AccessCounter.
func (m *remoteEndpointMiddleware) serveRemoteRead(req *mem.ReadReq) bool {
	ann, ok := req.Info.(*RemoteAccessAnnotation)
	if !ok {
		// No translation contract: nothing to consume.
		m.ToGPU.RetrieveIncoming()
		return true
	}
	addr, ok := vm.ConsumableAddress(ann.Location, ann.PAddr, true)
	if !ok {
		// INVALID and GPU_LOCAL translations have no remote endpoint request;
		// a GPU_LOCAL PA never routes remotely (uvm-manager.md §13.2).
		m.ToGPU.RetrieveIncoming()
		return true
	}

	forwarded := mem.ReadReqBuilder{}.
		WithSrc(m.ToRDMA.AsRemote()).
		WithDst(m.ToRDMA.AsRemote()).
		WithAddress(addr).
		WithByteSize(req.AccessByteSize).
		WithPID(req.PID).
		Build()
	// sbin_codex (todo 25): the forwarded read is a loop-back message on the
	// shared ToRDMA seam (Src == Dst == ToRDMA.AsRemote()); a real port's
	// Send rejects src == dst, so the read is delivered directly into the
	// shared port's incoming buffer, which the RDMA engine peeks.
	// if err := m.ToRDMA.Send(forwarded); err != nil {
	// 	return false
	// }
	if err := m.ToRDMA.Deliver(forwarded); err != nil {
		return false
	}

	m.inflight = append(m.inflight, inflightRead{
		original:  req,
		forwarded: forwarded,
	})
	m.ToGPU.RetrieveIncoming()

	if m.counter != nil {
		m.counter.RecordRemoteAccess(req.PID, m.gpuID, req.GetAddress())
	}

	tracing.TraceReqReceive(req, m.RemoteEndpoint)
	tracing.TraceReqInitiate(
		forwarded,
		m.RemoteEndpoint,
		tracing.MsgIDAtReceiver(req, m.RemoteEndpoint),
	)

	return true
}

// processDataRsp routes one modeled-PCIe data response back to the original
// GPU requester. A non-response message on the shared ToRDMA seam (e.g. the
// endpoint's own loop-back forwarded read) is left for the RDMA engine.
// sbin_codex (todo 25)
func (m *remoteEndpointMiddleware) processDataRsp() bool {
	msg := m.ToRDMA.PeekIncoming()
	if msg == nil {
		return false
	}
	rsp, ok := msg.(*mem.DataReadyRsp)
	if !ok {
		// sbin_codex (todo 25): the shared ToRDMA seam carries the
		// endpoint's own loop-back forwarded reads; skip them instead of
		// panicking.
		return false
	}
	idx := m.findInflight(rsp.RespondTo)
	if idx < 0 {
		// Unknown response: drop.
		m.ToRDMA.RetrieveIncoming()
		return true
	}
	original := m.inflight[idx].original
	forwarded := m.inflight[idx].forwarded

	rspToGPU := mem.DataReadyRspBuilder{}.
		WithSrc(m.ToGPU.AsRemote()).
		WithDst(original.Src).
		WithRspTo(original.ID).
		WithData(rsp.Data).
		Build()
	if err := m.ToGPU.Send(rspToGPU); err != nil {
		return false
	}

	m.inflight = append(m.inflight[:idx], m.inflight[idx+1:]...)
	m.ToRDMA.RetrieveIncoming()

	tracing.TraceReqFinalize(forwarded, m.RemoteEndpoint)
	tracing.TraceReqComplete(original, m.RemoteEndpoint)

	return true
}

// findInflight locates the tracked read that a data response answers.
func (m *remoteEndpointMiddleware) findInflight(rspTo string) int {
	for i, read := range m.inflight {
		if read.forwarded.ID == rspTo {
			return i
		}
	}
	return -1
}

// RemoteEndpointBuilder builds a RemoteEndpoint.
type RemoteEndpointBuilder struct {
	engine  sim.Engine
	freq    sim.Freq
	gpuID   int
	counter *AccessCounter
}

// MakeRemoteEndpointBuilder creates a new RemoteEndpoint builder.
func MakeRemoteEndpointBuilder() RemoteEndpointBuilder {
	return RemoteEndpointBuilder{freq: 1 * sim.GHz}
}

// WithEngine sets the engine of the endpoint to build.
func (b RemoteEndpointBuilder) WithEngine(engine sim.Engine) RemoteEndpointBuilder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency of the endpoint to build.
func (b RemoteEndpointBuilder) WithFreq(freq sim.Freq) RemoteEndpointBuilder {
	b.freq = freq
	return b
}

// WithGPU sets the GPU ID the endpoint reports to the AccessCounter.
func (b RemoteEndpointBuilder) WithGPU(gpuID int) RemoteEndpointBuilder {
	b.gpuID = gpuID
	return b
}

// WithAccessCounter wires the GPU-wide AccessCounter the endpoint reports
// served remote reads to.
func (b RemoteEndpointBuilder) WithAccessCounter(
	counter *AccessCounter,
) RemoteEndpointBuilder {
	b.counter = counter
	return b
}

// Build creates a new RemoteEndpoint.
func (b RemoteEndpointBuilder) Build(name string) *RemoteEndpoint {
	e := &RemoteEndpoint{
		gpuID:    b.gpuID,
		counter:  b.counter,
		inflight: make([]inflightRead, 0),
	}
	e.TickingComponent = sim.NewTickingComponent(name, b.engine, b.freq, e)
	e.ToGPU = sim.NewPort(e, 32, 32, name+".ToGPU")
	e.ToRDMA = sim.NewPort(e, 32, 32, name+".ToRDMA")
	e.AddPort("ToGPU", e.ToGPU)
	e.AddPort("ToRDMA", e.ToRDMA)
	middleware := &remoteEndpointMiddleware{RemoteEndpoint: e}
	e.AddMiddleware(middleware)
	return e
}
