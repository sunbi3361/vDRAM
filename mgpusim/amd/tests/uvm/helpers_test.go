package uvmtest

// sbin_codex: shared deterministic fixtures for the specification scenario
// integration tests (plan todo 25 of mgpusim-uvm-manager). The tests drive
// the real UVM driver through its public seams (port Deliver + engine Run,
// the same pattern as the samples runner's uvm_report_test.go) and the
// GPU-side AccessCounter/RemoteEndpoint components directly, so the same
// fixtures run under both the serial and the parallel Akita engines with
// identical ordered functional traces and counters.

import (
	"flag"
	"reflect"
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	uvm "github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// engineFlag selects the Akita engine the fixtures run on. The QA command
// passes -args -engine=serial or -args -engine=parallel; both engines must
// produce the identical ordered functional trace and counters.
var engineFlag = flag.String("engine", "serial",
	"akita engine for the scenario fixtures: serial or parallel")

// newEngine builds the engine selected by -engine.
func newEngine(t *testing.T) sim.Engine {
	t.Helper()

	switch *engineFlag {
	case "serial":
		return sim.NewSerialEngine()
	case "parallel":
		return sim.NewParallelEngine()
	default:
		t.Fatalf("unknown -engine %q (want serial or parallel)", *engineFlag)
		return nil
	}
}

// scenarioFixture is the wired driver fixture: the real driver, its
// builder-owned GPU port ("Driver.ToGPUs"), the registered GPU port that
// receives the driver's requests, the CPU page table (authoritative backing
// truth), the per-GPU page tables (published PTE state), and the host
// storage.
type scenarioFixture struct {
	d              *driver.Driver
	engine         sim.Engine
	gpuPort        sim.Port
	registeredPort sim.Port
	pageTable      vm.PageTable
	gpuTables      []vm.PageTable
	storage        *mem.Storage
}

// buildScenarioFixture builds a real UVM driver over the given engine with
// the given config, plus a connected GPU port so the driver's requests flow
// to a retrievable port.
func buildScenarioFixture(
	t *testing.T,
	engine sim.Engine,
	cfg driver.UVMConfig,
) *scenarioFixture {
	t.Helper()

	pageTable := vm.NewPageTable(12)
	gpuTables := []vm.PageTable{vm.NewPageTable(12), vm.NewPageTable(12)}
	storage := mem.NewStorage(8 * mem.GB)

	d := driver.MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGPUPageTables(gpuTables).
		WithGlobalStorage(storage).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	var gpuPort sim.Port
	for _, p := range d.Ports() {
		if p.Name() == "Driver.ToGPUs" {
			gpuPort = p
		}
	}
	if gpuPort == nil {
		t.Fatal("driver GPU port not found")
	}
	registeredPort := sim.NewPort(d, 4096, 4096, "TestGPU")
	d.RegisterGPU(registeredPort, driver.DeviceProperties{
		CUCount: 4, DRAMSize: 4 * mem.GB,
	})
	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("Conn")
	conn.PlugIn(gpuPort)
	conn.PlugIn(registeredPort)

	return &scenarioFixture{
		d: d, engine: engine,
		gpuPort: gpuPort, registeredPort: registeredPort,
		pageTable: pageTable, gpuTables: gpuTables, storage: storage,
	}
}

// latencySeconds converts a duration to the sim time unit (seconds): the
// fault service records sim.VTimeInSec(float64(latency)/float64(time.Second)),
// so 20 us becomes 2e-5 (a bare sim.VTimeInSec(20*time.Microsecond) would be
// 20000 seconds).
func latencySeconds(d time.Duration) sim.VTimeInSec {
	return sim.VTimeInSec(float64(d) / float64(time.Second))
}

// contextPID reads the unexported pid of a driver context (the driver
// exposes no pid accessor; the fault envelope needs the allocation's pid).
func contextPID(ctx *driver.Context) vm.PID {
	return vm.PID(reflect.ValueOf(ctx).Elem().FieldByName("pid").Uint())
}

// retrieveReq takes the one outstanding request from the registered port.
func retrieveReq(t *testing.T, registeredPort sim.Port) sim.Msg {
	t.Helper()

	req := registeredPort.PeekIncoming()
	if req == nil {
		t.Fatal("no request reached the registered port")
	}
	registeredPort.RetrieveIncoming()
	return req
}

// drainReqs takes every outstanding request from the registered port.
func drainReqs(registeredPort sim.Port) []sim.Msg {
	var reqs []sim.Msg
	for {
		req := registeredPort.PeekIncoming()
		if req == nil {
			return reqs
		}
		registeredPort.RetrieveIncoming()
		reqs = append(reqs, req)
	}
}

// deliverGeneralRsp injects a GeneralRsp completion into the driver's GPU
// port (the Deliver -> NotifyRecv -> TickLater chain schedules the driver).
func deliverGeneralRsp(t *testing.T, gpuPort sim.Port, originalReq sim.Msg) {
	t.Helper()

	rsp := &sim.GeneralRsp{}
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = gpuPort.AsRemote()
	rsp.Dst = originalReq.Meta().Src
	rsp.OriginalReq = originalReq
	if err := gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver GeneralRsp: %v", err)
	}
}

// deliverTLBAck injects the CP-style TLB-invalidation completion.
func deliverTLBAck(t *testing.T, gpuPort sim.Port, req *protocol.UVMTLBInvalidateReq) {
	t.Helper()

	rsp := protocol.UVMTLBInvalidateRspBuilder{}.
		WithSrc(gpuPort.AsRemote()).
		WithDst(gpuPort.AsRemote()).
		WithRspTo(req.ID).
		Build()
	if err := gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver TLB ack: %v", err)
	}
}

// deliverReplayAck injects the CP-style replay completion.
func deliverReplayAck(t *testing.T, gpuPort sim.Port, req *protocol.UVMFaultReplayReq) {
	t.Helper()

	rsp := protocol.UVMFaultReplayRspBuilder{}.
		WithSrc(gpuPort.AsRemote()).
		WithDst(gpuPort.AsRemote()).
		WithRspTo(req.ID).
		Build()
	if err := gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver replay ack: %v", err)
	}
}

// deliverFlushRsp injects the CP-style range cache operation completion.
func deliverFlushRsp(
	t *testing.T,
	gpuPort sim.Port,
	req *protocol.UVMCacheRangeFlushReq,
) {
	t.Helper()

	rsp := protocol.UVMCacheRangeFlushRspBuilder{}.
		WithSrc(gpuPort.AsRemote()).
		WithDst(gpuPort.AsRemote()).
		WithRspTo(req.ID).
		Build()
	if err := gpuPort.Deliver(rsp); err != nil {
		t.Fatalf("Deliver flush ack: %v", err)
	}
}

// deliverFault injects one raw 4 KB PageFaultReq into the driver's GPU port.
func deliverFault(
	t *testing.T,
	f *scenarioFixture,
	pid vm.PID,
	vaddr uint64,
) {
	t.Helper()

	req := protocol.PageFaultReqBuilder{}.
		WithSrc(f.gpuPort.AsRemote()).
		WithDst(f.gpuPort.AsRemote()).
		WithPID(pid).
		WithGPU(1).
		WithVAddr(vaddr).
		WithAccessType(vm.AccessKindRead).
		WithFaultPendingToken(vm.FaultPendingToken(1)).
		Build()
	if err := f.gpuPort.Deliver(req); err != nil {
		t.Fatalf("Deliver PageFaultReq: %v", err)
	}
}

// deliverNotification injects one threshold AccessCounterNotification into
// the driver's GPU port.
func deliverNotification(
	t *testing.T,
	f *scenarioFixture,
	pid vm.PID,
	regionBase uint64,
	accessCount uint64,
) {
	t.Helper()

	notif := protocol.AccessCounterNotificationBuilder{}.
		WithSrc(f.gpuPort.AsRemote()).
		WithDst(f.gpuPort.AsRemote()).
		WithPID(pid).
		WithGPU(1).
		WithVAddr(regionBase).
		WithAccessKind(vm.AccessKindRead).
		WithAccessCount(accessCount).
		Build()
	if err := f.gpuPort.Deliver(notif); err != nil {
		t.Fatalf("Deliver AccessCounterNotification: %v", err)
	}
}

// runEngine runs the engine to quiescence.
func runEngine(t *testing.T, f *scenarioFixture) {
	t.Helper()

	if err := f.engine.Run(); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
}

// runEngineFlush runs the engine to quiescence and then keeps running it
// until the driver's GPU port outgoing buffer is drained. The
// directconnection's same-time TickNow coalescing can strand the last
// message of a burst in the outgoing buffer (the connection's tick at time T
// can pop before the driver's tick at T, and the subsequent NotifySend is
// coalesced); the extra runs let the connection forward the stranded
// message.
func runEngineFlush(t *testing.T, f *scenarioFixture) {
	t.Helper()

	for i := 0; i < 8; i++ {
		runEngine(t, f)
		if f.gpuPort.PeekOutgoing() == nil {
			return
		}
	}
	t.Fatal("driver GPU port outgoing buffer not drained after 8 runs")
}

// pteOf returns the page-table entry of vaddr on the given table, failing
// the test when the entry is missing.
func pteOf(t *testing.T, table vm.PageTable, pid vm.PID, vaddr uint64) vm.Page {
	t.Helper()

	page, ok := table.Find(pid, vaddr)
	if !ok {
		t.Fatalf("no PTE for pid=%d va=%#x", pid, vaddr)
	}
	return page
}

// cpuBackingPA returns the authoritative CPU backing PA of the allocation
// page containing vaddr (from the CPU page table truth).
func cpuBackingPA(t *testing.T, f *scenarioFixture, pid vm.PID, vaddr uint64) uint64 {
	t.Helper()

	return pteOf(t, f.pageTable, pid, vaddr).PAddr
}

// assertOnlyTypes fails when the request stream contains a type outside the
// allowed set (the absence-of-prohibited-global-controls contract).
func assertOnlyTypes(t *testing.T, reqs []sim.Msg, allowed ...string) {
	t.Helper()

	allowedSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = true
	}
	for _, r := range reqs {
		name := reflect.TypeOf(r).String()
		if !allowedSet[name] {
			t.Errorf("prohibited global control in the stream: %s", name)
		}
	}
}

// buildCounterAndEndpoint wires the GPU-side AccessCounter and
// RemoteEndpoint. The counter's ToCP seam and the endpoint's ToRDMA seam are
// loop-back shared ports: the notification and the forwarded read land in
// their own incoming buffers (peeked directly). The endpoint's ToGPU seam is
// connected to the requester port so the data response routes back to the
// original GPU requester.
func buildCounterAndEndpoint(
	t *testing.T,
	engine sim.Engine,
	threshold uint64,
) (*uvm.AccessCounter, *uvm.RemoteEndpoint, sim.Port) {
	t.Helper()

	counter := uvm.MakeAccessCounterBuilder().
		WithEngine(engine).
		WithThreshold(threshold).
		Build("Counter")
	endpoint := uvm.MakeRemoteEndpointBuilder().
		WithEngine(engine).
		WithGPU(1).
		WithAccessCounter(counter).
		Build("Endpoint")

	requesterPort := sim.NewPort(endpoint, 1, 1, "RequesterPort")

	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("EndpointConn")
	conn.PlugIn(endpoint.ToGPU)
	conn.PlugIn(requesterPort)

	return counter, endpoint, requesterPort
}

// deliverRemoteRead delivers one CPU_REMOTE read of byteSize bytes at the
// managed vaddr to the endpoint. The annotation carries the consumable CPU
// backing PA.
func deliverRemoteRead(
	t *testing.T,
	endpoint *uvm.RemoteEndpoint,
	requesterPort sim.Port,
	pid vm.PID,
	vaddr, cpuPA, byteSize uint64,
) *mem.ReadReq {
	t.Helper()

	req := mem.ReadReqBuilder{}.
		WithSrc(requesterPort.AsRemote()).
		WithDst(endpoint.ToGPU.AsRemote()).
		WithAddress(vaddr).
		WithByteSize(byteSize).
		WithPID(pid).
		Build()
	req.Info = &uvm.RemoteAccessAnnotation{
		Location: vm.MemoryLocationCPU_REMOTE,
		PAddr:    cpuPA,
	}
	if err := endpoint.ToGPU.Deliver(req); err != nil {
		t.Fatalf("Deliver remote read: %v", err)
	}
	return req
}

// deliverRemoteWrite delivers one CPU_REMOTE write of byteSize bytes at the
// managed vaddr to the endpoint. The annotation carries the consumable CPU
// backing PA.
func deliverRemoteWrite(
	t *testing.T,
	endpoint *uvm.RemoteEndpoint,
	requesterPort sim.Port,
	pid vm.PID,
	vaddr, cpuPA, byteSize uint64,
	data []byte,
) *mem.WriteReq {
	t.Helper()

	req := mem.WriteReqBuilder{}.
		WithSrc(requesterPort.AsRemote()).
		WithDst(endpoint.ToGPU.AsRemote()).
		WithAddress(vaddr).
		WithPID(pid).
		WithData(data).
		Build()
	req.Info = &uvm.RemoteAccessAnnotation{
		Location: vm.MemoryLocationCPU_REMOTE,
		PAddr:    cpuPA,
	}
	if err := endpoint.ToGPU.Deliver(req); err != nil {
		t.Fatalf("Deliver remote write: %v", err)
	}
	return req
}
