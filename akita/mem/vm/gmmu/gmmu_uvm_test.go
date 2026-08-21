package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

func newUVMMiddleware() *middleware {
	engine := sim.NewSerialEngine()
	comp := &Comp{latency: 10, log2PageSize: 12}
	comp.TickingComponent = *sim.NewTickingComponent(
		"GMMUUVMTest",
		engine,
		sim.GHz,
		nil,
	)
	comp.uvmPort = sim.NewPort(comp, 8, 8, "GMMUUVMTest.UVMPort")
	dummyPort := sim.NewPort(comp, 8, 8, "GMMUUVMTest.DummyPort")
	comp.accessCounters = make(map[uint64]uint64)
	comp.accessCounterNotified = make(map[uint64]bool)
	comp.accessCounterThreshold = 64
	// Wire the UVM port through a direct connection so notification sends
	// resolve without a full platform.
	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(sim.GHz).
		Build("GMMUUVMTest.UVMConn")
	conn.PlugIn(comp.uvmPort)
	conn.PlugIn(dummyPort)
	comp.UVMServiceProvider = dummyPort.AsRemote()
	return &middleware{Comp: comp}
}

// sbin_codex: writes to remotely-accessible pages fault immediately; reads
// are served as remote accesses.
func TestNeedsUVMFaultWriteImmediate(t *testing.T) {
	m := newUVMMiddleware()
	remote := vm.Page{DeviceID: 0, Managed: true, RemoteAccessible: true}

	if m.needsUVMFault(remote, &vm.TranslationReq{IsWrite: true}) != true {
		t.Fatal("write to remotely-accessible page must fault immediately")
	}
	if m.needsUVMFault(remote, &vm.TranslationReq{IsWrite: false}) != false {
		t.Fatal("read of remotely-accessible page must be a remote access")
	}
	cpu := vm.Page{DeviceID: 0, Managed: true, RemoteAccessible: false}
	if m.needsUVMFault(cpu, &vm.TranslationReq{}) != true {
		t.Fatal("CPU-resident non-remote page must fault")
	}
}

// sbin_codex: the GMMU counts remote reads at 64KB granularity and notifies
// the driver once at the threshold.
func TestCountRemoteAccessNotifiesAtThreshold(t *testing.T) {
	m := newUVMMiddleware()
	page := vm.Page{PID: 1, DeviceID: 0, Managed: true, RemoteAccessible: true}
	req := &vm.TranslationReq{PID: 1, VAddr: 0x1000, DeviceID: 1}

	for i := uint64(0); i < m.accessCounterThreshold-1; i++ {
		m.countRemoteAccess(page, req)
	}
	if len(m.accessCounterNotified) != 0 {
		t.Fatal("notification fired before the threshold")
	}
	m.countRemoteAccess(page, req)
	if len(m.accessCounterNotified) != 1 {
		t.Fatal("notification must fire exactly once at the threshold")
	}
	key := (uint64(1) << 32) | (0x1000 >> 16)
	if m.accessCounters[key] < m.accessCounterThreshold {
		t.Fatalf("counter = %d, want >= %d", m.accessCounters[key], m.accessCounterThreshold)
	}
	// Subsequent accesses must not notify again.
	m.countRemoteAccess(page, req)
	if len(m.accessCounterNotified) != 1 {
		t.Fatal("duplicate notification after threshold")
	}
}

// sbin_codex: the driver-issued reset clears the counter and the latch.
func TestResetAccessCounter(t *testing.T) {
	m := newUVMMiddleware()
	page := vm.Page{PID: 1, DeviceID: 0, Managed: true, RemoteAccessible: true}
	req := &vm.TranslationReq{PID: 1, VAddr: 0x1000, DeviceID: 1}
	for i := uint64(0); i < m.accessCounterThreshold; i++ {
		m.countRemoteAccess(page, req)
	}
	if len(m.accessCounterNotified) != 1 {
		t.Fatal("notification did not fire")
	}
	m.resetAccessCounter(1, 0x1000)
	if len(m.accessCounters) != 0 || len(m.accessCounterNotified) != 0 {
		t.Fatal("reset must clear counter and latch")
	}
}
