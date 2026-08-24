package runner

// sbin_codex: UVM statistics reporter tests (plan todo 22 of
// mgpusim-uvm-manager). These plain Go tests drive the real driver through
// its public seams (port Deliver + engine Run) and query the emitted SQLite
// rows directly: same-mode repeats require complete sorted-row equality, and
// the ideal mode's UVM latency rows are zero.

import (
	"context"
	"database/sql"
	"flag"
	"reflect"
	"sort"
	"testing"

	"github.com/sarchlab/akita/v4/datarecording"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// buildReportFixture builds a real UVM driver (normal or ideal), a reporter
// over an in-memory SQLite database, and a connected GPU port so the driver's
// requests flow to a retrievable port. It returns the driver's real GPU port
// (the builder-owned "Driver.ToGPUs" port) and the registered GPU port that
// receives the driver's requests.
func buildReportFixture(t *testing.T, ideal bool) (
	*driver.Driver, *reporter, *sql.DB, sim.Port, sim.Port,
) {
	t.Helper()

	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.Ideal = ideal

	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)
	gpuTables := []vm.PageTable{vm.NewPageTable(12), vm.NewPageTable(12)}

	d := driver.MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGPUPageTables(gpuTables).
		WithGlobalStorage(mem.NewStorage(8 * mem.GB)).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	// The driver sends every request through its builder-owned GPU port; the
	// registered port supplies the request destination address.
	var gpuPort sim.Port
	for _, p := range d.Ports() {
		if p.Name() == "Driver.ToGPUs" {
			gpuPort = p
		}
	}
	if gpuPort == nil {
		t.Fatal("driver GPU port not found")
	}
	registeredPort := sim.NewPort(d, 1, 1, "TestGPU")
	d.RegisterGPU(registeredPort, driver.DeviceProperties{
		CUCount: 4, DRAMSize: 4 * mem.GB,
	})
	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("Conn")
	conn.PlugIn(gpuPort)
	conn.PlugIn(registeredPort)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	rec := datarecording.NewDataRecorderWithDB(db)
	r := &reporter{
		dataRecorder: rec,
		extended:     &extendedReporter{driver: d},
		kernelTimeTracer: &kernelTimeTracer{
			tracer: tracing.NewBusyTimeTracer(
				engine, func(task tracing.Task) bool { return false }),
			comp: d,
		},
	}
	r.dataRecorder.CreateTable(tableName, metric{})

	return d, r, db, gpuPort, registeredPort
}

// contextPID reads the unexported pid of a driver context (the driver
// exposes no pid accessor; the fault envelope needs the allocation's pid).
func contextPID(ctx *driver.Context) vm.PID {
	return vm.PID(reflect.ValueOf(ctx).Elem().FieldByName("pid").Uint())
}

// retrieveReportReq takes the one outstanding request from the dummy port.
func retrieveReportReq(t *testing.T, registeredPort sim.Port) sim.Msg {
	t.Helper()

	req := registeredPort.PeekIncoming()
	if req == nil {
		t.Fatal("no request reached the dummy port")
	}
	registeredPort.RetrieveIncoming()
	return req
}

// deliverReportRsp injects a GeneralRsp completion into the driver's GPU
// port (the Deliver -> NotifyRecv -> TickLater chain schedules the driver).
func deliverReportRsp(t *testing.T, gpuPort sim.Port, originalReq sim.Msg) {
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

// driveReportFault drives one 64 KB demand fault to completion through the
// driver's public seams: intake -> scheduled latency -> H2D -> TLB -> replay.
func driveReportFault(
	t *testing.T,
	d *driver.Driver,
	engine sim.Engine,
	gpuPort, registeredPort sim.Port,
	pid vm.PID,
	ptr uint64,
) {
	t.Helper()

	req := protocol.PageFaultReqBuilder{}.
		WithSrc(gpuPort.AsRemote()).
		WithDst(gpuPort.AsRemote()).
		WithPID(pid).
		WithGPU(1).
		WithVAddr(ptr).
		WithAccessType(vm.AccessKindRead).
		WithFaultPendingToken(vm.FaultPendingToken(1)).
		Build()
	if err := gpuPort.Deliver(req); err != nil {
		t.Fatalf("Deliver PageFaultReq: %v", err)
	}
	engine.Run() // intake, latency event, service, H2D send + forward

	h2d := retrieveReportReq(t, registeredPort)
	if _, ok := h2d.(*protocol.MemCopyH2DReq); !ok {
		t.Fatalf("service request = %T, want MemCopyH2DReq", h2d)
	}
	deliverReportRsp(t, gpuPort, h2d)
	engine.Run() // complete migration, TLB send + forward

	tlb := retrieveReportReq(t, registeredPort)
	tlbReq, ok := tlb.(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", tlb)
	}
	tlbRsp := protocol.UVMTLBInvalidateRspBuilder{}.
		WithSrc(gpuPort.AsRemote()).
		WithDst(gpuPort.AsRemote()).
		WithRspTo(tlbReq.ID).
		Build()
	if err := gpuPort.Deliver(tlbRsp); err != nil {
		t.Fatalf("Deliver TLB ack: %v", err)
	}
	engine.Run() // replay send + forward

	replay := retrieveReportReq(t, registeredPort)
	replayReq, ok := replay.(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", replay)
	}
	replayRsp := protocol.UVMFaultReplayRspBuilder{}.
		WithSrc(gpuPort.AsRemote()).
		WithDst(gpuPort.AsRemote()).
		WithRspTo(replayReq.ID).
		Build()
	if err := gpuPort.Deliver(replayRsp); err != nil {
		t.Fatalf("Deliver replay ack: %v", err)
	}
	engine.Run() // retire the transaction
}

// queryReportRows reads every emitted SQLite row through the data reader.
func queryReportRows(t *testing.T, db *sql.DB) []metricRow {
	t.Helper()

	reader := datarecording.NewReaderWithDB(db)
	reader.MapTable(tableName, metric{})
	results, total, err := reader.Query(
		context.Background(), tableName, datarecording.QueryParams{})
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if total != len(results) {
		t.Fatalf("row count = %d, want %d", len(results), total)
	}
	rows := make([]metricRow, 0, len(results))
	for _, r := range results {
		m, ok := r.(*metric)
		if !ok {
			t.Fatalf("row = %T, want *metric", r)
		}
		rows = append(rows, metricRow{
			Location: m.Location, What: m.What, Value: m.Value, Unit: m.Unit,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Location != rows[j].Location {
			return rows[i].Location < rows[j].Location
		}
		if rows[i].What != rows[j].What {
			return rows[i].What < rows[j].What
		}
		if rows[i].Value != rows[j].Value {
			return rows[i].Value < rows[j].Value
		}
		return rows[i].Unit < rows[j].Unit
	})
	return rows
}

// metricRow is the comparable form of one emitted SQLite row.
type metricRow struct {
	Location string
	What     string
	Value    float64
	Unit     string
}

// metricValue returns the value of the row with the given metric name (0
// when the row is absent).
func metricValue(rows []metricRow, name string) (float64, bool) {
	for _, r := range rows {
		if r.What == name {
			return r.Value, true
		}
	}
	return 0, false
}

// TestUVMSameModeCompleteRows runs the identical predeclared trace twice in
// the same mode and requires COMPLETE sorted-row equality of the emitted
// SQLite rows (the reporter tests query the emitted rows directly — the
// gitignored scripts/ collectors are not the source of truth).
func TestUVMSameModeCompleteRows(t *testing.T) {
	flag.Set("report-all", "true")

	run := func() []metricRow {
		d, r, db, gpuPort, registeredPort := buildReportFixture(t, false)
		ctx := d.Init()
		pid := contextPID(ctx)
		ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
		driveReportFault(t, d, d.Engine, gpuPort, registeredPort, pid, uint64(ptr))
		r.report()
		r.dataRecorder.Flush()
		return queryReportRows(t, db)
	}

	first := run()
	second := run()

	if len(first) == 0 || len(second) == 0 {
		t.Fatal("the reporter must emit rows")
	}
	if len(first) != len(second) {
		t.Fatalf("row count = %d vs %d, want complete equality",
			len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("row %d differs: %+v vs %+v", i, first[i], second[i])
		}
	}

	// The trace's UVM rows are present and nonzero (the reporter tests query
	// the emitted rows, not the ignored scripts/ collectors).
	if v, ok := metricValue(first, "num_gpu_page_fault_requests"); !ok || v != 1 {
		t.Errorf("num_gpu_page_fault_requests = %v (present=%v), want 1",
			v, ok)
	}
	if v, ok := metricValue(first, "bytes_cpu_to_gpu"); !ok || v != 64*1024 {
		t.Errorf("bytes_cpu_to_gpu = %v (present=%v), want 65536", v, ok)
	}
	if v, ok := metricValue(first, "num_uvm_tlb_range_invalidations"); !ok || v != 1 {
		t.Errorf("num_uvm_tlb_range_invalidations = %v (present=%v), want 1",
			v, ok)
	}
	if v, ok := metricValue(first, "uvm_capacity_bytes"); !ok || v != 4*1024*1024*1024 {
		t.Errorf("uvm_capacity_bytes = %v (present=%v), want 4GB", v, ok)
	}
}

// TestUVMInvariantLatencyZero proves the ideal mode's UVM latency rows are
// zero while the functional rows are still emitted (the same counters, no
// duplicate ideal counters).
func TestUVMInvariantLatencyZero(t *testing.T) {
	flag.Set("report-all", "true")

	d, r, db, gpuPort, registeredPort := buildReportFixture(t, true)
	ctx := d.Init()
	pid := contextPID(ctx)
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	driveReportFault(t, d, d.Engine, gpuPort, registeredPort, pid, uint64(ptr))
	r.report()
	r.dataRecorder.Flush()
	rows := queryReportRows(t, db)

	if v, ok := metricValue(rows, "uvm_mode"); !ok || v != 1 {
		t.Errorf("uvm_mode = %v (present=%v), want 1 (ideal)", v, ok)
	}
	for _, name := range []string{"fault_service_latency_total",
		"fault_service_latency_avg"} {
		if v, ok := metricValue(rows, name); !ok || v != 0 {
			t.Errorf("ideal %s = %v (present=%v), want 0", name, v, ok)
		}
	}
	// The functional rows are still emitted and nonzero in ideal mode.
	if v, ok := metricValue(rows, "num_gpu_page_fault_requests"); !ok || v != 1 {
		t.Errorf("ideal num_gpu_page_fault_requests = %v (present=%v), want 1",
			v, ok)
	}
	if v, ok := metricValue(rows, "bytes_cpu_to_gpu"); !ok || v != 64*1024 {
		t.Errorf("ideal bytes_cpu_to_gpu = %v (present=%v), want 65536", v, ok)
	}
}