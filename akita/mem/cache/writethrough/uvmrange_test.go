package writethrough

// sbin_codex: contract tests for the scoped UVM range writeback/invalidation
// of the writethrough cache (plan todo 13 of mgpusim-uvm-manager). Written
// first (RED), then made to pass (GREEN). Write-through caches degenerate to
// drain + invalidate: no dirty lines can be held.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// uvmRangeWTAgent receives the cache's bottom traffic and replies with data.
type uvmRangeWTAgent struct {
	*sim.TickingComponent
	port     sim.Port
	received []sim.Msg
}

func newUVMRangeWTAgent(engine sim.Engine) *uvmRangeWTAgent {
	agent := &uvmRangeWTAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMRangeWTAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMRangeWTAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmRangeWTAgent) Tick() bool {
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

// uvmRangeWTCtrlAgent captures the cache's control responses.
type uvmRangeWTCtrlAgent struct {
	*sim.TickingComponent
	port     sim.Port
	received []sim.Msg
}

func newUVMRangeWTCtrlAgent(engine sim.Engine) *uvmRangeWTCtrlAgent {
	agent := &uvmRangeWTCtrlAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMRangeWTCtrlAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMRangeWTCtrlAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmRangeWTCtrlAgent) Tick() bool {
	progress := false
	for {
		msg := agent.port.RetrieveIncoming()
		if msg == nil {
			break
		}
		agent.received = append(agent.received, msg)
		progress = true
	}
	return progress
}

func newUVMRangeWTHarness(t *testing.T) (*Comp, *uvmRangeWTAgent, *uvmRangeWTCtrlAgent) {
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
			Port: sim.RemotePort("UVMRangeWTAgent.Port"),
		}).
		Build("UVMRangeWTCache")

	agent := newUVMRangeWTAgent(engine)
	ctrl := newUVMRangeWTCtrlAgent(engine)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("UVMRangeWTConn")
	conn.PlugIn(cacheComp.topPort)
	conn.PlugIn(cacheComp.bottomPort)
	conn.PlugIn(cacheComp.controlPort)
	conn.PlugIn(agent.port)
	conn.PlugIn(ctrl.port)

	return cacheComp, agent, ctrl
}

func wtRangeBlock(c *Comp, tag uint64) *cache.Block {
	sets := c.directory.GetSets()
	setID := int((tag >> c.log2BlockSize) % uint64(len(sets)))
	for _, b := range sets[setID].Blocks {
		if !b.IsValid {
			return b
		}
	}
	return sets[setID].Blocks[0]
}

func populateWTBlock(
	c *Comp,
	tag uint64,
	pid vm.PID,
	ann *cache.VirtualAccessAnnotation,
) *cache.Block {
	block := wtRangeBlock(c, tag)
	block.Tag = tag
	block.PID = pid
	block.IsValid = true
	block.Annotation = ann
	return block
}

func wtRangeFlushReq(
	ctrl *uvmRangeWTCtrlAgent,
	cacheComp *Comp,
	op cache.UVMCacheRangeFlushOp,
	pid vm.PID,
	vaBase, mask uint64,
	runs []cache.PhysicalRun,
) *cache.UVMCacheRangeFlushReq {
	req := cache.UVMCacheRangeFlushReqBuilder{}.
		WithSrc(ctrl.port.AsRemote()).
		WithDst(cacheComp.controlPort.AsRemote()).
		WithOperation(op).
		WithPID(pid).
		WithVABase(vaBase).
		WithValidPageMask(mask).
		WithPhysicalRuns(runs).
		Build()
	return req
}

func wtRangeRsp(ctrl *uvmRangeWTCtrlAgent) *cache.UVMCacheRangeFlushRsp {
	for _, m := range ctrl.received {
		if r, ok := m.(*cache.UVMCacheRangeFlushRsp); ok {
			return r
		}
	}
	return nil
}

func wtDriveUntil(t *testing.T, engine sim.Engine, cond func() bool, max int) {
	t.Helper()
	for i := 0; i < max; i++ {
		engine.Run()
		if cond() {
			return
		}
	}
	t.Fatal("condition not met within the drive budget")
}

// TestUVMRangeWritebackOnly proves WRITEBACK_ONLY drains without invalidating
// matching lines in a write-through cache.
func TestUVMRangeWritebackOnly(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWTHarness(t)

	inBlock := populateWTBlock(cacheComp, 0x10040, 1, nil)
	outBlock := populateWTBlock(cacheComp, 0x20080, 1, nil)

	req := wtRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackOnly,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	wtDriveUntil(t, cacheComp.Engine, func() bool {
		return wtRangeRsp(ctrl) != nil
	}, 300)

	if !inBlock.IsValid {
		t.Fatal("WRITEBACK_ONLY must keep the in-range line valid")
	}
	if !outBlock.IsValid {
		t.Fatal("the unrelated line must stay valid")
	}
	if len(agent.received) != 0 {
		t.Fatal("a write-through cache must not write back during the range flush")
	}
}

// TestUVMRangeWritebackInvalidate proves WRITEBACK_INVALIDATE drains and
// invalidates matching lines in a write-through cache.
func TestUVMRangeWritebackInvalidate(t *testing.T) {
	cacheComp, _, ctrl := newUVMRangeWTHarness(t)

	inBlock := populateWTBlock(cacheComp, 0x10040, 1, nil)
	outBlock := populateWTBlock(cacheComp, 0x20080, 1, nil)

	req := wtRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	wtDriveUntil(t, cacheComp.Engine, func() bool {
		return wtRangeRsp(ctrl) != nil
	}, 300)

	if inBlock.IsValid {
		t.Fatal("WRITEBACK_INVALIDATE must invalidate the in-range line")
	}
	if !outBlock.IsValid {
		t.Fatal("the unrelated line must stay valid")
	}
}

// TestUVMRangeVirtualTag proves the virtual writethrough cache matches by
// PID+VA and validates the stored annotation PA.
func TestUVMRangeVirtualTag(t *testing.T) {
	cacheComp, _, ctrl := newUVMRangeWTHarness(t)
	cacheComp.uvmRangeVirtual = true

	ann := func(pid vm.PID, vapage, hbmPA uint64, generation uint64) *cache.VirtualAccessAnnotation {
		return &cache.VirtualAccessAnnotation{
			PID:        pid,
			VAPage:     vapage,
			HBMPA:      hbmPA,
			Location:   vm.MemoryLocationGPU_LOCAL,
			Generation: generation,
		}
	}

	matchBlock := populateWTBlock(cacheComp, 0x10040, 1, ann(1, 0x10000, 0x8000, 2))
	page1Block := populateWTBlock(cacheComp, 0x11000, 1, ann(1, 0x11000, 0x9000, 2))
	wrongPIDBlock := populateWTBlock(cacheComp, 0x100C0, 2, ann(2, 0x10000, 0x8000, 2))
	staleBlock := populateWTBlock(cacheComp, 0x10100, 1, ann(1, 0x10000, 0x7000, 1))

	req := wtRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x8000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	wtDriveUntil(t, cacheComp.Engine, func() bool {
		return wtRangeRsp(ctrl) != nil
	}, 300)

	if matchBlock.IsValid {
		t.Fatal("the matching virtual line must be invalidated")
	}
	if !page1Block.IsValid {
		t.Fatal("the line in the invalid page must stay valid")
	}
	if !wrongPIDBlock.IsValid {
		t.Fatal("the other-PID line must stay valid")
	}
	if !staleBlock.IsValid {
		t.Fatal("the line with a stale stored PA must stay valid")
	}
}
