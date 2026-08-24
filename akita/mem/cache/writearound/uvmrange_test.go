package writearound

// sbin_codex: contract tests for the scoped UVM range writeback/invalidation
// of the writearound cache (plan todo 13 of mgpusim-uvm-manager). Written
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

// uvmRangeWAAgent receives the cache's bottom traffic and replies with data.
// holdReads/holdWrites queue the corresponding responses until released.
type uvmRangeWAAgent struct {
	*sim.TickingComponent
	port          sim.Port
	received      []sim.Msg
	holdReads     bool
	holdWrites    bool
	pendingReads  []*mem.ReadReq
	pendingWrites []*mem.WriteReq
}

func newUVMRangeWAAgent(engine sim.Engine) *uvmRangeWAAgent {
	agent := &uvmRangeWAAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMRangeWAAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMRangeWAAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmRangeWAAgent) Tick() bool {
	progress := false
	for {
		msg := agent.port.RetrieveIncoming()
		if msg == nil {
			break
		}
		agent.received = append(agent.received, msg)
		switch req := msg.(type) {
		case *mem.ReadReq:
			if agent.holdReads {
				agent.pendingReads = append(agent.pendingReads, req)
				continue
			}
			data := make([]byte, req.AccessByteSize)
			for i := range data {
				data[i] = byte(i + 1)
			}
			if agent.port.CanSend() {
				_ = agent.port.Send(req.GenerateRsp(data))
			}
		case *mem.WriteReq:
			if agent.holdWrites {
				agent.pendingWrites = append(agent.pendingWrites, req)
				continue
			}
			if agent.port.CanSend() {
				_ = agent.port.Send(req.GenerateRsp())
			}
		}
		progress = true
	}
	if !agent.holdReads {
		for len(agent.pendingReads) > 0 && agent.port.CanSend() {
			req := agent.pendingReads[0]
			data := make([]byte, req.AccessByteSize)
			for i := range data {
				data[i] = byte(i + 1)
			}
			_ = agent.port.Send(req.GenerateRsp(data))
			agent.pendingReads = agent.pendingReads[1:]
			progress = true
		}
	}
	if !agent.holdWrites {
		for len(agent.pendingWrites) > 0 && agent.port.CanSend() {
			req := agent.pendingWrites[0]
			_ = agent.port.Send(req.GenerateRsp())
			agent.pendingWrites = agent.pendingWrites[1:]
			progress = true
		}
	}
	return progress
}

// uvmRangeWACtrlAgent captures the cache's control responses.
type uvmRangeWACtrlAgent struct {
	*sim.TickingComponent
	port     sim.Port
	received []sim.Msg
}

func newUVMRangeWACtrlAgent(engine sim.Engine) *uvmRangeWACtrlAgent {
	agent := &uvmRangeWACtrlAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMRangeWACtrlAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMRangeWACtrlAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmRangeWACtrlAgent) Tick() bool {
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

func newUVMRangeWAHarness(t *testing.T) (*Comp, *uvmRangeWAAgent, *uvmRangeWACtrlAgent) {
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
			Port: sim.RemotePort("UVMRangeWAAgent.Port"),
		}).
		Build("UVMRangeWACache")

	agent := newUVMRangeWAAgent(engine)
	ctrl := newUVMRangeWACtrlAgent(engine)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("UVMRangeWAConn")
	conn.PlugIn(cacheComp.topPort)
	conn.PlugIn(cacheComp.bottomPort)
	conn.PlugIn(cacheComp.controlPort)
	conn.PlugIn(agent.port)
	conn.PlugIn(ctrl.port)

	return cacheComp, agent, ctrl
}

func waRangeBlock(c *Comp, tag uint64) *cache.Block {
	sets := c.directory.GetSets()
	setID := int((tag >> c.log2BlockSize) % uint64(len(sets)))
	for _, b := range sets[setID].Blocks {
		if !b.IsValid {
			return b
		}
	}
	return sets[setID].Blocks[0]
}

func populateWABlock(
	c *Comp,
	tag uint64,
	pid vm.PID,
	ann *cache.VirtualAccessAnnotation,
) *cache.Block {
	block := waRangeBlock(c, tag)
	block.Tag = tag
	block.PID = pid
	block.IsValid = true
	block.Annotation = ann
	return block
}

func waRangeFlushReq(
	ctrl *uvmRangeWACtrlAgent,
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

func waRangeRsp(ctrl *uvmRangeWACtrlAgent) *cache.UVMCacheRangeFlushRsp {
	for _, m := range ctrl.received {
		if r, ok := m.(*cache.UVMCacheRangeFlushRsp); ok {
			return r
		}
	}
	return nil
}

func waWriteReqs(agent *uvmRangeWAAgent) []*mem.WriteReq {
	var out []*mem.WriteReq
	for _, m := range agent.received {
		if w, ok := m.(*mem.WriteReq); ok {
			out = append(out, w)
		}
	}
	return out
}

// releaseHeld delivers the agent's pending responses to the cache, releasing
// the hold without relying on the engine rescheduling the idle agent.
func releaseHeld(agent *uvmRangeWAAgent, cacheComp *Comp) {
	for _, req := range agent.pendingReads {
		data := make([]byte, req.AccessByteSize)
		for i := range data {
			data[i] = byte(i + 1)
		}
		_ = cacheComp.bottomPort.Deliver(req.GenerateRsp(data))
	}
	for _, req := range agent.pendingWrites {
		_ = cacheComp.bottomPort.Deliver(req.GenerateRsp())
	}
	agent.pendingReads = nil
	agent.pendingWrites = nil
	agent.holdReads = false
	agent.holdWrites = false
}

func waDriveUntil(t *testing.T, engine sim.Engine, cond func() bool, max int) {
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
	cacheComp, agent, ctrl := newUVMRangeWAHarness(t)

	inBlock := populateWABlock(cacheComp, 0x10040, 1, nil)
	outBlock := populateWABlock(cacheComp, 0x20080, 1, nil)

	req := waRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackOnly,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	waDriveUntil(t, cacheComp.Engine, func() bool {
		return waRangeRsp(ctrl) != nil
	}, 300)

	if !inBlock.IsValid {
		t.Fatal("WRITEBACK_ONLY must keep the in-range line valid")
	}
	if !outBlock.IsValid {
		t.Fatal("the unrelated line must stay valid")
	}
	if len(waWriteReqs(agent)) != 0 {
		t.Fatal("a write-through cache must not write back during the range flush")
	}
}

// TestUVMRangeWritebackInvalidate proves WRITEBACK_INVALIDATE drains and
// invalidates matching lines in a write-through cache.
func TestUVMRangeWritebackInvalidate(t *testing.T) {
	cacheComp, _, ctrl := newUVMRangeWAHarness(t)

	inBlock := populateWABlock(cacheComp, 0x10040, 1, nil)
	outBlock := populateWABlock(cacheComp, 0x20080, 1, nil)

	req := waRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	waDriveUntil(t, cacheComp.Engine, func() bool {
		return waRangeRsp(ctrl) != nil
	}, 300)

	if inBlock.IsValid {
		t.Fatal("WRITEBACK_INVALIDATE must invalidate the in-range line")
	}
	if !outBlock.IsValid {
		t.Fatal("the unrelated line must stay valid")
	}
}

// TestUVMRangeVirtualTag proves the virtual writearound cache matches by
// PID+VA and validates the stored annotation PA.
func TestUVMRangeVirtualTag(t *testing.T) {
	cacheComp, _, ctrl := newUVMRangeWAHarness(t)
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

	matchBlock := populateWABlock(cacheComp, 0x10040, 1, ann(1, 0x10000, 0x8000, 2))
	page1Block := populateWABlock(cacheComp, 0x11000, 1, ann(1, 0x11000, 0x9000, 2))
	wrongPIDBlock := populateWABlock(cacheComp, 0x100C0, 2, ann(2, 0x10000, 0x8000, 2))
	staleBlock := populateWABlock(cacheComp, 0x10100, 1, ann(1, 0x10000, 0x7000, 1))

	req := waRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x8000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	waDriveUntil(t, cacheComp.Engine, func() bool {
		return waRangeRsp(ctrl) != nil
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

// TestUVMRangeInflightDrain proves the flush waits for matching in-flight
// cache/MSHR transactions before invalidating.
func TestUVMRangeInflightDrain(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWAHarness(t)

	read := mem.ReadReqBuilder{}.
		WithSrc(ctrl.port.AsRemote()).
		WithDst(cacheComp.topPort.AsRemote()).
		WithAddress(0x10004).
		WithByteSize(4).
		WithPID(1).
		Build()
	if err := cacheComp.topPort.Deliver(read); err != nil {
		t.Fatalf("deliver read: %v", err)
	}
	agent.holdReads = true
	waDriveUntil(t, cacheComp.Engine, func() bool {
		return cacheComp.mshr.Query(1, 0x10000) != nil
	}, 200)

	req := waRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	for i := 0; i < 50; i++ {
		cacheComp.Engine.Run()
	}
	if waRangeRsp(ctrl) != nil {
		t.Fatal("the flush must not ack while a matching refill is pending")
	}

	releaseHeld(agent, cacheComp)
	waDriveUntil(t, cacheComp.Engine, func() bool {
		return waRangeRsp(ctrl) != nil
	}, 500)

	if cacheComp.directory.Lookup(1, 0x10000) != nil {
		t.Fatal("the drained matching line must be invalidated")
	}
}

// TestUVMRangeUnrelatedProgress proves unrelated traffic completes while the
// range flush is still draining.
func TestUVMRangeUnrelatedProgress(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWAHarness(t)

	write := mem.WriteReqBuilder{}.
		WithSrc(ctrl.port.AsRemote()).
		WithDst(cacheComp.topPort.AsRemote()).
		WithAddress(0x10004).
		WithData([]byte{1, 2, 3, 4}).
		WithPID(1).
		Build()
	if err := cacheComp.topPort.Deliver(write); err != nil {
		t.Fatalf("deliver matching write: %v", err)
	}
	agent.holdWrites = true
	waDriveUntil(t, cacheComp.Engine, func() bool {
		return len(waWriteReqs(agent)) > 0
	}, 200)

	req := waRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	read := mem.ReadReqBuilder{}.
		WithSrc(ctrl.port.AsRemote()).
		WithDst(cacheComp.topPort.AsRemote()).
		WithAddress(0x20084).
		WithByteSize(4).
		WithPID(1).
		Build()
	if err := cacheComp.topPort.Deliver(read); err != nil {
		t.Fatalf("deliver unrelated read: %v", err)
	}

	waDriveUntil(t, cacheComp.Engine, func() bool {
		for _, m := range ctrl.received {
			if _, ok := m.(*mem.DataReadyRsp); ok {
				return true
			}
		}
		return false
	}, 300)

	if waRangeRsp(ctrl) != nil {
		t.Fatal("the unrelated read must complete while the flush is still active")
	}

	releaseHeld(agent, cacheComp)
	waDriveUntil(t, cacheComp.Engine, func() bool {
		return waRangeRsp(ctrl) != nil
	}, 500)
}

// TestUVMRangeRejectMalformed proves a malformed command is acked without any
// cache mutation.
func TestUVMRangeRejectMalformed(t *testing.T) {
	cacheComp, _, ctrl := newUVMRangeWAHarness(t)

	block := populateWABlock(cacheComp, 0x10040, 1, nil)

	req := waRangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10004, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	waDriveUntil(t, cacheComp.Engine, func() bool {
		return waRangeRsp(ctrl) != nil
	}, 300)

	if !block.IsValid {
		t.Fatal("a malformed command must not mutate the cache")
	}
}
