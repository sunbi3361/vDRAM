// sbin_codex: Regression tests for translation-coherent CPU-to-GPU migration.
package driver

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

type migrationQuiescenceFixture struct { // sbin_codex
	driver       *Driver
	context      *Context
	engine       *sim.SerialEngine
	cpuPageTable vm.PageTable
	gpuPageTable vm.PageTable
}

func newMigrationQuiescenceFixture(t *testing.T) migrationQuiescenceFixture { // sbin_codex
	t.Helper()
	engine := sim.NewSerialEngine()
	cpuPageTable := vm.NewPageTable(12)
	gpuPageTable := vm.NewPageTable(12)
	driver := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(cpuPageTable).
		WithGPUPageTables([]vm.PageTable{gpuPageTable}).
		// WithGlobalStorage(mem.NewStorage(1 << 24)). // sbin_codex: GPU frames occupy the device physical-address range.
		WithGlobalStorage(mem.NewStorage(8 * mem.GB)). // sbin_codex
		WithD2HCycles(1).
		WithH2DCycles(1).
		WithUVM(UVMConfig{Enabled: true, Ideal: true, GPUCapacityBytes: 1 << 20}).
		Build("Driver")
	driver.RegisterGPU(
		sim.NewPort(driver, 8, 8, "TestGPU.CP"),
		DeviceProperties{CUCount: 1, DRAMSize: 1 << 20},
	)
	connection := directconnection.MakeBuilder().
		WithEngine(engine).
		WithFreq(1 * sim.GHz).
		Build("MigrationQuiescenceConnection")
	connection.PlugIn(driver.gpuPort)
	connection.PlugIn(driver.GPUs[0])

	return migrationQuiescenceFixture{
		driver: driver, context: driver.Init(), engine: engine,
		cpuPageTable: cpuPageTable, gpuPageTable: gpuPageTable,
	}
}

func (f migrationQuiescenceFixture) remotelyMapPage(t *testing.T, key PageKey) *ManagedPage { // sbin_codex
	t.Helper()
	page := f.driver.uvm.pages[key]
	if page == nil {
		t.Fatalf("managed page %v is missing", key)
	}
	page.RemoteMapped = true
	f.driver.memAllocator.UpdatePage(vm.Page{
		PID: key.PID, VAddr: key.VAddr, PAddr: page.CPUBackingPAddr,
		PageSize: 4096, Valid: true, DeviceID: 0, Managed: true,
		RemoteAccessible: true,
	})
	return page
}

func (f migrationQuiescenceFixture) startFaultMigration(t *testing.T, key PageKey) *Migration { // sbin_codex
	t.Helper()
	fault := &PageFault{
		ID: "fault-migration", Key: FaultKey{Page: key, DeviceID: 1},
		State: FaultReady,
	}
	f.driver.uvm.stateMu.Lock()
	f.driver.uvm.startCPUGPUMigration(fault, tbnSelection{
		pageKeys: []PageKey{key}, demandKey: key, demandPages: 1,
	})
	f.driver.uvm.stateMu.Unlock()
	for _, migration := range f.driver.uvm.migrations {
		return migration
	}
	t.Fatal("fault migration was not created")
	return nil
}

func queuedShootdown(t *testing.T, driver *Driver) *protocol.ShootDownCommand { // sbin_codex
	t.Helper()
	driver.requestsToSendMutex.Lock()
	defer driver.requestsToSendMutex.Unlock()
	for _, queued := range driver.requestsToSend {
		if request, ok := queued.(*protocol.ShootDownCommand); ok {
			return request
		}
	}
	t.Fatalf("queued GPU requests contain no *protocol.ShootDownCommand: %T", driver.requestsToSend)
	return nil
}

func assertRDMADrainQueued(t *testing.T, driver *Driver) { // sbin_codex
	t.Helper()
	driver.requestsToSendMutex.Lock()
	defer driver.requestsToSendMutex.Unlock()
	for _, queued := range driver.requestsToSend {
		if _, ok := queued.(*protocol.RDMADrainCmdFromDriver); ok {
			return
		}
	}
	t.Fatalf("queued GPU requests contain no *protocol.RDMADrainCmdFromDriver: %T", driver.requestsToSend)
}

func drainUVMTestQuiescence(driver *Driver, engine *sim.SerialEngine) { // sbin_codex
	for {
		if driver.uvm.hasPendingEvictions() {
			driver.uvm.finalizeEviction()
			_ = engine.Run()
			continue
		}
		if driver.uvm.hasPendingMigrationDrain() {
			driver.processRDMADrainRsp(&protocol.RDMADrainRspToDriver{})
			continue
		}
		if driver.uvm.hasPendingMigrationQuiescence() {
			driver.uvm.finalizeMigrationQuiescence()
			_ = engine.Run()
			continue
		}
		if driver.uvm.hasPendingMigrationGPURestart() {
			driver.handleGPURestartRsp(&protocol.GPURestartRsp{})
			continue
		}
		if driver.uvm.hasPendingMigrationRDMARestart() {
			driver.processRDMARestartRspToDriver(&protocol.RDMARestartRspToDriver{})
			continue
		}
		return
	}
}

func assertMigratingPTE(t *testing.T, table vm.PageTable, key PageKey, backing uint64) { // sbin_codex
	t.Helper()
	pte, found := table.Find(key.PID, key.VAddr)
	if !found {
		t.Fatalf("PTE for %v is missing", key)
	}
	if !pte.IsMigrating || !pte.Managed || !pte.RemoteAccessible ||
		pte.PAddr != backing || pte.DeviceID != 0 || pte.PageSize != 4096 {
		t.Fatalf("migrating PTE = %+v, want CPU-backed managed remote PTE", pte)
	}
}

func TestFaultMigrationPublishesMigratingPTEBeforeQuiescence(t *testing.T) { // sbin_codex
	// Given
	fixture := newMigrationQuiescenceFixture(t)
	base := uint64(fixture.driver.AllocateManaged(fixture.context, 4096))
	key := PageKey{PID: fixture.context.pid, VAddr: base}
	page := fixture.remotelyMapPage(t, key)

	// When
	fixture.startFaultMigration(t, key)

	// Then
	assertMigratingPTE(t, fixture.cpuPageTable, key, page.CPUBackingPAddr)
	assertMigratingPTE(t, fixture.gpuPageTable, key, page.CPUBackingPAddr)
}

func TestAccessCounterMigrationPublishesMigratingPTEWithoutFaultWaiter(t *testing.T) { // sbin_codex
	// Given
	fixture := newMigrationQuiescenceFixture(t)
	base := uint64(fixture.driver.AllocateManaged(fixture.context, 4096))
	key := PageKey{PID: fixture.context.pid, VAddr: base}
	page := fixture.remotelyMapPage(t, key)

	// When
	fixture.driver.uvm.onAccessCounterNotify(
		fixture.context.pid, base&^(64*1024-1), 1)

	// Then
	assertMigratingPTE(t, fixture.gpuPageTable, key, page.CPUBackingPAddr)
	assertRDMADrainQueued(t, fixture.driver)
	fixture.driver.processRDMADrainRsp(&protocol.RDMADrainRspToDriver{})
	migration := queuedShootdown(t, fixture.driver)
	if len(migration.VAddr) == 0 {
		t.Fatal("access-counter migration shootdown has no pages")
	}
}

func TestMigrationCopiesOnlyOnceAfterShootdownACK(t *testing.T) { // sbin_codex
	// Given
	fixture := newMigrationQuiescenceFixture(t)
	base := uint64(fixture.driver.AllocateManaged(fixture.context, 4096))
	key := PageKey{PID: fixture.context.pid, VAddr: base}
	page := fixture.remotelyMapPage(t, key)
	want := []byte{1, 2, 3, 4}
	if err := fixture.driver.globalStorage.Write(page.CPUBackingPAddr, want); err != nil {
		t.Fatalf("write CPU backing: %v", err)
	}
	migration := fixture.startFaultMigration(t, key)

	// When
	beforeACK, err := fixture.driver.globalStorage.Read(page.GPUFramePAddr, uint64(len(want)))
	if err != nil {
		t.Fatalf("read GPU frame before ACK: %v", err)
	}

	// Then
	if beforeACK[0] != 0 || beforeACK[1] != 0 || beforeACK[2] != 0 || beforeACK[3] != 0 {
		t.Fatalf("GPU frame before shootdown ACK = %v, want zeroed destination", beforeACK)
	}
	assertRDMADrainQueued(t, fixture.driver)
	if !fixture.driver.processRDMADrainRsp(&protocol.RDMADrainRspToDriver{}) {
		t.Fatal("RDMA drain ACK did not start shootdown")
	}
	shootdown := queuedShootdown(t, fixture.driver)
	if len(shootdown.VAddr) != 1 || shootdown.VAddr[0] != key.VAddr {
		t.Fatalf("shootdown pages = %v, want [%#x]", shootdown.VAddr, key.VAddr)
	}
	if !fixture.driver.processShootdownCompleteRsp(&protocol.ShootDownCompleteRsp{}) {
		t.Fatal("shootdown ACK did not start migration data")
	}
	afterACK, err := fixture.driver.globalStorage.Read(page.GPUFramePAddr, uint64(len(want)))
	if err != nil {
		t.Fatalf("read GPU frame after ACK: %v", err)
	}
	if string(afterACK) != string(want) {
		t.Fatalf("GPU frame after shootdown ACK = %v, want %v", afterACK, want)
	}
	if err := fixture.driver.globalStorage.Write(page.CPUBackingPAddr, []byte{9, 9, 9, 9}); err != nil {
		t.Fatalf("mutate CPU backing after ACK: %v", err)
	}
	if err := fixture.engine.Run(); err != nil {
		t.Fatalf("complete migration: %v", err)
	}
	afterCompletion, err := fixture.driver.globalStorage.Read(page.GPUFramePAddr, uint64(len(want)))
	if err != nil {
		t.Fatalf("read GPU frame after completion: %v", err)
	}
	if string(afterCompletion) != string(want) {
		t.Fatalf("migration %s copied more than once: got %v, want %v", migration.ID, afterCompletion, want)
	}
	if page.State != GPUResident {
		t.Fatalf("access-counter-free migration state = %v, want GPUResident", page.State)
	}
	if !fixture.driver.uvm.hasPendingMigrationGPURestart() {
		t.Fatal("migration completion did not begin GPU restart")
	}
	fixture.driver.handleGPURestartRsp(&protocol.GPURestartRsp{})
	if !fixture.driver.uvm.hasPendingMigrationRDMARestart() {
		t.Fatal("GPU restart ACK did not begin RDMA restart")
	}
	fixture.driver.processRDMARestartRspToDriver(&protocol.RDMARestartRspToDriver{})
}

func TestEvictionAndMigrationQuiescenceSerialize(t *testing.T) { // sbin_codex
	// Given
	fixture := newMigrationQuiescenceFixture(t)
	base := uint64(fixture.driver.AllocateManaged(fixture.context, 4096))
	key := PageKey{PID: fixture.context.pid, VAddr: base}
	fixture.remotelyMapPage(t, key)
	fixture.driver.uvm.evictACK = 1

	// When
	fixture.driver.uvm.onAccessCounterNotify(
		fixture.context.pid, base&^(64*1024-1), 1)

	// Then
	if len(fixture.driver.uvm.pendingResumes) != 1 || len(fixture.driver.uvm.migrations) != 0 {
		t.Fatalf("overlap state: resumes=%d migrations=%d, want queued request only",
			len(fixture.driver.uvm.pendingResumes), len(fixture.driver.uvm.migrations))
	}
	fixture.driver.uvm.finalizeEviction()
	assertRDMADrainQueued(t, fixture.driver)
	fixture.driver.processRDMADrainRsp(&protocol.RDMADrainRspToDriver{})
	queuedShootdown(t, fixture.driver)
	if len(fixture.driver.uvm.migrations) != 1 {
		t.Fatalf("resumed migrations = %d, want one", len(fixture.driver.uvm.migrations))
	}
}
