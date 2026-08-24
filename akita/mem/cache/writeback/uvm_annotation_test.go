package writeback

// sbin_codex: virtual-caching L2 annotation persistence contract test (plan
// todo 10 of mgpusim-uvm-manager). Prove the writeback cache (the virtual L2
// policy) persists the typed request annotation on the block and carries it on
// the dirty-replacement writeback, so replacement traffic stays annotated.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// uvmWBAgent receives the cache's bottom traffic and replies with data.
type uvmWBAgent struct {
	*sim.TickingComponent
	port     sim.Port
	received []sim.Msg
}

func newUVMWBAgent(engine sim.Engine) *uvmWBAgent {
	agent := &uvmWBAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMWBTestAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMWBTestAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmWBAgent) Tick() bool {
	progress := false
	for {
		msg := agent.port.RetrieveIncoming()
		if msg == nil {
			break
		}
		agent.received = append(agent.received, msg)
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

func newUVMWBHarness(t *testing.T) (*Comp, *uvmWBAgent) {
	t.Helper()
	engine := sim.NewSerialEngine()
	cacheComp := MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		WithLog2BlockSize(6).
		WithWayAssociativity(1).
		WithNumMSHREntry(16).
		WithNumReqPerCycle(8).
		WithByteSize(2 * mem.KB).
		WithBankLatency(1).
		WithDirectoryLatency(1).
		WithAddressToPortMapper(&mem.SinglePortMapper{
			Port: sim.RemotePort("UVMWBTestAgent.Port"),
		}).
		Build("UVMWBCache")

	agent := newUVMWBAgent(engine)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("UVMWBTestConn")
	conn.PlugIn(cacheComp.topPort)
	conn.PlugIn(cacheComp.bottomPort)
	conn.PlugIn(agent.port)

	return cacheComp, agent
}

func uvmWBAnnotation() *cache.VirtualAccessAnnotation {
	return &cache.VirtualAccessAnnotation{
		PID:        vm.PID(1),
		VAPage:     0x1000,
		HBMPA:      0x8000,
		Location:   vm.MemoryLocationGPU_LOCAL,
		Generation: 2,
	}
}

func annotatedWrite(addr uint64, ann *cache.VirtualAccessAnnotation) *mem.WriteReq {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i + 7)
	}
	req := mem.WriteReqBuilder{}.
		WithSrc(sim.RemotePort("UVMWBTestAgent.Port")).
		WithDst(sim.RemotePort("UVMWBCache.TopPort")).
		WithAddress(addr).
		WithData(data).
		WithPID(1).
		Build()
	cache.Annotate(req, ann)
	return req
}

// TestUVMVirtualDirtyReplacement proves a dirty block keeps its annotation and
// the replacement writeback carries the stored metadata.
func TestUVMVirtualDirtyReplacement(t *testing.T) {
	harness, agent := newUVMWBHarness(t)
	ann := uvmWBAnnotation()

	// A full-line annotated write fills the (only) way and dirties it.
	write := annotatedWrite(0x1000, ann)
	if err := harness.topPort.Deliver(write); err != nil {
		t.Fatalf("deliver write: %v", err)
	}
	for i := 0; i < 200; i++ {
		harness.Engine.Run()
	}
	block := harness.directory.Lookup(1, 0x1000)
	if block == nil || !block.IsValid || !block.IsDirty {
		t.Fatal("the written block must be valid and dirty")
	}
	if block.Annotation != ann {
		t.Fatalf("the dirty block must retain the annotation, got %+v",
			block.Annotation)
	}

	// A read to a different line in the same set evicts the dirty block.
	read := mem.ReadReqBuilder{}.
		WithSrc(sim.RemotePort("UVMWBTestAgent.Port")).
		WithDst(sim.RemotePort("UVMWBCache.TopPort")).
		WithAddress(0x2000).
		WithByteSize(4).
		WithPID(1).
		Build()
	if err := harness.topPort.Deliver(read); err != nil {
		t.Fatalf("deliver read: %v", err)
	}

	// Drive until the eviction writeback reaches the bottom agent.
	var writeback *mem.WriteReq
	for i := 0; i < 500; i++ {
		harness.Engine.Run()
		for _, msg := range agent.received {
			if w, ok := msg.(*mem.WriteReq); ok {
				writeback = w
				break
			}
		}
		if writeback != nil {
			break
		}
	}
	if writeback == nil {
		t.Fatal("the dirty eviction must write back to the bottom")
	}

	if got := cache.ResolveAnnotation(writeback); got != ann {
		t.Fatalf("the replacement writeback must carry the stored annotation, got %+v",
			got)
	}
}
