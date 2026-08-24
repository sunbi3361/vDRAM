package shaderarray

// sbin_codex: virtual-caching L1V/L1S UVM access-gate unit contract tests
// (plan todo 10 of mgpusim-uvm-manager). Prove the gate classifies probe
// responses by location, stamps GPU_LOCAL requests with the typed annotation,
// retries stale-generation probes, records unique/coalesced waiter counts,
// and never annotates CPU_REMOTE or INVALID requests into the cache.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// fakeGenerationProvider is a mutable generation source for the gate.
type fakeGenerationProvider struct {
	generation uint64
}

func (p *fakeGenerationProvider) Generation() uint64 {
	return p.generation
}

// gateTestAgent sends pending messages and records received ones.
type gateTestAgent struct {
	*sim.TickingComponent
	port     sim.Port
	pending  []sim.Msg
	received []sim.Msg
}

func newGateTestAgent(engine sim.Engine) *gateTestAgent {
	agent := &gateTestAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"GateTestAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "GateTestAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *gateTestAgent) Tick() bool {
	progress := false
	for {
		msg := agent.port.RetrieveIncoming()
		if msg == nil {
			break
		}
		agent.received = append(agent.received, msg)
		progress = true
	}
	if len(agent.pending) > 0 && agent.port.CanSend() {
		if err := agent.port.Send(agent.pending[0]); err == nil {
			agent.pending = agent.pending[1:]
			progress = true
		}
	}
	return progress
}

func (agent *gateTestAgent) enqueue(msg sim.Msg) {
	agent.pending = append(agent.pending, msg)
	agent.TickLater()
}

// gateProbeHarness drives a gate with a stub TLB that answers probes.
type gateProbeHarness struct {
	gate     *VirtualAccessGate
	engine   sim.Engine
	agent    *gateTestAgent
	provider *fakeGenerationProvider
	probes   []*vm.TranslationReq
}

func (h *gateProbeHarness) probeCapture(ctx sim.HookCtx) {
	if ctx.Pos == sim.HookPosPortMsgSend {
		if probe, ok := ctx.Item.(*vm.TranslationReq); ok {
			h.probes = append(h.probes, probe)
		}
	}
}

// probeHook adapts the harness capture to the sim.Hook interface.
type probeHook struct {
	capture func(ctx sim.HookCtx)
}

func (h *probeHook) Func(ctx sim.HookCtx) {
	h.capture(ctx)
}

func newGateProbeHarness(t *testing.T) *gateProbeHarness {
	t.Helper()
	engine := sim.NewSerialEngine()
	gate := &VirtualAccessGate{}
	gate.TickingComponent = sim.NewTickingComponent(
		"UVMGate", engine, 1*sim.GHz, gate)
	gate.topPort = sim.NewPort(gate, 16, 16, "UVMGate.TopPort")
	gate.bottomPort = sim.NewPort(gate, 16, 16, "UVMGate.BottomPort")
	gate.translationPort = sim.NewPort(gate, 16, 16, "UVMGate.TranslationPort")
	gate.ctrlPort = sim.NewPort(gate, 16, 16, "UVMGate.ControlPort")
	gate.AddPort("Top", gate.topPort)
	gate.AddPort("Bottom", gate.bottomPort)
	gate.AddPort("Translation", gate.translationPort)
	gate.AddPort("Control", gate.ctrlPort)
	gate.log2PageSize = 12
	gate.deviceID = 1
	gate.memoryPortMapper = &mem.SinglePortMapper{
		Port: sim.RemotePort("GateTestAgent.Port"),
	}
	gate.translationPortMapper = &mem.SinglePortMapper{
		Port: sim.RemotePort("GateStubTLB.Port"),
	}
	gate.pendingRegions = make(map[uint64]int)
	gate.SetUVMGateID(7)
	provider := &fakeGenerationProvider{}
	gate.SetGenerationProvider(provider)
	gate.AddMiddleware(&gateMiddleware{VirtualAccessGate: gate})

	agent := newGateTestAgent(engine)
	stub := newGateTestAgent(engine)
	stub.TickingComponent = sim.NewTickingComponent(
		"GateStubTLB", engine, 1*sim.GHz, stub)
	stub.port = sim.NewPort(stub, 16, 16, "GateStubTLB.Port")
	stub.AddPort("Port", stub.port)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("UVMGateConn")
	conn.PlugIn(gate.topPort)
	conn.PlugIn(gate.bottomPort)
	conn.PlugIn(gate.translationPort)
	conn.PlugIn(gate.ctrlPort)
	conn.PlugIn(agent.port)
	conn.PlugIn(stub.port)

	h := &gateProbeHarness{
		gate:     gate,
		engine:   engine,
		agent:    agent,
		provider: provider,
	}
	gate.translationPort.AcceptHook(&probeHook{capture: h.probeCapture})
	return h
}

func (h *gateProbeHarness) admit(addr uint64, pid vm.PID) *mem.ReadReq {
	req := mem.ReadReqBuilder{}.
		WithSrc(h.agent.port.AsRemote()).
		WithDst(h.gate.GetPortByName("Top").AsRemote()).
		WithAddress(addr).
		WithByteSize(4).
		WithPID(pid).
		Build()
	h.agent.enqueue(req)
	h.engine.Run()
	return req
}

func (h *gateProbeHarness) answerProbe(
	page vm.Page,
	faultPending vm.FaultPendingToken,
) {
	probe := h.probes[len(h.probes)-1]
	rsp := probe.GenerateRsp(page).(*vm.TranslationRsp)
	rsp.FaultPendingToken = faultPending
	if err := h.gate.GetPortByName("Translation").Deliver(rsp); err != nil {
		panic(err)
	}
	h.engine.Run()
}

func (h *gateProbeHarness) forwardedReads() []*mem.ReadReq {
	reads := make([]*mem.ReadReq, 0)
	for _, msg := range h.agent.received {
		if read, ok := msg.(*mem.ReadReq); ok {
			reads = append(reads, read)
		}
	}
	return reads
}

// TestVirtualGateGPU_LOCALAnnotation proves a GPU_LOCAL response forwards the
// request with the VA retained and the (PID, VA page, HBM PA, location,
// generation) annotation attached.
func TestVirtualGateGPU_LOCALAnnotation(t *testing.T) {
	h := newGateProbeHarness(t)
	h.provider.generation = 3

	h.admit(0x1004, 1)
	if len(h.probes) != 1 {
		t.Fatalf("the gate must probe the L2 TLB, got %d probes", len(h.probes))
	}
	if h.probes[0].WaiterDelta.InitialWaiters != 1 {
		t.Fatalf("the probe must carry the leaf initial waiter count, got %d",
			h.probes[0].WaiterDelta.InitialWaiters)
	}
	h.answerProbe(vm.Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x8000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationGPU_LOCAL,
	}, 0)

	reads := h.forwardedReads()
	if len(reads) != 1 {
		t.Fatalf("the gate must forward exactly one request, got %d", len(reads))
	}
	if reads[0].GetAddress() != 0x1004 {
		t.Fatalf("the forwarded request must retain the VA, got 0x%x",
			reads[0].GetAddress())
	}
	ann := cache.ResolveAnnotation(reads[0])
	if ann == nil {
		t.Fatal("the forwarded request must carry the typed annotation")
	}
	if ann.PID != 1 || ann.VAPage != 0x1000 || ann.HBMPA != 0x8000 ||
		ann.Location != vm.MemoryLocationGPU_LOCAL || ann.Generation != 3 {
		t.Fatalf("the annotation must be (1, 0x1000, 0x8000, GPU_LOCAL, 3), got %+v",
			ann)
	}
}

// TestVirtualGateStaleGenerationRetries proves a probe response arriving after
// a generation change is retried with a fresh probe instead of being admitted
// with a stale mapping.
func TestVirtualGateStaleGenerationRetries(t *testing.T) {
	h := newGateProbeHarness(t)
	h.provider.generation = 1

	h.admit(0x1004, 1)
	if len(h.probes) != 1 {
		t.Fatalf("the gate must probe the L2 TLB, got %d probes", len(h.probes))
	}
	probe1 := h.probes[0]

	// The mapping is published: generation advances before the response.
	h.provider.generation = 2
	rsp := probe1.GenerateRsp(vm.Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x8000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationGPU_LOCAL,
	}).(*vm.TranslationRsp)
	if err := h.gate.GetPortByName("Translation").Deliver(rsp); err != nil {
		t.Fatalf("deliver stale response: %v", err)
	}
	h.engine.Run()

	// The stale response must be retried: a fresh probe leaves the gate.
	if len(h.probes) != 2 {
		t.Fatalf("the stale response must be retried with a fresh probe, got %d probes",
			len(h.probes))
	}
	if h.probes[1].ID == probe1.ID {
		t.Fatal("the retry must be a new probe")
	}

	// The fresh response admits the request with the current generation.
	h.answerProbe(vm.Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x8000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationGPU_LOCAL,
	}, 0)
	reads := h.forwardedReads()
	if len(reads) != 1 {
		t.Fatalf("the fresh response must forward the request, got %d reads",
			len(reads))
	}
	if ann := cache.ResolveAnnotation(reads[0]); ann == nil || ann.Generation != 2 {
		t.Fatalf("the annotation must carry the current generation 2, got %+v",
			cache.ResolveAnnotation(reads[0]))
	}
}

// TestVirtualGateRemoteAndInvalidNeverAnnotated proves CPU_REMOTE reads are
// committed to the endpoint only and INVALID responses retain the request;
// neither ever reaches the cache with an annotation.
func TestVirtualGateRemoteAndInvalidNeverAnnotated(t *testing.T) {
	h := newGateProbeHarness(t)

	h.admit(0x2004, 1)
	h.answerProbe(vm.Page{
		PID: 1, VAddr: 0x2000, PAddr: 0x9000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationCPU_REMOTE,
	}, 0)
	if reads := h.forwardedReads(); len(reads) != 0 {
		t.Fatalf("the CPU_REMOTE read must never enter the cache, got %d reads",
			len(reads))
	}

	h.admit(0x3004, 1)
	h.answerProbe(vm.Page{
		PID: 1, VAddr: 0x3000, PAddr: 0,
		PageSize: 4096, Valid: false, Managed: true,
		Location: vm.MemoryLocationINVALID,
	}, 7)
	if reads := h.forwardedReads(); len(reads) != 0 {
		t.Fatalf("the INVALID request must never enter the cache, got %d reads",
			len(reads))
	}
}

// TestVirtualGateWaiterCounts proves the gate records one unique waiter for
// the first request of a 64 KB region and coalesced waiters for later
// requests of the same region.
func TestVirtualGateWaiterCounts(t *testing.T) {
	h := newGateProbeHarness(t)

	h.admit(0x1004, 1)
	h.admit(0x1104, 1)
	h.admit(0x20004, 1)

	unique, coalesced := h.gate.WaiterCounts()
	if len(h.probes) != 3 {
		t.Fatalf("the gate must probe all three requests, got %d probes",
			len(h.probes))
	}
	if unique != 2 || coalesced != 1 {
		t.Fatalf("the gate must record 2 unique and 1 coalesced waiter, got %d/%d",
			unique, coalesced)
	}
	if coalesced != 1 {
		t.Fatalf("the gate must record 1 coalesced waiter, got %d", coalesced)
	}
	if raw := unique + coalesced; raw != 3 {
		t.Fatalf("the raw waiter count must be 3, got %d", raw)
	}
}
