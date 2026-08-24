package driver

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// sbin_codex: Access-Counter and remote-write migration transaction contract
// tests (todo 18 of plan mgpusim-uvm-manager, uvm-manager.md §15, §16, §21.3).
// The QA regex
// 'TestUVM(AccessCounterMigration|RemoteWriteMigration|AdmissionBlockBarrier|
// ExistingRemoteRead|NotificationCoalescing)' runs the fixtures in this file:
// threshold and write triggers become one migration transaction each; the
// transaction issues a commandID BlockRange and waits the Todo-8 watermark
// completion before any H2D; the PTE is published REMOTE -> GPU_LOCAL only
// after the DMA and the mandatory 64 KB TLB invalidate follows the PTE update
// (§21.2/§21.3); recency is updated only on admission transitions (§31.2);
// the parked remote write is never committed to host (§15); old remote reads
// complete concurrently without disturbing the transaction; and a notification
// during active work is suppressed (§16) — no duplicate transaction, no
// duplicate DMA.

// buildMigrationDriver builds a real driver (real allocator, real CPU + GPU
// page tables, real UVM manager, host storage) with the AC/write migration
// middleware wired, plus a registered GPU port.
func buildMigrationDriver(t *testing.T) (
	*Driver, *migrationMiddleware, []vm.PageTable,
) {
	t.Helper()

	cfg := DefaultUVMConfig()
	cfg.Enabled = true

	engine := sim.NewSerialEngine()
	pageTable := vm.NewPageTable(12)
	gpuTables := []vm.PageTable{vm.NewPageTable(12), vm.NewPageTable(12)}

	d := MakeBuilder().
		WithEngine(engine).
		WithLog2PageSize(12).
		WithPageTable(pageTable).
		WithGPUPageTables(gpuTables).
		WithGlobalStorage(mem.NewStorage(8 * mem.GB)).
		WithUVMConfig(cfg).
		WithUVMGPUMemorySize(4 * mem.GB).
		Build("Driver")

	gpuPort := sim.NewPort(d, 1, 1, "TestGPU")
	d.RegisterGPU(gpuPort, DeviceProperties{CUCount: 4, DRAMSize: 4 * mem.GB})

	return d, d.uvmMigration, gpuTables
}

// intakeNotification delivers one threshold AccessCounterNotification through
// the driver's GPU port and consumes it via the notification intake seam.
func intakeNotification(t *testing.T, d *Driver, pid vm.PID, gpu int, vaddr uint64) {
	t.Helper()

	notif := protocol.AccessCounterNotificationBuilder{}.
		WithSrc(d.gpuPort.AsRemote()).
		WithDst(d.gpuPort.AsRemote()).
		WithPID(pid).
		WithGPU(gpu).
		WithVAddr(vaddr).
		WithAccessKind(vm.AccessKindRead).
		WithAccessCount(8).
		Build()
	if err := d.gpuPort.Deliver(notif); err != nil {
		t.Fatalf("Deliver notification: %v", err)
	}
	if !d.processReturnReq() {
		t.Fatalf("processReturnReq did not consume the notification at %#x", vaddr)
	}
}

// intakeRemoteWrite drives the remote-write migration trigger directly (the
// parked write itself is GPU-side; the driver policy consumes the trigger).
func intakeRemoteWrite(t *testing.T, d *Driver, pid vm.PID, gpu int, vaddr uint64) {
	t.Helper()

	if !d.uvmMigration.intakeRemoteWrite(pid, gpu, vaddr) {
		t.Fatalf("intakeRemoteWrite did not consume the trigger at %#x", vaddr)
	}
}

// TestUVMAccessCounterMigration drives the full threshold-migration
// transaction: notification -> claim -> commandID block -> H2D -> PTE
// REMOTE->GPU_LOCAL -> mandatory 64 KB TLB invalidate -> GPU_RESIDENT (recency)
// -> replay -> unblock -> done, with exact trigger-specific counters.
func TestUVMAccessCounterMigration(t *testing.T) {
	d, mw, gpuTables := buildMigrationDriver(t)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]
	region := reg.VABlocks[0].SubBlocks[0]

	// Distinct CPU backing bytes so the DMA payload is observable.
	cpuData := make([]byte, 15*basePageSize)
	for i := range cpuData {
		cpuData[i] = byte(i * 3)
	}
	for i := 0; i < 15; i++ {
		d.globalStorage.Write(reg.CPUBackingPages[i],
			cpuData[i*basePageSize:(i+1)*basePageSize])
	}

	// Recency sentinel: the admission transitions must update it (§31.2).
	region.LastMigrationTime = 42

	// Threshold notification -> one migration transaction.
	intakeNotification(t, d, pid, 1, uint64(ptr))
	if mw.active == nil {
		t.Fatal("no migration transaction after the notification")
	}
	tx := mw.active
	if tx.Trigger != migrationTriggerAccessCounter {
		t.Errorf("trigger = %v, want access counter", tx.Trigger)
	}
	if got := d.uvm.AccessCounterMigrationCount(); got != 1 {
		t.Errorf("AC migration count = %d, want 1", got)
	}
	if got := d.uvm.RemoteWriteMigrationCount(); got != 0 {
		t.Errorf("write migration count = %d, want 0", got)
	}
	if region.State != RegionMigratingToGPU {
		t.Errorf("region state = %s, want MIGRATING_TO_GPU", region.State)
	}
	if region.LastMigrationTime == 42 {
		t.Error("recency not updated by the admission transition")
	}

	// Claim + block: the BlockRange barrier is issued before any DMA.
	if !mw.Tick() {
		t.Fatal("tick did not claim and block")
	}
	if tx.phase != migrationPhaseBlocking {
		t.Fatalf("phase = %v, want blocking", tx.phase)
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 BlockRange", len(reqs))
	}
	block, ok := reqs[0].(*vm.BlockRange)
	if !ok {
		t.Fatalf("request = %T, want BlockRange", reqs[0])
	}
	if block.CommandID != tx.Ticket {
		t.Errorf("block commandID = %d, want ticket %d", block.CommandID, tx.Ticket)
	}
	if block.StartVA != 0 || block.Size != 64*mem.KB {
		t.Errorf("block range = %#x+%d, want 0+64KB", block.StartVA, block.Size)
	}

	// The transaction waits for the Todo-8 watermark completion before DMA.
	if mw.Tick() {
		t.Error("transaction progressed without the block completion")
	}
	if len(d.requestsToSend) != 0 {
		t.Error("DMA issued before the block completion")
	}

	// Block completion -> H2D of the region's 15 valid pages as one maximal
	// run (contiguous CPU backing + contiguous allocated frames).
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-block requests = %d, want 1 DMA", len(reqs))
	}
	h2d, ok := reqs[0].(*protocol.MemCopyH2DReq)
	if !ok {
		t.Fatalf("post-block request = %T, want MemCopyH2DReq", reqs[0])
	}
	if tx.phase != migrationPhaseMigrating {
		t.Fatalf("phase = %v, want migrating", tx.phase)
	}
	if len(h2d.SrcBuffer) != 15*basePageSize {
		t.Errorf("DMA payload = %d bytes, want 60KB", len(h2d.SrcBuffer))
	}
	if !bytes.Equal(h2d.SrcBuffer, cpuData) {
		t.Error("DMA payload mismatch")
	}
	if pte, found := gpuTables[0].Find(pid, uint64(ptr)); found &&
		pte.Location == vm.MemoryLocationGPU_LOCAL {
		t.Error("PTE published before the DMA completed")
	}

	// DMA completion -> PTE publication (REMOTE -> GPU_LOCAL) + mandatory
	// 64 KB TLB invalidate AFTER the PTE update (§21.2/§21.3).
	deliverGeneralRsp(t, d, h2d)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbReq, ok := reqs[0].(*protocol.UVMTLBInvalidateReq)
	if !ok {
		t.Fatalf("post-DMA request = %T, want UVMTLBInvalidateReq", reqs[0])
	}
	if tlbReq.StartVA != 0 || tlbReq.Size != 64*mem.KB {
		t.Errorf("TLB invalidate = %#x+%d, want 0+64KB", tlbReq.StartVA, tlbReq.Size)
	}
	for page := uint64(0); page < 15; page++ {
		pte, found := gpuTables[0].Find(pid, reg.Base+page*basePageSize)
		if !found || pte.Location != vm.MemoryLocationGPU_LOCAL {
			t.Errorf("page %d PTE = %+v/%v, want GPU_LOCAL", page, pte, found)
		}
		if !maskBit(reg.ResidentMask, page) {
			t.Errorf("page %d not resident after migration", page)
		}
		if maskBit(reg.InFlightMask, page) {
			t.Errorf("page %d still in flight after migration", page)
		}
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 15*basePageSize {
		t.Errorf("reserved N = %d, want 60KB (commit only at admission completion)",
			got)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 0 {
		t.Errorf("resident R = %d, want 0 before the TLB ack", got)
	}
	if region.State != RegionMigratingToGPU {
		t.Errorf("region state = %s, want MIGRATING_TO_GPU before TLB ack",
			region.State)
	}

	// TLB ack -> GPU_RESIDENT (recency updated on admission) + replay.
	deliverTLBAck(t, d, tlbReq)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replay, ok := reqs[0].(*protocol.UVMFaultReplayReq)
	if !ok {
		t.Fatalf("post-TLB request = %T, want UVMFaultReplayReq", reqs[0])
	}
	if replay.StartVA != 0 || replay.Size != 64*mem.KB {
		t.Errorf("replay = %#x+%d, want 0+64KB", replay.StartVA, replay.Size)
	}
	if replay.ReplayToken != tx.ReplayToken {
		t.Errorf("replay token = %d, want %d", replay.ReplayToken, tx.ReplayToken)
	}
	if region.State != RegionGPUResident {
		t.Errorf("region state = %s, want GPU_RESIDENT after TLB ack",
			region.State)
	}
	if got := d.uvm.Reservation().ReservedBytes(); got != 0 {
		t.Errorf("reserved N = %d, want 0 after commit", got)
	}
	if got := d.uvm.Reservation().ResidentBytes(); got != 15*basePageSize {
		t.Errorf("resident R = %d, want 60KB", got)
	}
	if region.LastMigrationTime == 42 {
		t.Error("recency not updated by the admission completion")
	}

	// Replay ack -> unblock; the ownership slot is released before the
	// unblock completes.
	deliverReplayAck(t, d, replay)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-replay requests = %d, want 1 UnblockRange", len(reqs))
	}
	unblock, ok := reqs[0].(*vm.UnblockRange)
	if !ok {
		t.Fatalf("post-replay request = %T, want UnblockRange", reqs[0])
	}
	if unblock.CommandID != tx.Ticket {
		t.Errorf("unblock commandID = %d, want ticket %d", unblock.CommandID, tx.Ticket)
	}
	if tx.phase != migrationPhaseUnblocking {
		t.Fatalf("phase = %v, want unblocking", tx.phase)
	}
	key := copyRegionKey{PID: pid, GPU: 1, RegionBase: 0}
	if typ, owner := d.uvm.OwnerOf(key); typ != OwnershipIdle {
		t.Errorf("owner = %v/%d, want IDLE before unblock completes", typ, owner)
	}

	// Unblock ack -> done: coalescing entry removed, tickets woken.
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()
	if tx.phase != migrationPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	if mw.active != nil {
		t.Error("transaction not retired after completion")
	}
	if d.uvm.migrationByKey[key] != nil {
		t.Error("coalescing entry not removed after completion")
	}
	if got := d.uvm.AccessCounterMigrationCount(); got != 1 {
		t.Errorf("AC migration count = %d, want 1", got)
	}
	if got := d.uvm.SuppressedMigrationCount(); got != 0 {
		t.Errorf("suppressed count = %d, want 0", got)
	}
}

// TestUVMRemoteWriteMigration drives the write-triggered migration: the
// parked remote write is never committed to host (§15) — the transaction only
// issues H2D DMA, never D2H, and the CPU backing bytes stay unchanged; the
// write-trigger counter is exact.
func TestUVMRemoteWriteMigration(t *testing.T) {
	d, mw, gpuTables := buildMigrationDriver(t)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)
	reg := d.uvm.registrations[0]

	cpuBefore := make([]byte, 15*basePageSize)
	for i := 0; i < 15; i++ {
		data, err := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		if err != nil {
			t.Fatal(err)
		}
		copy(cpuBefore[i*basePageSize:(i+1)*basePageSize], data)
	}

	// Remote-write trigger -> one write migration.
	intakeRemoteWrite(t, d, pid, 1, uint64(ptr))
	if mw.active == nil {
		t.Fatal("no migration transaction after the write trigger")
	}
	tx := mw.active
	if tx.Trigger != migrationTriggerRemoteWrite {
		t.Errorf("trigger = %v, want remote write", tx.Trigger)
	}
	if got := d.uvm.RemoteWriteMigrationCount(); got != 1 {
		t.Errorf("write migration count = %d, want 1", got)
	}
	if got := d.uvm.AccessCounterMigrationCount(); got != 0 {
		t.Errorf("AC migration count = %d, want 0", got)
	}

	// Complete the full transaction, collecting every issued request.
	var all []sim.Msg
	collect := func() {
		all = append(all, drainRequests(d)...)
	}
	mw.Tick()
	collect()
	deliverGeneralRsp(t, d, all[0]) // block ack
	mw.Tick()
	collect()
	deliverGeneralRsp(t, d, all[1]) // DMA ack
	mw.Tick()
	collect()
	deliverTLBAck(t, d, all[2].(*protocol.UVMTLBInvalidateReq))
	mw.Tick()
	collect()
	deliverReplayAck(t, d, all[3].(*protocol.UVMFaultReplayReq))
	mw.Tick()
	collect()
	deliverGeneralRsp(t, d, all[4]) // unblock ack
	mw.Tick()

	if tx.phase != migrationPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
	if region := reg.VABlocks[0].SubBlocks[0]; region.State != RegionGPUResident {
		t.Errorf("region state = %s, want GPU_RESIDENT", region.State)
	}
	if pte, found := gpuTables[0].Find(pid, uint64(ptr)); !found ||
		pte.Location != vm.MemoryLocationGPU_LOCAL {
		t.Errorf("PTE = %+v/%v, want GPU_LOCAL", pte, found)
	}

	// No host write: the migration never issues D2H and never mutates the
	// CPU backing bytes.
	for _, req := range all {
		if _, ok := req.(*protocol.MemCopyD2HReq); ok {
			t.Error("migration issued a D2H request (host write path)")
		}
	}
	for i := 0; i < 15; i++ {
		data, err := d.globalStorage.Read(reg.CPUBackingPages[i], basePageSize)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, cpuBefore[i*basePageSize:(i+1)*basePageSize]) {
			t.Errorf("CPU backing page %d changed: the migration wrote to host", i)
		}
	}
}

// TestUVMAdmissionBlockBarrier proves the block barrier precedes any H2D: the
// BlockRange is the first request, the transaction never issues DMA before
// the watermark completion, and a notification while the barrier is up is
// suppressed without a duplicate transaction or DMA.
func TestUVMAdmissionBlockBarrier(t *testing.T) {
	d, mw, _ := buildMigrationDriver(t)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)

	intakeNotification(t, d, pid, 1, uint64(ptr))
	tx := mw.active

	// Claim + block: the BlockRange is the FIRST request; no DMA precedes it.
	if !mw.Tick() {
		t.Fatal("tick did not claim")
	}
	reqs := drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want exactly 1 BlockRange before any DMA",
			len(reqs))
	}
	if _, ok := reqs[0].(*vm.BlockRange); !ok {
		t.Fatalf("first request = %T, want BlockRange", reqs[0])
	}

	// Without the block completion the transaction never issues DMA: the
	// post-barrier access stays parked behind the barrier.
	if mw.Tick() {
		t.Error("transaction progressed without the block completion")
	}
	if got := len(d.requestsToSend); got != 0 {
		t.Errorf("requests queued without the block ack = %d, want 0", got)
	}
	if tx.phase != migrationPhaseBlocking {
		t.Errorf("phase = %v, want blocking", tx.phase)
	}

	// A second notification while the barrier is up is suppressed: no
	// duplicate transaction, no duplicate DMA.
	intakeNotification(t, d, pid, 1, uint64(ptr))
	if mw.active != tx {
		t.Error("duplicate notification replaced the active transaction")
	}
	if got := d.uvm.AccessCounterMigrationCount(); got != 1 {
		t.Errorf("AC migration count = %d, want 1 (no duplicate)", got)
	}
	if got := d.uvm.SuppressedMigrationCount(); got != 1 {
		t.Errorf("suppressed count = %d, want 1", got)
	}

	// Block completion releases the barrier: DMA follows.
	deliverGeneralRsp(t, d, reqs[0])
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-block requests = %d, want 1 DMA", len(reqs))
	}
	if _, ok := reqs[0].(*protocol.MemCopyH2DReq); !ok {
		t.Fatalf("post-block request = %T, want MemCopyH2DReq", reqs[0])
	}
}

// TestUVMExistingRemoteRead proves an old remote read completes concurrently
// with the migration: its completion is not consumed by the transaction and
// does not disturb the H2D/PTE/TLB/replay/unblock progression.
func TestUVMExistingRemoteRead(t *testing.T) {
	d, mw, _ := buildMigrationDriver(t)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)

	intakeNotification(t, d, pid, 1, uint64(ptr))
	tx := mw.active
	mw.Tick()
	reqs := drainRequests(d)
	block := reqs[0].(*vm.BlockRange)

	// The gate's watermark completion implies every <=watermark old-remote
	// read committed before the ack; the migration proceeds to H2D.
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	reqs = drainRequests(d)
	h2d := reqs[0].(*protocol.MemCopyH2DReq)
	if tx.phase != migrationPhaseMigrating {
		t.Fatalf("phase = %v, want migrating", tx.phase)
	}

	// An old remote read completes concurrently with the H2D: its completion
	// (an unrelated in-flight request's completion) is not consumed by the
	// migration and does not disturb it.
	stray := &vm.BlockRange{CommandID: 999, PID: pid, StartVA: 0, Size: 64 * mem.KB}
	stray.ID = sim.GetIDGenerator().Generate()
	stray.Src = d.gpuPort.AsRemote()
	stray.Dst = d.gpuPort.AsRemote()
	deliverGeneralRsp(t, d, stray)
	mw.Tick()
	if tx.phase != migrationPhaseMigrating {
		t.Errorf("phase = %v, want migrating (read completion must not disturb)",
			tx.phase)
	}
	if d.gpuPort.PeekIncoming() == nil {
		t.Error("the old remote read's completion was consumed by the migration")
	}
	d.gpuPort.RetrieveIncoming()

	// The migration's own DMA completion drives it forward.
	deliverGeneralRsp(t, d, h2d)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbReq := reqs[0].(*protocol.UVMTLBInvalidateReq)
	deliverTLBAck(t, d, tlbReq)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replay := reqs[0].(*protocol.UVMFaultReplayReq)
	deliverReplayAck(t, d, replay)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-replay requests = %d, want 1 unblock", len(reqs))
	}
	unblock := reqs[0].(*vm.UnblockRange)
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()

	if tx.phase != migrationPhaseDone {
		t.Errorf("phase = %v, want done", tx.phase)
	}
}

// TestUVMNotificationCoalescing proves a notification during active work is
// ignored (§16): no duplicate transaction, no duplicate DMA, and the active
// transaction completes with exactly one DMA/TLB/replay/unblock.
func TestUVMNotificationCoalescing(t *testing.T) {
	d, mw, _ := buildMigrationDriver(t)
	ctx := d.Init()
	pid := ctx.pid
	ptr := d.AllocateManagedMemory(ctx, 64*mem.KB)

	// Threshold notification -> transaction.
	intakeNotification(t, d, pid, 1, uint64(ptr))
	if got := d.uvm.AccessCounterMigrationCount(); got != 1 {
		t.Errorf("AC migration count = %d, want 1", got)
	}
	mw.Tick()
	reqs := drainRequests(d)
	block := reqs[0].(*vm.BlockRange)
	deliverGeneralRsp(t, d, block)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("DMA reqs = %d, want 1", len(reqs))
	}
	h2d := reqs[0].(*protocol.MemCopyH2DReq)

	// A notification during active work (migrating) is ignored: no duplicate
	// transaction, no duplicate DMA.
	intakeNotification(t, d, pid, 1, uint64(ptr))
	if got := d.uvm.AccessCounterMigrationCount(); got != 1 {
		t.Errorf("AC migration count = %d, want 1 (no duplicate)", got)
	}
	if got := d.uvm.SuppressedMigrationCount(); got != 1 {
		t.Errorf("suppressed count = %d, want 1", got)
	}
	if got := len(d.requestsToSend); got != 0 {
		t.Errorf("requests queued by the duplicate notification = %d, want 0", got)
	}
	if mw.active == nil {
		t.Fatal("duplicate notification retired the active transaction")
	}

	// Complete the transaction: exactly one DMA, one TLB, one replay, one
	// unblock — no duplicate work.
	deliverGeneralRsp(t, d, h2d)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-DMA requests = %d, want 1 TLB invalidate", len(reqs))
	}
	tlbReq := reqs[0].(*protocol.UVMTLBInvalidateReq)
	deliverTLBAck(t, d, tlbReq)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-TLB requests = %d, want 1 replay", len(reqs))
	}
	replay := reqs[0].(*protocol.UVMFaultReplayReq)
	deliverReplayAck(t, d, replay)
	mw.Tick()
	reqs = drainRequests(d)
	if len(reqs) != 1 {
		t.Fatalf("post-replay requests = %d, want 1 unblock", len(reqs))
	}
	unblock := reqs[0].(*vm.UnblockRange)
	deliverGeneralRsp(t, d, unblock)
	mw.Tick()

	if mw.active != nil {
		t.Error("transaction not retired after completion")
	}
	if got := d.uvm.SuppressedMigrationCount(); got != 1 {
		t.Errorf("suppressed count = %d, want 1", got)
	}
	if got := d.uvm.AccessCounterMigrationCount(); got != 1 {
		t.Errorf("AC migration count = %d, want 1", got)
	}
}