package writeback

// sbin_codex: contract tests for the scoped UVM range writeback/invalidation
// of the writeback cache (plan todo 13 of mgpusim-uvm-manager). Written first
// (RED), then made to pass (GREEN) by implementing uvmrangeflusher.go.

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// uvmRangeWBAgent receives the cache's bottom traffic and replies with data.
// holdReads/holdWrites queue the corresponding responses until released.
type uvmRangeWBAgent struct {
	*sim.TickingComponent
	port          sim.Port
	received      []sim.Msg
	holdReads     bool
	holdWrites    bool
	pendingReads  []*mem.ReadReq
	pendingWrites []*mem.WriteReq
}

func newUVMRangeWBAgent(engine sim.Engine) *uvmRangeWBAgent {
	agent := &uvmRangeWBAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMRangeWBAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMRangeWBAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmRangeWBAgent) Tick() bool {
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

// uvmRangeCtrlAgent captures the cache's control responses.
type uvmRangeCtrlAgent struct {
	*sim.TickingComponent
	port     sim.Port
	received []sim.Msg
}

func newUVMRangeCtrlAgent(engine sim.Engine) *uvmRangeCtrlAgent {
	agent := &uvmRangeCtrlAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"UVMRangeCtrlAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "UVMRangeCtrlAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *uvmRangeCtrlAgent) Tick() bool {
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

func newUVMRangeWBHarness(t *testing.T) (*Comp, *uvmRangeWBAgent, *uvmRangeCtrlAgent) {
	t.Helper()
	engine := sim.NewSerialEngine()
	cacheComp := MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		WithLog2BlockSize(6).
		WithWayAssociativity(4).
		WithNumMSHREntry(16).
		WithNumReqPerCycle(8).
		WithByteSize(4 * mem.KB).
		WithBankLatency(1).
		WithDirectoryLatency(1).
		WithAddressToPortMapper(&mem.SinglePortMapper{
			Port: sim.RemotePort("UVMRangeWBAgent.Port"),
		}).
		Build("UVMRangeWBCache")

	agent := newUVMRangeWBAgent(engine)
	ctrl := newUVMRangeCtrlAgent(engine)

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("UVMRangeWBConn")
	conn.PlugIn(cacheComp.topPort)
	conn.PlugIn(cacheComp.bottomPort)
	conn.PlugIn(cacheComp.controlPort)
	conn.PlugIn(agent.port)
	conn.PlugIn(ctrl.port)

	return cacheComp, agent, ctrl
}

// wbRangeBlock returns a free block of the set that the tag maps to.
func wbRangeBlock(c *Comp, tag uint64) *cache.Block {
	sets := c.directory.GetSets()
	setID := int((tag >> c.log2BlockSize) % uint64(len(sets)))
	for _, b := range sets[setID].Blocks {
		if !b.IsValid {
			return b
		}
	}
	return sets[setID].Blocks[0]
}

func populateWBBlock(
	c *Comp,
	tag uint64,
	pid vm.PID,
	dirty bool,
	ann *cache.VirtualAccessAnnotation,
	data []byte,
	mask []bool,
) *cache.Block {
	block := wbRangeBlock(c, tag)
	block.Tag = tag
	block.PID = pid
	block.IsValid = true
	block.IsDirty = dirty
	block.Annotation = ann
	block.DirtyMask = mask
	if err := c.storage.Write(block.CacheAddress, data); err != nil {
		panic(err)
	}
	return block
}

func fullDirtyMask() []bool {
	mask := make([]bool, 64)
	for i := range mask {
		mask[i] = true
	}
	return mask
}

func rangeFlushReq(
	ctrl *uvmRangeCtrlAgent,
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

func wbWriteReqs(agent *uvmRangeWBAgent) []*mem.WriteReq {
	var out []*mem.WriteReq
	for _, m := range agent.received {
		if w, ok := m.(*mem.WriteReq); ok {
			out = append(out, w)
		}
	}
	return out
}

func wbRangeRsp(ctrl *uvmRangeCtrlAgent) *cache.UVMCacheRangeFlushRsp {
	for _, m := range ctrl.received {
		if r, ok := m.(*cache.UVMCacheRangeFlushRsp); ok {
			return r
		}
	}
	return nil
}

// releaseHeld delivers the agent's pending responses to the cache, releasing
// the hold without relying on the engine rescheduling the idle agent.
func releaseHeld(agent *uvmRangeWBAgent, cacheComp *Comp) {
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

func driveUntil(t *testing.T, engine sim.Engine, cond func() bool, max int) {
	t.Helper()
	for i := 0; i < max; i++ {
		engine.Run()
		if cond() {
			return
		}
	}
	t.Fatal("condition not met within the drive budget")
}

// TestUVMRangeWritebackOnly proves WRITEBACK_ONLY writes dirty matching lines
// to their stored PAs and leaves them valid but clean; unrelated lines are
// untouched.
func TestUVMRangeWritebackOnly(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0xAA}, 64)
	dataB := bytes.Repeat([]byte{0xBB}, 64)
	dataOut := bytes.Repeat([]byte{0xCC}, 64)
	populateWBBlock(cacheComp, 0x10000, 1, true, nil, dataA, fullDirtyMask())
	populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataB, fullDirtyMask())
	outBlock := populateWBBlock(cacheComp, 0x20080, 1, true, nil, dataOut, fullDirtyMask())

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackOnly,
		1, 0x10000, 0b11, []cache.PhysicalRun{{Start: 0x10000, Length: 0x2000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	writebacks := wbWriteReqs(agent)
	if len(writebacks) != 2 {
		t.Fatalf("expected 2 writebacks, got %d", len(writebacks))
	}
	byAddr := map[uint64]*mem.WriteReq{}
	for _, w := range writebacks {
		byAddr[w.Address] = w
	}
	if w, ok := byAddr[0x10000]; !ok || !bytes.Equal(w.Data, dataA) {
		t.Fatalf("writeback to 0x10000 missing or wrong data: %+v", w)
	}
	if w, ok := byAddr[0x10040]; !ok || !bytes.Equal(w.Data, dataB) {
		t.Fatalf("writeback to 0x10040 missing or wrong data: %+v", w)
	}

	inA := cacheComp.directory.Lookup(1, 0x10000)
	inB := cacheComp.directory.Lookup(1, 0x10040)
	if inA == nil || !inA.IsValid || inA.IsDirty {
		t.Fatalf("WRITEBACK_ONLY must keep the in-range line valid and clean: %+v", inA)
	}
	if inB == nil || !inB.IsValid || inB.IsDirty {
		t.Fatalf("WRITEBACK_ONLY must keep the in-range line valid and clean: %+v", inB)
	}
	if !outBlock.IsValid || !outBlock.IsDirty {
		t.Fatal("the unrelated dirty line must stay dirty and valid")
	}
}

// TestUVMRangeWritebackInvalidate proves WRITEBACK_INVALIDATE writes dirty
// matching lines to their stored PAs and invalidates them.
func TestUVMRangeWritebackInvalidate(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0xAA}, 64)
	dataB := bytes.Repeat([]byte{0xBB}, 64)
	dataOut := bytes.Repeat([]byte{0xCC}, 64)
	populateWBBlock(cacheComp, 0x10000, 1, true, nil, dataA, fullDirtyMask())
	populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataB, fullDirtyMask())
	outBlock := populateWBBlock(cacheComp, 0x20080, 1, true, nil, dataOut, fullDirtyMask())

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b11, []cache.PhysicalRun{{Start: 0x10000, Length: 0x2000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	writebacks := wbWriteReqs(agent)
	if len(writebacks) != 2 {
		t.Fatalf("expected 2 writebacks, got %d", len(writebacks))
	}
	byAddr := map[uint64]*mem.WriteReq{}
	for _, w := range writebacks {
		byAddr[w.Address] = w
	}
	if w, ok := byAddr[0x10000]; !ok || !bytes.Equal(w.Data, dataA) {
		t.Fatalf("writeback to 0x10000 missing or wrong data: %+v", w)
	}
	if w, ok := byAddr[0x10040]; !ok || !bytes.Equal(w.Data, dataB) {
		t.Fatalf("writeback to 0x10040 missing or wrong data: %+v", w)
	}

	if cacheComp.directory.Lookup(1, 0x10000) != nil {
		t.Fatal("WRITEBACK_INVALIDATE must invalidate the in-range line")
	}
	if cacheComp.directory.Lookup(1, 0x10040) != nil {
		t.Fatal("WRITEBACK_INVALIDATE must invalidate the in-range line")
	}
	if !outBlock.IsValid || !outBlock.IsDirty {
		t.Fatal("the unrelated dirty line must stay dirty and valid")
	}
}

// TestUVMRangeDirty proves only the dirty bytes of a partially dirty line are
// written back, with the exact mask and data.
func TestUVMRangeDirty(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	data := make([]byte, 64)
	for i := range data {
		data[i] = 0xEE
	}
	mask := make([]bool, 64)
	for i := 0; i < 32; i++ {
		data[i] = byte(i)
		mask[i] = true
	}
	populateWBBlock(cacheComp, 0x10000, 1, true, nil, data, mask)

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	writebacks := wbWriteReqs(agent)
	if len(writebacks) != 1 {
		t.Fatalf("expected 1 writeback, got %d", len(writebacks))
	}
	w := writebacks[0]
	if w.Address != 0x10000 {
		t.Fatalf("writeback address = %#x, want 0x10000", w.Address)
	}
	if !bytes.Equal(w.Data, data) {
		t.Fatal("the writeback must carry the exact line data")
	}
	if w.DirtyMask == nil || len(w.DirtyMask) != 64 {
		t.Fatalf("the writeback must carry the exact dirty mask, got %v", w.DirtyMask)
	}
	for i := 0; i < 64; i++ {
		if w.DirtyMask[i] != mask[i] {
			t.Fatalf("dirty mask byte %d = %v, want %v", i, w.DirtyMask[i], mask[i])
		}
	}
}

// TestUVMRangeFragmentedPFN proves fragmented physical runs each produce a
// writeback to the exact stored PA.
func TestUVMRangeFragmentedPFN(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0x11}, 64)
	dataB := bytes.Repeat([]byte{0x22}, 64)
	populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataA, fullDirtyMask())
	populateWBBlock(cacheComp, 0x20040, 1, true, nil, dataB, fullDirtyMask())

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b11, []cache.PhysicalRun{
			{Start: 0x10000, Length: 0x1000},
			{Start: 0x20000, Length: 0x1000},
		})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	writebacks := wbWriteReqs(agent)
	if len(writebacks) != 2 {
		t.Fatalf("expected 2 writebacks, got %d", len(writebacks))
	}
	byAddr := map[uint64]*mem.WriteReq{}
	for _, w := range writebacks {
		byAddr[w.Address] = w
	}
	if w, ok := byAddr[0x10040]; !ok || !bytes.Equal(w.Data, dataA) {
		t.Fatalf("writeback to 0x10040 missing or wrong data: %+v", w)
	}
	if w, ok := byAddr[0x20040]; !ok || !bytes.Equal(w.Data, dataB) {
		t.Fatalf("writeback to 0x20040 missing or wrong data: %+v", w)
	}
}

// TestUVMRangePartialMask proves lines in pages outside the valid-page mask
// are not flushed.
func TestUVMRangePartialMask(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0x33}, 64)
	dataGap := bytes.Repeat([]byte{0x44}, 64)
	dataC := bytes.Repeat([]byte{0x55}, 64)
	populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataA, fullDirtyMask())
	gapBlock := populateWBBlock(cacheComp, 0x11040, 1, true, nil, dataGap, fullDirtyMask())
	populateWBBlock(cacheComp, 0x12040, 1, true, nil, dataC, fullDirtyMask())

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b0101, []cache.PhysicalRun{
			{Start: 0x10000, Length: 0x1000},
			{Start: 0x12000, Length: 0x1000},
		})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	writebacks := wbWriteReqs(agent)
	if len(writebacks) != 2 {
		t.Fatalf("expected 2 writebacks, got %d", len(writebacks))
	}
	byAddr := map[uint64]*mem.WriteReq{}
	for _, w := range writebacks {
		byAddr[w.Address] = w
	}
	if w, ok := byAddr[0x10040]; !ok || !bytes.Equal(w.Data, dataA) {
		t.Fatalf("writeback to 0x10040 missing or wrong data: %+v", w)
	}
	if w, ok := byAddr[0x12040]; !ok || !bytes.Equal(w.Data, dataC) {
		t.Fatalf("writeback to 0x12040 missing or wrong data: %+v", w)
	}
	if !gapBlock.IsValid || !gapBlock.IsDirty {
		t.Fatal("the line in the invalid page must stay dirty and valid")
	}
}

// TestUVMRangeVirtualTag proves the virtual cache matches by PID+VA, writes
// back to the stored annotation HBM PA, and validates the stored PA.
func TestUVMRangeVirtualTag(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)
	cacheComp.uvmRangeVirtual = true

	ann := func(hbmPA uint64, generation uint64) *cache.VirtualAccessAnnotation {
		return &cache.VirtualAccessAnnotation{
			PID:        vm.PID(1),
			VAPage:     0x10000,
			HBMPA:      hbmPA,
			Location:   vm.MemoryLocationGPU_LOCAL,
			Generation: generation,
		}
	}

	dataA := bytes.Repeat([]byte{0x66}, 64)
	dataB := bytes.Repeat([]byte{0x77}, 64)
	dataC := bytes.Repeat([]byte{0x88}, 64)
	dataD := bytes.Repeat([]byte{0x99}, 64)
	dataE := bytes.Repeat([]byte{0xAA}, 64)
	populateWBBlock(cacheComp, 0x10040, 1, true, ann(0x8000, 2), dataA, fullDirtyMask())
	populateWBBlock(cacheComp, 0x10080, 1, true, ann(0x8000, 2), dataB, fullDirtyMask())
	page1Block := populateWBBlock(cacheComp, 0x11000, 1, true,
		&cache.VirtualAccessAnnotation{PID: 1, VAPage: 0x11000, HBMPA: 0x9000,
			Location: vm.MemoryLocationGPU_LOCAL, Generation: 2}, dataC, fullDirtyMask())
	wrongPIDBlock := populateWBBlock(cacheComp, 0x100C0, 2, true,
		&cache.VirtualAccessAnnotation{PID: 2, VAPage: 0x10000, HBMPA: 0x8000,
			Location: vm.MemoryLocationGPU_LOCAL, Generation: 2}, dataD, fullDirtyMask())
	staleBlock := populateWBBlock(cacheComp, 0x10100, 1, true, ann(0x7000, 1), dataE, fullDirtyMask())

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x8000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	writebacks := wbWriteReqs(agent)
	if len(writebacks) != 2 {
		t.Fatalf("expected 2 writebacks, got %d", len(writebacks))
	}
	byAddr := map[uint64]*mem.WriteReq{}
	for _, w := range writebacks {
		byAddr[w.Address] = w
	}
	if w, ok := byAddr[0x8040]; !ok || !bytes.Equal(w.Data, dataA) {
		t.Fatalf("writeback to stored PA 0x8040 missing or wrong data: %+v", w)
	}
	if w, ok := byAddr[0x8080]; !ok || !bytes.Equal(w.Data, dataB) {
		t.Fatalf("writeback to stored PA 0x8080 missing or wrong data: %+v", w)
	}

	if cacheComp.directory.Lookup(1, 0x10040) != nil {
		t.Fatal("the matching virtual line must be invalidated")
	}
	if cacheComp.directory.Lookup(1, 0x10080) != nil {
		t.Fatal("the matching virtual line must be invalidated")
	}
	if !page1Block.IsValid || !page1Block.IsDirty {
		t.Fatal("the line in the invalid page must stay valid and dirty")
	}
	if !wrongPIDBlock.IsValid || !wrongPIDBlock.IsDirty {
		t.Fatal("the other-PID line must stay valid and dirty")
	}
	if !staleBlock.IsValid || !staleBlock.IsDirty {
		t.Fatal("the line with a stale stored PA must stay valid and dirty")
	}
}

// TestUVMRangeInflightDrain proves the flush waits for matching in-flight
// cache/MSHR transactions before invalidating.
func TestUVMRangeInflightDrain(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

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
	driveUntil(t, cacheComp.Engine, func() bool {
		return cacheComp.mshr.Query(1, 0x10000) != nil
	}, 200)

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	for i := 0; i < 50; i++ {
		cacheComp.Engine.Run()
	}
	if wbRangeRsp(ctrl) != nil {
		t.Fatal("the flush must not ack while a matching refill is pending")
	}

	releaseHeld(agent, cacheComp)
	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	if cacheComp.directory.Lookup(1, 0x10000) != nil {
		t.Fatal("the drained matching line must be invalidated")
	}
}

// TestUVMRangeUnrelatedProgress proves unrelated traffic completes while the
// range flush is still active.
func TestUVMRangeUnrelatedProgress(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0x11}, 64)
	dataU := bytes.Repeat([]byte{0x22}, 64)
	populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataA, fullDirtyMask())
	populateWBBlock(cacheComp, 0x20080, 1, false, nil, dataU, nil)

	agent.holdWrites = true
	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return len(wbWriteReqs(agent)) > 0
	}, 300)

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

	driveUntil(t, cacheComp.Engine, func() bool {
		for _, m := range ctrl.received {
			if _, ok := m.(*mem.DataReadyRsp); ok {
				return true
			}
		}
		return false
	}, 300)

	if wbRangeRsp(ctrl) != nil {
		t.Fatal("the unrelated read must complete while the flush is still active")
	}

	releaseHeld(agent, cacheComp)
	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	if cacheComp.directory.Lookup(1, 0x10040) != nil {
		t.Fatal("the matching line must be invalidated after the flush")
	}
}

// TestUVMRangeBackpressure proves the flush waits for held writeback
// completion before acking.
func TestUVMRangeBackpressure(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0x33}, 64)
	populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataA, fullDirtyMask())

	agent.holdWrites = true
	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10000, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return len(wbWriteReqs(agent)) > 0
	}, 300)

	for i := 0; i < 50; i++ {
		cacheComp.Engine.Run()
	}
	if wbRangeRsp(ctrl) != nil {
		t.Fatal("the flush must not ack while the writeback is held")
	}

	releaseHeld(agent, cacheComp)
	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 500)

	if cacheComp.directory.Lookup(1, 0x10040) != nil {
		t.Fatal("the matching line must be invalidated after the flush")
	}
}

// TestUVMRangeRejectMalformed proves a malformed command is acked without any
// cache mutation.
func TestUVMRangeRejectMalformed(t *testing.T) {
	cacheComp, agent, ctrl := newUVMRangeWBHarness(t)

	dataA := bytes.Repeat([]byte{0x44}, 64)
	block := populateWBBlock(cacheComp, 0x10040, 1, true, nil, dataA, fullDirtyMask())

	req := rangeFlushReq(ctrl, cacheComp, cache.UVMCacheRangeFlushWritebackInvalidate,
		1, 0x10004, 0b1, []cache.PhysicalRun{{Start: 0x10000, Length: 0x1000}})
	if err := cacheComp.controlPort.Deliver(req); err != nil {
		t.Fatalf("deliver range flush: %v", err)
	}

	driveUntil(t, cacheComp.Engine, func() bool {
		return wbRangeRsp(ctrl) != nil
	}, 300)

	if len(wbWriteReqs(agent)) != 0 {
		t.Fatal("a malformed command must not trigger any writeback")
	}
	if !block.IsValid || !block.IsDirty {
		t.Fatal("a malformed command must not mutate the cache")
	}
}
