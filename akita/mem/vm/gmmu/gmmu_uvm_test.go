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
	// comp.uvmPort = sim.NewPort(comp, 8, 8, "GMMUUVMTest.UVMPort")
	comp.uvmPort = sim.NewPort(comp, 8, 8, "GMMUUVMTest.UVMPort") // sbin_codex
	comp.topPort = sim.NewPort(comp, 8, 8, "GMMUUVMTest.TopPort") // sbin_codex
	dummyPort := sim.NewPort(comp, 8, 8, "GMMUUVMTest.DummyPort")
	comp.pageTable = vm.NewPageTable(12) // sbin_codex
	// Wire the UVM port through a direct connection so notification sends
	// resolve without a full platform.
	conn := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(sim.GHz).
		Build("GMMUUVMTest.UVMConn")
	conn.PlugIn(comp.uvmPort)
	conn.PlugIn(comp.topPort) // sbin_codex
	conn.PlugIn(dummyPort)
	comp.UVMServiceProvider = dummyPort.AsRemote()
	return &middleware{Comp: comp}
}

// sbin_codex: writes to remotely-accessible pages fault immediately; reads
// are served as remote accesses.
//
// Old immediate-write gating test, preserved for reference:
// func TestNeedsUVMFaultWriteImmediate(t *testing.T) {
// 	m := newUVMMiddleware()
// 	remote := vm.Page{DeviceID: 0, Managed: true, RemoteAccessible: true}
//
// 	if m.needsUVMFault(remote, &vm.TranslationReq{IsWrite: true}) != true {
// 		t.Fatal("write to remotely-accessible page must fault immediately")
// 	}
// 	if m.needsUVMFault(remote, &vm.TranslationReq{IsWrite: false}) != false {
// 		t.Fatal("read of remotely-accessible page must be a remote access")
// 	}
// 	cpu := vm.Page{DeviceID: 0, Managed: true, RemoteAccessible: false}
// 	if m.needsUVMFault(cpu, &vm.TranslationReq{}) != true {
// 		t.Fatal("CPU-resident non-remote page must fault")
// 	}
// }

// sbin_codex: every access to a remotely-accessible CPU-resident managed
// page, including writes, is served as a remote access; only cold pages and
// migrating pages fault.
func TestNeedsUVMFaultRemoteAccess(t *testing.T) {
	m := newUVMMiddleware()
	remote := vm.Page{DeviceID: 0, Managed: true, RemoteAccessible: true}

	if m.needsUVMFault(remote, &vm.TranslationReq{IsWrite: true}) != false {
		t.Fatal("write to remotely-accessible page must be a remote access")
	}
	if m.needsUVMFault(remote, &vm.TranslationReq{IsWrite: false}) != false {
		t.Fatal("read of remotely-accessible page must be a remote access")
	}
	cpu := vm.Page{DeviceID: 0, Managed: true, RemoteAccessible: false}
	if m.needsUVMFault(cpu, &vm.TranslationReq{}) != true {
		t.Fatal("CPU-resident non-remote page must fault")
	}
	migrating := vm.Page{
		DeviceID: 0, Managed: true, RemoteAccessible: true, IsMigrating: true,
	}
	if m.needsUVMFault(migrating, &vm.TranslationReq{IsWrite: true}) != true {
		t.Fatal("migrating page must fault")
	}
}

// sbin_codex: the GMMU counts remote reads at 64KB granularity and notifies
// the driver once at the threshold.
// The former GMMU counter/notification/reset tests are obsolete: the PCIe
// accesscounter is now the sole counter owner. // sbin_codex

func TestFinalizePageWalkRemotePageOnlyEmitsTranslationRsp(t *testing.T) { // sbin_codex
	// Given
	m := newUVMMiddleware()
	page := vm.Page{PID: 1, VAddr: 0x1000, DeviceID: 0, Managed: true, RemoteAccessible: true}
	m.pageTable.Insert(page)

	// When
	for i := 0; i < 3; i++ {
		req := vm.TranslationReqBuilder{}.
			WithSrc("Requester").
			WithDst(m.topPort.AsRemote()).
			WithPID(1).
			WithVAddr(0x1000).
			WithDeviceID(1).
			Build()
		m.walkingTranslations = []transaction{{req: req, state: pageWalkComplete}}
		if !m.finalizePageWalk(0) {
			t.Fatal("remote page walk did not complete")
		}
		if _, ok := m.topPort.RetrieveOutgoing().(*vm.TranslationRsp); !ok {
			t.Fatal("remote page walk did not emit TranslationRsp")
		}
	}

	// Then
	if got := m.uvmPort.RetrieveOutgoing(); got != nil {
		t.Fatalf("GMMU emitted non-translation message %T", got)
	}
}

func TestProcessUVMFaultRspDoesNotAcceptAccessCounterReset(t *testing.T) { // sbin_codex
	// Given
	m := newUVMMiddleware()
	reset := vm.NewAccessCounterResetReq("Driver.UVM", m.uvmPort.AsRemote())
	if err := m.uvmPort.Deliver(reset); err != nil {
		t.Fatalf("deliver reset: %v", err)
	}

	// When
	progress := m.processUVMFaultRsp()

	// Then
	if progress || m.uvmPort.PeekIncoming() != reset {
		t.Fatal("GMMU accepted AccessCounterResetReq")
	}
}
