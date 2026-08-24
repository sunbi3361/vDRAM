package r9nano

// sbin_codex: virtual-caching UVM access-gate contract tests (plan todo 10 of
// mgpusim-uvm-manager). Prove the virtual L1V/L1S gates are wired before cache
// admission (probe wiring), the GPU_LOCAL/CPU_REMOTE flows annotate and route
// correctly through the L2, and the ROB->gate and remote-read barrier races
// satisfy the Todo 8 ack invariant.

import (
	"fmt"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/shaderarray"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rob"
)

// virtualUVMAgent sends pending messages and records received ones without
// Gomega (plain Go tests cannot use the Ginkgo l2FlowAgent helper). // sbin_codex
type virtualUVMAgent struct {
	*sim.TickingComponent
	port     sim.Port
	pending  []sim.Msg
	received []sim.Msg
}

func newVirtualUVMAgent(engine sim.Engine) *virtualUVMAgent {
	agent := &virtualUVMAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"VirtualUVMTestAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "VirtualUVMTestAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *virtualUVMAgent) Tick() bool {
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

func (agent *virtualUVMAgent) enqueue(msg sim.Msg) {
	agent.pending = append(agent.pending, msg)
	agent.TickLater()
}

// virtualFlowCapture captures the reads/writes that cross a port.
type virtualFlowCapture struct {
	reads        []*mem.ReadReq
	writes       []*mem.WriteReq
	translations []*vm.TranslationReq
	acks         []sim.Msg
}

func (c *virtualFlowCapture) Func(ctx sim.HookCtx) {
	// Count only outgoing sends so a message is not double-counted when the
	// connection retrieves it from the port buffer.
	if ctx.Pos != sim.HookPosPortMsgSend {
		return
	}
	switch msg := ctx.Item.(type) {
	case *mem.ReadReq:
		c.reads = append(c.reads, msg)
	case *mem.WriteReq:
		c.writes = append(c.writes, msg)
	case *vm.TranslationReq:
		c.translations = append(c.translations, msg)
	case *vm.BlockAck, *vm.UnblockAck:
		c.acks = append(c.acks, msg.(sim.Msg))
	}
}

// connectionNameOf returns the direct-connection name that a port is already
// plugged into, by probing a fresh connection and recovering the panic.
func connectionNameOf(t *testing.T, port sim.Port) (name string) {
	t.Helper()
	probe := directconnection.MakeBuilder().
		WithEngine(nil).
		WithFreq(1 * sim.GHz).
		Build("ProbeConnection")
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			const prefix = "connection already set to "
			const suffix = ", now connecting to "
			if len(msg) > len(prefix)+len(suffix) &&
				msg[:len(prefix)] == prefix {
				name = msg[len(prefix):]
				if i := indexOf(msg, suffix); i >= 0 {
					name = msg[len(prefix):i]
				}
				return
			}
			t.Fatalf("unexpected probe panic: %v", r)
		}
		t.Fatalf("port %s must already be connected", port.Name())
	}()
	probe.PlugIn(port)
	return
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// expectPortOnConnection asserts the port is plugged into the named
// connection, mirroring the Ginkgo helper for plain Go tests. // sbin_codex
func expectPortOnConnection(t *testing.T, port sim.Port, connectionName string) {
	t.Helper()
	if got := connectionNameOf(t, port); got != connectionName {
		t.Fatalf("port %s must be on %s, got %s", port.Name(),
			connectionName, got)
	}
}

func buildVirtualGPU(
	t *testing.T,
	name string,
	pageTable vm.PageTable,
	globalStorage *mem.Storage,
) (*simulation.Simulation, *sim.Domain) {
	t.Helper()
	testSimulation, gpuPageTable, cpuMMU := newPlainR9NanoSimulation(t, name)
	if pageTable != nil {
		gpuPageTable = pageTable
	}
	domain := MakeBuilder().
		WithSimulation(testSimulation).
		WithNumShaderArray(1).
		WithNumCUPerShaderArray(1).
		WithNumMemoryBank(2).
		WithL2CacheSize(32 * mem.KB).
		WithDramSize(2 * mem.GB).
		WithGlobalStorage(globalStorage).
		WithMMU(cpuMMU).
		WithDataPathTopology(NewVirtualDataPathTopology()).
		WithMemoryTopology(NewVirtualMemoryTopology()).
		WithGPUID(1).
		WithPageTable(gpuPageTable).
		WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
			BankSize:   2 * mem.GB,
			LowModules: []sim.RemotePort{"CPU"},
		}).
		Build("GPU")
	return testSimulation, domain
}

// TestVirtualUVMProbeWiring proves the virtual data path builds the L1V/L1S
// gates before cache admission: each gate carries a deterministic gate ID, the
// ROB targets the gate, the gate targets the cache, the control ports are
// exposed and reachable by the CP, and the translation ports probe the shared
// L2 TLB.
func TestVirtualUVMProbeWiring(t *testing.T) {
	testSimulation, _ := buildVirtualGPU(t, "virtual-probe", nil, mem.NewStorage(4*mem.GB))

	gate := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VGate[0]").(*shaderarray.VirtualAccessGate)
	if want := shaderarray.VirtualAccessGateIDBase; gate.GetUVMGateID() != want {
		t.Fatalf("L1V[0] must be wired as gate %d, got %d", want, gate.GetUVMGateID())
	}

	l1sGate := testSimulation.GetComponentByName(
		"GPU.SA[0].L1SGate").(*shaderarray.VirtualAccessGate)
	if want := shaderarray.VirtualAccessGateIDBase + 1; l1sGate.GetUVMGateID() != want {
		t.Fatalf("L1S must be wired as gate %d, got %d", want, l1sGate.GetUVMGateID())
	}

	// ROB -> gate -> cache wiring.
	robComp := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VROB[0]").(*rob.ReorderBuffer)
	if robComp.BottomUnit != gate.GetPortByName("Top").AsRemote() {
		t.Fatalf("the ROB must target the gate top, got %v", robComp.BottomUnit)
	}
	cacheTop := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VCache[0]").GetPortByName("Top")
	if connectionNameOf(t, gate.GetPortByName("Bottom")) !=
		connectionNameOf(t, cacheTop) {
		t.Fatal("the gate bottom and the cache top must share a connection")
	}

	// Gate control ports reachable by the CP through the internal connection,
	// and probe ports plugged into the shared L2 TLB connection.
	expectPortOnConnection(t, gate.GetPortByName("Control"), "GPU.InternalConn")
	expectPortOnConnection(t, l1sGate.GetPortByName("Control"), "GPU.InternalConn")
	expectPortOnConnection(
		t, gate.GetPortByName("Translation"), "GPU.TranslationToL2TLB")
	expectPortOnConnection(
		t, l1sGate.GetPortByName("Translation"), "GPU.TranslationToL2TLB")
}

// TestL2VirtualUVMFlow proves a GPU_LOCAL request is annotated with
// (PID, VA page, HBM PA, location, generation) and forwarded to the cache with
// the VA retained, the annotation persists through the L1V refill to the L2,
// and the data returns from the HBM PA. A CPU_REMOTE read never enters the
// cache: the CPU PA is routed only to the endpoint.
func TestL2VirtualUVMFlow(t *testing.T) {
	globalStorage := mem.NewStorage(4 * mem.GB)
	original := make([]byte, 64)
	for i := range original {
		original[i] = byte(i + 1)
	}
	globalStorage.Write(0x8000, original)

	gpuPageTable := vm.NewPageTable(12)
	gpuPageTable.Insert(vm.Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x8000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationGPU_LOCAL,
	})
	gpuPageTable.Insert(vm.Page{
		PID: 1, VAddr: 0x2000, PAddr: 0x9000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationCPU_REMOTE,
	})

	testSimulation, _ := buildVirtualGPU(t, "virtual-flow", gpuPageTable, globalStorage)

	gate := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VGate[0]").(*shaderarray.VirtualAccessGate)
	gateConn := testSimulation.GetComponentByName(
		connectionNameOf(t, gate.GetPortByName("Top"))).(*directconnection.Comp)
	agent := newVirtualUVMAgent(testSimulation.GetEngine())
	gateConn.PlugIn(agent.port)

	gateBottom := gate.GetPortByName("Bottom")
	cacheBottom := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VCache[0]").GetPortByName("Bottom")
	gateCapture := &virtualFlowCapture{}
	cacheCapture := &virtualFlowCapture{}
	gateBottom.AcceptHook(gateCapture)
	cacheBottom.AcceptHook(cacheCapture)

	// GPU_LOCAL read: annotated at the gate, VA retained, data from HBM PA.
	read := mem.ReadReqBuilder{}.
		WithSrc(agent.port.AsRemote()).
		WithDst(gate.GetPortByName("Top").AsRemote()).
		WithAddress(0x1004).
		WithByteSize(4).
		WithPID(1).
		Build()
	agent.enqueue(read)
	testSimulation.GetEngine().Run()

	if len(agent.received) != 1 {
		t.Fatalf("the GPU_LOCAL read must complete, got %d responses",
			len(agent.received))
	}
	readRsp, ok := agent.received[0].(*mem.DataReadyRsp)
	if !ok {
		t.Fatalf("the response must carry data, got %T", agent.received[0])
	}
	if readRsp.RespondTo != read.ID {
		t.Fatalf("the response must reply to the read, got %s", readRsp.RespondTo)
	}
	if string(readRsp.Data) != string(original[4:8]) {
		t.Fatalf("the data must come from the HBM PA, got %v", readRsp.Data)
	}

	if len(gateCapture.reads) != 1 {
		t.Fatalf("the gate must forward exactly one request to the cache, got %d",
			len(gateCapture.reads))
	}
	forwarded := gateCapture.reads[0]
	if forwarded.GetAddress() != 0x1004 {
		t.Fatalf("the forwarded request must retain the VA, got 0x%x",
			forwarded.GetAddress())
	}
	ann := cache.ResolveAnnotation(forwarded)
	if ann == nil {
		t.Fatal("the forwarded request must carry the typed annotation")
	}
	if ann.PID != 1 || ann.VAPage != 0x1000 || ann.HBMPA != 0x8000 ||
		ann.Location != vm.MemoryLocationGPU_LOCAL {
		t.Fatalf("the annotation must be (PID, VA page, HBM PA, location), got %+v",
			ann)
	}

	if len(cacheCapture.reads) != 1 {
		t.Fatalf("the L1V refill must reach the L2, got %d reads",
			len(cacheCapture.reads))
	}
	if got := cache.ResolveAnnotation(cacheCapture.reads[0]); got != ann {
		t.Fatalf("the refill must persist the annotation, got %+v", got)
	}

	// CPU_REMOTE read: routed only to the endpoint, never into the cache.
	remoteRead := mem.ReadReqBuilder{}.
		WithSrc(agent.port.AsRemote()).
		WithDst(gate.GetPortByName("Top").AsRemote()).
		WithAddress(0x2004).
		WithByteSize(4).
		WithPID(1).
		Build()
	agent.enqueue(remoteRead)
	testSimulation.GetEngine().Run()

	if len(gateCapture.reads) != 1 {
		t.Fatalf("the CPU_REMOTE read must never enter the cache, got %d reads",
			len(gateCapture.reads))
	}
	if len(cacheCapture.reads) != 1 {
		t.Fatalf("the CPU_REMOTE read must never reach the L2, got %d reads",
			len(cacheCapture.reads))
	}
	if len(agent.received) != 1 {
		t.Fatalf("the CPU_REMOTE read must be committed to the endpoint only, got %d responses",
			len(agent.received))
	}
}

// TestVirtualBarrierROBAndRemote proves the Todo 8 ack invariant on the
// virtual gates: a barrier first acks with the local watermark, a request
// arriving from the ROB after closure parks (never probes, never reaches the
// cache), an old remote read committed before the barrier does not delay the
// ack, and a post-barrier remote read parks until the unblock releases it with
// the new mapping.
func TestVirtualBarrierROBAndRemote(t *testing.T) {
	globalStorage := mem.NewStorage(4 * mem.GB)
	gpuPageTable := vm.NewPageTable(12)
	gpuPageTable.Insert(vm.Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x8000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationGPU_LOCAL,
	})
	gpuPageTable.Insert(vm.Page{
		PID: 1, VAddr: 0x2000, PAddr: 0x9000,
		PageSize: 4096, Valid: true, Managed: true,
		Location: vm.MemoryLocationCPU_REMOTE,
	})

	testSimulation, _ := buildVirtualGPU(t, "virtual-barrier", gpuPageTable, globalStorage)

	gate := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VGate[0]").(*shaderarray.VirtualAccessGate)
	gateConn := testSimulation.GetComponentByName(
		connectionNameOf(t, gate.GetPortByName("Top"))).(*directconnection.Comp)
	agent := newVirtualUVMAgent(testSimulation.GetEngine())
	gateConn.PlugIn(agent.port)

	robComp := testSimulation.GetComponentByName(
		"GPU.SA[0].L1VROB[0]").(*rob.ReorderBuffer)
	robConn := testSimulation.GetComponentByName(
		connectionNameOf(t, robComp.GetPortByName("Top"))).(*directconnection.Comp)
	robAgent := newVirtualUVMAgent(testSimulation.GetEngine())
	robConn.PlugIn(robAgent.port)

	gateBottom := gate.GetPortByName("Bottom")
	gateTranslation := gate.GetPortByName("Translation")
	gateCapture := &virtualFlowCapture{}
	probeCapture := &virtualFlowCapture{}
	gateBottom.AcceptHook(gateCapture)
	gateTranslation.AcceptHook(probeCapture)

	// The gate acks back to the CP's ToGMMU port (a member of the internal
	// connection); the hook captures the ack as it leaves the gate.
	commandProcessor := testSimulation.GetComponentByName(
		"GPU.CommandProcessor").(*cp.CommandProcessor)
	ackCapture := &virtualFlowCapture{}
	gate.GetPortByName("Control").AcceptHook(ackCapture)

	// Barrier first: no request has reached the gate, ack with watermark 0.
	block := &vm.BlockRange{CommandID: 21, PID: 1, StartVA: 0x1000, Size: 0x10000}
	block.ID = sim.GetIDGenerator().Generate()
	block.Src = commandProcessor.ToGMMU.AsRemote()
	block.Dst = gate.GetPortByName("Control").AsRemote()
	if err := gate.GetPortByName("Control").Deliver(block); err != nil {
		t.Fatalf("deliver block: %v", err)
	}
	for i := 0; i < 100 && len(ackCapture.acks) == 0; i++ {
		testSimulation.GetEngine().Run()
	}
	if len(ackCapture.acks) == 0 {
		t.Fatal("the barrier must ack with no in-gate request")
	}
	ack, ok := ackCapture.acks[0].(*vm.BlockAck)
	if !ok {
		t.Fatalf("the ack must be a BlockAck, got %T", ackCapture.acks[0])
	}
	if ack.CommandID != 21 || ack.GateID != shaderarray.VirtualAccessGateIDBase ||
		ack.Watermark != 0 {
		t.Fatalf("the ack must be {21, %d, 0}, got %+v",
			shaderarray.VirtualAccessGateIDBase, ack)
	}

	// A request in the ROB when the barrier lands arrives after closure:
	// parked, never probed, never admitted to the cache.
	robReq := mem.ReadReqBuilder{}.
		WithSrc(robAgent.port.AsRemote()).
		WithDst(robComp.GetPortByName("Top").AsRemote()).
		WithAddress(0x1004).
		WithByteSize(4).
		WithPID(1).
		Build()
	robAgent.enqueue(robReq)
	testSimulation.GetEngine().Run()
	if len(probeCapture.translations) != 0 {
		t.Fatal("the post-barrier request must not probe the L2 TLB")
	}
	if len(gateCapture.reads) != 0 {
		t.Fatal("the post-barrier request must not reach the cache")
	}

	// Unblock releases the parked request with the new mapping.
	unblock := &vm.UnblockRange{CommandID: 21, PID: 1, StartVA: 0x1000, Size: 0x10000}
	unblock.ID = sim.GetIDGenerator().Generate()
	unblock.Src = commandProcessor.ToGMMU.AsRemote()
	unblock.Dst = gate.GetPortByName("Control").AsRemote()
	if err := gate.GetPortByName("Control").Deliver(unblock); err != nil {
		t.Fatalf("deliver unblock: %v", err)
	}
	testSimulation.GetEngine().Run()
	if len(probeCapture.translations) == 0 {
		t.Fatal("the released request must probe the L2 TLB")
	}
	if len(gateCapture.reads) == 0 {
		t.Fatal("the released request must reach the cache")
	}
	if len(robAgent.received) == 0 {
		t.Fatal("the released request must complete to the ROB requester")
	}

	// An old remote read committed before the barrier: the ack fires once the
	// remote read is committed to the endpoint (disposed).
	remoteRead := mem.ReadReqBuilder{}.
		WithSrc(agent.port.AsRemote()).
		WithDst(gate.GetPortByName("Top").AsRemote()).
		WithAddress(0x2004).
		WithByteSize(4).
		WithPID(1).
		Build()
	agent.enqueue(remoteRead)
	testSimulation.GetEngine().Run()
	if len(gateCapture.reads) != 1 {
		t.Fatalf("the old remote read must never enter the cache, got %d reads",
			len(gateCapture.reads))
	}

	block2 := &vm.BlockRange{CommandID: 22, PID: 1, StartVA: 0x2000, Size: 0x10000}
	block2.ID = sim.GetIDGenerator().Generate()
	block2.Src = commandProcessor.ToGMMU.AsRemote()
	block2.Dst = gate.GetPortByName("Control").AsRemote()
	if err := gate.GetPortByName("Control").Deliver(block2); err != nil {
		t.Fatalf("deliver block 2: %v", err)
	}
	for i := 0; i < 100 && len(ackCapture.acks) < 3; i++ {
		testSimulation.GetEngine().Run()
	}
	if len(ackCapture.acks) < 3 {
		t.Fatal("the barrier must ack once the old remote read is committed")
	}
	ack2, ok := ackCapture.acks[2].(*vm.BlockAck)
	if !ok {
		t.Fatalf("the ack must be a BlockAck, got %T", ackCapture.acks[2])
	}
	if ack2.Watermark != 2 {
		t.Fatalf("the watermark must include the old remote read, got %d",
			ack2.Watermark)
	}

	// A post-barrier remote read parks: never probed, never admitted.
	probesBefore := len(probeCapture.translations)
	readsBefore := len(gateCapture.reads)
	lateRemote := mem.ReadReqBuilder{}.
		WithSrc(agent.port.AsRemote()).
		WithDst(gate.GetPortByName("Top").AsRemote()).
		WithAddress(0x2008).
		WithByteSize(4).
		WithPID(1).
		Build()
	agent.enqueue(lateRemote)
	testSimulation.GetEngine().Run()
	if len(probeCapture.translations) != probesBefore {
		t.Fatalf("the post-barrier remote read must not probe, got %d probes",
			len(probeCapture.translations))
	}
	if len(gateCapture.reads) != readsBefore {
		t.Fatalf("the post-barrier remote read must not reach the cache, got %d reads",
			len(gateCapture.reads))
	}

	// Unblock releases it: it probes and is committed to the endpoint only.
	unblock2 := &vm.UnblockRange{CommandID: 22, PID: 1, StartVA: 0x2000, Size: 0x10000}
	unblock2.ID = sim.GetIDGenerator().Generate()
	unblock2.Src = commandProcessor.ToGMMU.AsRemote()
	unblock2.Dst = gate.GetPortByName("Control").AsRemote()
	if err := gate.GetPortByName("Control").Deliver(unblock2); err != nil {
		t.Fatalf("deliver unblock 2: %v", err)
	}
	testSimulation.GetEngine().Run()
	if len(probeCapture.translations) != probesBefore+1 {
		t.Fatalf("the released remote read must probe, got %d probes",
			len(probeCapture.translations))
	}
	if len(gateCapture.reads) != readsBefore {
		t.Fatalf("the released remote read must never enter the cache, got %d reads",
			len(gateCapture.reads))
	}
}
