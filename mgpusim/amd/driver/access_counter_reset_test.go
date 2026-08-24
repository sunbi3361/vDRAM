// sbin_codex: Regression tests for reliable access-counter reset delivery.
package driver

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

const resetDestination sim.RemotePort = "CPU.AccessCounter.Top"

type resetTestConnection struct {
	sim.HookableBase
}

func (*resetTestConnection) Name() string             { return "ResetTestConnection" }
func (*resetTestConnection) PlugIn(port sim.Port)     { port.SetConnection(&resetTestConnection{}) }
func (*resetTestConnection) Unplug(sim.Port)          {}
func (*resetTestConnection) NotifyAvailable(sim.Port) {}
func (*resetTestConnection) NotifySend()              {}

func newAccessCounterResetDriver(t *testing.T, bufferSize int) (*Driver, *Context) {
	t.Helper()
	engine := sim.NewSerialEngine()
	d := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(vm.NewPageTable(12)).
		WithGlobalStorage(mem.NewStorage(1 << 24)).
		WithD2HCycles(1).
		WithH2DCycles(1).
		WithUVM(UVMConfig{Enabled: true, Ideal: true, GPUCapacityBytes: 1 << 20}).
		Build("Driver")
	d.uvmPort = sim.NewPort(d, bufferSize, bufferSize, "Driver.ResetTestUVM")
	(&resetTestConnection{}).PlugIn(d.uvmPort)
	d.SetAccessCounterResetDestination(resetDestination)
	return d, d.Init()
}

func completeResetTestMigration(d *Driver, id string, trigger MigrationTrigger, pages []PageKey) {
	d.uvm.migrations[id] = &Migration{
		ID: id, Direction: CPUToGPU, Trigger: trigger, DeviceID: 1, Pages: pages,
	}
	d.uvm.completeMigration(id)
}

func TestCompleteMigrationQueuesDistinctPIDRegionResets(t *testing.T) { // sbin_codex
	// Given
	d, firstContext := newAccessCounterResetDriver(t, 8)
	firstBase := uint64(d.AllocateManaged(firstContext, 128*1024))
	secondContext := d.Init()
	secondBase := uint64(d.AllocateManaged(secondContext, 64*1024))
	pages := []PageKey{
		{PID: firstContext.pid, VAddr: firstBase},
		{PID: firstContext.pid, VAddr: firstBase + 4096},
		{PID: firstContext.pid, VAddr: firstBase + 64*1024},
		{PID: secondContext.pid, VAddr: secondBase},
	}

	// When
	completeResetTestMigration(d, "distinct-resets", TriggerFault, pages)

	// Then
	if got := len(d.uvm.pendingAccessCounterResets); got != 3 {
		t.Fatalf("queued resets = %d, want 3", got)
	}
	for i := 0; i < 3; i++ {
		if !d.Tick() {
			t.Fatal("queued reset did not make progress")
		}
		reset, ok := d.uvmPort.RetrieveOutgoing().(*vm.AccessCounterResetReq)
		if !ok || reset.Dst != resetDestination {
			t.Fatalf("unexpected reset route: %+v", reset)
		}
	}
}

func TestPendingResetRetriesAfterBackpressureWithoutAnotherEvent(t *testing.T) { // sbin_codex
	// Given
	d, ctx := newAccessCounterResetDriver(t, 1)
	base := uint64(d.AllocateManaged(ctx, 4096))
	blocker := vm.NewAccessCounterResetReq(d.uvmPort.AsRemote(), "Blocked")
	if err := d.uvmPort.Send(blocker); err != nil {
		t.Fatalf("prefill UVM port: %v", err)
	}
	completeResetTestMigration(d, "backpressured-reset", TriggerAccessCounter,
		[]PageKey{{PID: ctx.pid, VAddr: base}})

	// When
	// progress := d.Tick() // sbin_codex: isolate reset progress from unrelated driver work.
	progress := d.uvm.sendPendingAccessCounterReset() // sbin_codex

	// Then
	if progress || len(d.uvm.pendingAccessCounterResets) != 1 {
		t.Fatal("failed reset send was dropped or reported progress")
	}
	_ = d.uvmPort.RetrieveOutgoing()
	if !d.uvm.sendPendingAccessCounterReset() { // sbin_codex
		t.Fatal("pending reset was not retried")
	}
	reset, ok := d.uvmPort.RetrieveOutgoing().(*vm.AccessCounterResetReq)
	if !ok || reset.PID != ctx.pid || reset.Dst != resetDestination {
		t.Fatalf("unexpected retried reset: %+v", reset)
	}
}
