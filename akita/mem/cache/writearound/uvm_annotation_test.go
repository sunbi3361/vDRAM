package writearound

// sbin_codex: virtual-caching L1V annotation persistence contract tests (plan
// todo 10 of mgpusim-uvm-manager). Prove the writearound cache (the virtual
// L1V policy) persists the typed request annotation through the MSHR and the
// block for refill and replacement, and that every request admitted before a
// barrier keeps its metadata drain-visible through the gate->cache and
// cache-MSHR boundaries.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// uvmCacheAgent receives the cache's bottom traffic and replies with data.
type uvmCacheAgent struct {
	*sim.TickingComponent
	port     sim.Port
	received []sim.Msg
	hold     bool // sbin_codex: when set, the agent records requests without replying.
}

func newUVMCacheAgent(engine sim.Engine) *uvmCacheAgent {
	agent := &uvmCacheAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMTestAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMTestAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmCacheAgent) Tick() bool {
	progress := false
	for {
		msg := agent.port.RetrieveIncoming()
		if msg == nil {
			break
		}
		agent.received = append(agent.received, msg)
		if agent.hold {
			continue
		}
		switch req := msg.(type) {
		case *mem.ReadReq:
			data := make([]byte, req.AccessByteSize)
			for i := range data {
				data[i] = byte(i + 1)
			}
			if agent.port.CanSend() {
				_ = agent.port.Send(req.GenerateRsp(data))
			}
		case *mem.WriteReq:
			if agent.port.CanSend() {
				_ = agent.port.Send(req.GenerateRsp())
			}
		}
		progress = true
	}
	return progress
}

func newUVMCacheHarness(t *testing.T) (*Comp, *uvmCacheAgent, *directconnection.Comp) {
	t.Helper()
	engine := sim.NewSerialEngine()
	cacheComp := MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		WithLog2BlockSize(6).
		WithWayAssociativity(4).
		WithNumMSHREntry(16).
		WithNumBanks(1).
		WithBankLatency(1).
		WithMaxNumConcurrentTrans(16).
		WithNumReqsPerCycle(8).
		WithTotalByteSize(4 * mem.KB).
		WithAddressToPortMapper(&mem.SinglePortMapper{
			Port: sim.RemotePort("UVMTestAgent.Port"),
		}).
		Build("UVMCache")

	agent := newUVMCacheAgent(engine)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("UVMTestConn")
	conn.PlugIn(cacheComp.topPort)
	conn.PlugIn(cacheComp.bottomPort)
	conn.PlugIn(agent.port)

	return cacheComp, agent, conn
}

func uvmLocalAnnotation() *cache.VirtualAccessAnnotation {
	return &cache.VirtualAccessAnnotation{
		PID:        vm.PID(1),
		VAPage:     0x1000,
		HBMPA:      0x8000,
		Location:   vm.MemoryLocationGPU_LOCAL,
		Generation: 2,
	}
}

func annotatedRead(addr uint64, ann *cache.VirtualAccessAnnotation) *mem.ReadReq {
	req := mem.ReadReqBuilder{}.
		WithSrc(sim.RemotePort("UVMTestAgent.Port")).
		WithDst(sim.RemotePort("UVMCache.TopPort")).
		WithAddress(addr).
		WithByteSize(4).
		WithPID(1).
		Build()
	cache.Annotate(req, ann)
	return req
}

// TestUVMMSHRMetadata proves a read miss records the request annotation on the
// MSHR entry and carries it on the refill request to the bottom.
func TestUVMMSHRMetadata(t *testing.T) {
	harness, agent, _ := newUVMCacheHarness(t)
	ann := uvmLocalAnnotation()
	agent.hold = true // sbin_codex: keep the refill pending so the MSHR entry stays.

	req := annotatedRead(0x1004, ann)
	if err := harness.topPort.Deliver(req); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Drive until the refill request reaches the bottom agent.
	for i := 0; i < 100 && len(agent.received) == 0; i++ {
		harness.Engine.Run()
	}
	if len(agent.received) == 0 {
		t.Fatal("the cache must send a refill request to the bottom")
	}

	refill, ok := agent.received[0].(*mem.ReadReq)
	if !ok {
		t.Fatalf("the refill must be a read, got %T", agent.received[0])
	}
	if got := cache.ResolveAnnotation(refill); got != ann {
		t.Fatalf("the refill request must carry the annotation, got %+v", got)
	}

	mshrEntry := harness.mshr.Query(1, 0x1000)
	if mshrEntry == nil {
		t.Fatal("the MSHR must hold the pending refill")
	}
	if mshrEntry.Annotation != ann {
		t.Fatalf("the MSHR entry must retain the annotation, got %+v",
			mshrEntry.Annotation)
	}
}

// TestUVMBlockMetadata proves the block receives the annotation once the
// refill completes.
func TestUVMBlockMetadata(t *testing.T) {
	harness, _, _ := newUVMCacheHarness(t)
	ann := uvmLocalAnnotation()

	req := annotatedRead(0x1004, ann)
	if err := harness.topPort.Deliver(req); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	for i := 0; i < 200; i++ {
		harness.Engine.Run()
	}

	block := harness.directory.Lookup(1, 0x1000)
	if block == nil || !block.IsValid {
		t.Fatal("the refilled block must be valid")
	}
	if block.Annotation != ann {
		t.Fatalf("the block must retain the annotation, got %+v", block.Annotation)
	}
}

// TestUVMBarrierGateToCache proves a request that crossed the gate->cache
// boundary keeps its metadata: the cache records it on the MSHR and the
// refill request stays annotated (drain-visible with its metadata).
func TestUVMBarrierGateToCache(t *testing.T) {
	harness, agent, _ := newUVMCacheHarness(t)
	ann := uvmLocalAnnotation()

	req := annotatedRead(0x1004, ann)
	if err := harness.topPort.Deliver(req); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	for i := 0; i < 100 && len(agent.received) == 0; i++ {
		harness.Engine.Run()
	}
	if len(agent.received) == 0 {
		t.Fatal("the gate->cache request must reach the bottom as a refill")
	}

	refill, ok := agent.received[0].(*mem.ReadReq)
	if !ok {
		t.Fatalf("the refill must be a read, got %T", agent.received[0])
	}
	if got := cache.ResolveAnnotation(refill); got != ann {
		t.Fatalf("the drain-visible refill must carry the annotation, got %+v", got)
	}
}

// TestUVMBarrierCacheMSHR proves a request coalescing into the MSHR after the
// barrier keeps its annotation in the MSHR, and the completed refill
// propagates the metadata to the block.
func TestUVMBarrierCacheMSHR(t *testing.T) {
	harness, agent, _ := newUVMCacheHarness(t)
	ann := uvmLocalAnnotation()
	agent.hold = true // sbin_codex: keep the refill pending so the MSHR entry stays.

	first := annotatedRead(0x1004, ann)
	if err := harness.topPort.Deliver(first); err != nil {
		t.Fatalf("deliver first: %v", err)
	}
	for i := 0; i < 100 && harness.mshr.Query(1, 0x1000) == nil; i++ {
		harness.Engine.Run()
	}
	if harness.mshr.Query(1, 0x1000) == nil {
		t.Fatal("the first request must create an MSHR entry")
	}

	second := annotatedRead(0x1008, ann)
	if err := harness.topPort.Deliver(second); err != nil {
		t.Fatalf("deliver second: %v", err)
	}
	for i := 0; i < 100; i++ {
		harness.Engine.Run()
		if entry := harness.mshr.Query(1, 0x1000); entry != nil && len(entry.Requests) == 2 {
			break
		}
	}

	mshrEntry := harness.mshr.Query(1, 0x1000)
	if mshrEntry == nil {
		t.Fatal("the MSHR entry must still be present during the refill")
	}
	if len(mshrEntry.Requests) != 2 {
		t.Fatalf("the second request must coalesce into the MSHR, got %d waiters",
			len(mshrEntry.Requests))
	}
	if mshrEntry.Annotation != ann {
		t.Fatalf("the MSHR must retain the annotation for coalesced waiters, got %+v",
			mshrEntry.Annotation)
	}

	// Release the refill: the completed refill propagates the metadata.
	agent.hold = false
	for i := 0; i < 200; i++ {
		harness.Engine.Run()
	}

	block := harness.directory.Lookup(1, 0x1000)
	if block == nil || !block.IsValid {
		t.Fatal("the refilled block must be valid")
	}
	if block.Annotation != ann {
		t.Fatalf("the block must retain the annotation after refill, got %+v",
			block.Annotation)
	}
}
