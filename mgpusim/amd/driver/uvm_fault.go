package driver

// allow: SIZE_OK - cohesive UVM fault-service state machine. // sbin_codex

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
)

// registerManagedAllocation records the residency metadata for a managed
// allocation produced by the allocator and installs the initial GPU mapping.
//
// Per spec 7.1 the initial GPU PTE is REMOTE when access-counter mode is on
// and INVALID otherwise. Installing REMOTE eagerly is what makes a cold read
// a counted remote access instead of a demand fault. // sbin_codex
func (m *UVMManager) registerManagedAllocation(
	pid vm.PID,
	res internal.ManagedAllocationResult,
) {
	m.stateMu.Lock() // sbin_codex: allocation can overlap parallel simulation events.
	defer m.stateMu.Unlock()

	cfg := m.config
	alloc := &ManagedAllocation{
		ID:        m.newID("alloc"),
		PID:       pid,
		Base:      res.Base,
		Size:      res.Size,
		PageBase:  cfg.alignDown(res.Base, cfg.PageSize),
		PageCount: res.PageCount,
	}
	m.allocations[alloc.ID] = alloc
	m.totalManagedBytes += res.PageCount * cfg.PageSize
	m.resolveRelativeCapacity()

	for i := uint64(0); i < res.PageCount; i++ {
		vAddr := res.Base + i*cfg.PageSize
		m.registerManagedPage(pid, vAddr, alloc, res.CPUBackingPages[i])
	}
}

// resolveRelativeCapacity derives the UVM GPU capacity from the managed
// allocation footprint when an oversubscription ratio is configured (spec 20).
//
// The numerator is what the benchmark allocated through AllocateManaged, not
// the set of pages it actually touches, and the denominator is the UVM
// capacity budget rather than the GPU's physical memory. It is recomputed as
// allocations arrive, which is safe because every managed buffer is allocated
// before the first kernel launches. // sbin_codex
func (m *UVMManager) resolveRelativeCapacity() {
	if m.config.OversubscriptionRatio <= 0 {
		return
	}

	capacity := uint64(
		float64(m.totalManagedBytes) / m.config.OversubscriptionRatio)

	capacity = capacity / m.config.RegionSize * m.config.RegionSize
	if capacity < m.config.RegionSize {
		capacity = m.config.RegionSize
	}

	m.config.GPUCapacityBytes = capacity
}

func (m *UVMManager) registerManagedPage(
	pid vm.PID,
	vAddr uint64,
	alloc *ManagedAllocation,
	cpuBacking uint64,
) {
	cfg := m.config
	pageKey := PageKey{PID: pid, VAddr: vAddr}
	regionBase := cfg.alignDown(vAddr, cfg.RegionSize)
	blockBase := cfg.alignDown(vAddr, cfg.VABlockSize)

	managedPage := &ManagedPage{
		Key:             pageKey,
		AllocationID:    alloc.ID,
		CPUBackingPAddr: cpuBacking,
		State:           CPUResident,
		RegionBase:      regionBase,
		VABlockBase:     blockBase,
	}
	m.pages[pageKey] = managedPage

	blockKey := BlockKey{PID: pid, Base: blockBase}

	block := m.blocks[blockKey]
	if block == nil {
		block = &VABlock{
			Key:          blockKey,
			AllocationID: alloc.ID,
			Regions:      make([]*RegionState, cfg.regionsPerBlock()),
		}
		m.blocks[blockKey] = block
	}

	regionIndex := (regionBase - blockBase) / cfg.RegionSize
	regionKey := RegionKey{PID: pid, Base: regionBase, DeviceID: uvmDeviceID}

	region := block.Regions[regionIndex]
	if region == nil {
		region = &RegionState{Key: regionKey, Phase: RegionIdle}
		block.Regions[regionIndex] = region
		m.regions[regionKey] = region
	}

	region.Pages = append(region.Pages, pageKey)

	// Pre-edit code (commented per project convention):
	// if m.config.AccessCounterEnabled {
	// 	m.installRemotePTE(managedPage)
	// }
	//
	// sbin_claude_uvm: lazy mode leaves the page INVALID so that the first GPU
	// access faults, and lets that fault publish the REMOTE mapping.
	if m.config.AccessCounterEnabled && !m.config.LazyRemotePTE {
		m.installRemotePTE(managedPage)
	}
}

// installRemotePTE publishes a CPU-remote mapping the GPU may use over PCIe
// without migrating. It never implies residency on the GPU.
func (m *UVMManager) installRemotePTE(managedPage *ManagedPage) {
	managedPage.RemoteMapped = true
	m.d.memAllocator.UpdatePage(vm.Page{
		PID:              managedPage.Key.PID,
		VAddr:            managedPage.Key.VAddr,
		PAddr:            managedPage.CPUBackingPAddr,
		PageSize:         m.config.PageSize,
		Valid:            true,
		DeviceID:         0,
		Unified:          false,
		Managed:          true,
		IsMigrating:      false,
		RemoteAccessible: true,
	})
	m.stats.RemotePTEInstalls++
}

// tryLazyRemoteMapLocked answers a cold-region fault by publishing the
// region's REMOTE mappings instead of opening a migration service.
//
// With LazyRemotePTE a managed page starts INVALID rather than REMOTE, so the
// first access to a 64KB region faults. A read fault is not a demand for
// residency: the driver only publishes the CPU-remote mapping the eager path
// would have published at allocation time, and the access counter keeps sole
// responsibility for deciding when the region migrates (spec 7.1, 15). The
// fixed software latency is charged once, the same way a fault service is.
//
// A region whose first touch is a WRITE is migrated on the spot instead. A
// remote write is never committed to host memory (spec 15): the access counter
// stalls it and asks for the very migration this fault could have started. So
// publishing REMOTE for it would buy a guaranteed-wasted round trip - fault,
// install, replay, stall, notify, service - to reach the same place. Handing
// the fault back to the ordinary path makes it a demand migration, and because
// no page was ever REMOTE-mapped the transition is INVALID -> GPU_LOCAL, which
// needs no TLB invalidation either (spec 2.1 transition table).
//
// Only the region's first fault decides. Once an install is pending, a write
// arriving behind it rides the same replay and then takes the ordinary
// REMOTE-write stall - which also keeps the install from racing a migration
// for the same pages.
//
// The install is region-scoped because the fault-service granularity is, and
// because the replay that releases the stalled translations names a 64KB range
// (spec 8.3): mapping only the faulting 4KB page would leave the other fifteen
// to re-fault behind that same replay.
//
// It reports whether it took ownership of the fault. // sbin_claude_uvm
func (m *UVMManager) tryLazyRemoteMapLocked(key RegionKey, isWrite bool) bool {
	if !m.config.AccessCounterEnabled || !m.config.LazyRemotePTE {
		return false
	}

	region := m.regions[key]
	if region == nil {
		return false
	}

	// A region a fault service or a migration already owns is on its way to
	// the GPU. Publishing a REMOTE mapping now would race the local one, so
	// the fault takes the ordinary path and joins that work instead.
	if region.busy() || region.MigrationID != "" || region.FaultID != "" {
		return false
	}

	if m.pendingRemoteMaps[key] {
		// The install is already scheduled; this fault rides its replay.
		m.stats.CoalescedFaults++

		return true
	}

	// First touch is a write: migrate now rather than map remotely.
	if isWrite {
		return false
	}

	if !m.regionNeedsRemoteMapLocked(region) {
		return false
	}

	m.pendingRemoteMaps[key] = true

	now := m.d.TickScheduler.CurrentTime()
	readyAt := now

	if cycles := m.config.faultHandlingCycles(); cycles > 0 {
		readyAt = m.config.GPUCoreFrequency.NCyclesLater(cycles, now)
		m.stats.FaultHandlingTime += readyAt - now
	}

	m.d.Engine.Schedule(newRemoteMapCompleteEvent(readyAt, m.d, key))

	return true
}

// regionNeedsRemoteMapLocked reports whether any page of the region is still a
// cold CPU-resident page with no mapping published. // sbin_claude_uvm
func (m *UVMManager) regionNeedsRemoteMapLocked(region *RegionState) bool {
	for _, pk := range region.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil {
			continue
		}

		if managedPage.State == CPUResident && !managedPage.RemoteMapped {
			return true
		}
	}

	return false
}

// completeRemoteMap publishes the REMOTE mappings of one region and replays the
// translations that were stalled on it.
//
// The region keeps its RegionIdle phase throughout: nothing was reserved, no
// frame was taken, and no service slot was held, so there is nothing to
// release. // sbin_claude_uvm
func (m *UVMManager) completeRemoteMap(key RegionKey) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	if !m.pendingRemoteMaps[key] {
		return
	}

	delete(m.pendingRemoteMaps, key)

	region := m.regions[key]
	if region == nil {
		m.sendFaultReplayLocked(key)

		return
	}

	for _, pk := range region.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil {
			continue
		}

		// A page that became GPU-resident while the install was in flight
		// already holds the better mapping; REMOTE must not overwrite it.
		if managedPage.State != CPUResident || managedPage.RemoteMapped {
			continue
		}

		m.installRemotePTE(managedPage)
	}

	m.stats.LazyRemoteMaps++

	m.sendFaultReplayLocked(key)
}

// onPageFault ingests one 4KB fault request from a GPU GMMU.
//
// Faults are coalesced by the aligned 64KB fault-service region (spec 8.3). A
// duplicate fault joins the outstanding transaction and never incurs another
// fixed software delay. // sbin_codex
func (m *UVMManager) onPageFault(
	pid vm.PID,
	vAddr uint64,
	deviceID uint64,
	isWrite bool,
) {
	m.stateMu.Lock() // sbin_codex: serialize fault ingestion with migration completion.
	defer m.stateMu.Unlock()

	cfg := m.config
	regionBase := cfg.alignDown(vAddr, cfg.RegionSize)
	key := RegionKey{PID: pid, Base: regionBase, DeviceID: deviceID}

	m.stats.RawPageFaults++

	pageBase := cfg.alignDown(vAddr, cfg.PageSize)

	if txn, found := m.faults[key]; found {
		txn.RawFaults++
		txn.IsWrite = txn.IsWrite || isWrite
		m.promoteToDemandFaultLocked(txn)
		txn.demandVAddrs[pageBase] = true
		m.stats.CoalescedFaults++

		return
	}

	// sbin_claude_uvm: under LazyRemotePTE a cold region's first read is
	// answered by publishing its REMOTE mappings rather than by migrating it.
	// A first-touch write falls through to the ordinary demand path.
	if m.tryLazyRemoteMapLocked(key, isWrite) {
		return
	}

	txn := &FaultTransaction{
		ID:           m.newID("fault"),
		Key:          key,
		Trigger:      TriggerFault,
		VABlockBase:  cfg.alignDown(vAddr, cfg.VABlockSize),
		CreatedAt:    m.d.TickScheduler.CurrentTime(),
		State:        FaultPending,
		RawFaults:    1,
		IsWrite:      isWrite,
		demandVAddrs: map[uint64]bool{pageBase: true},
	}
	m.faults[key] = txn
	m.faultsByID[txn.ID] = txn
	m.faultServiceCue = append(m.faultServiceCue, txn.ID)
	m.stats.UniqueFaultServices++

	m.startNextFaultServiceLocked()
}

// promoteToDemandFaultLocked re-labels a queued access-counter service as a
// demand-fault service.
//
// A real fault outranks the counter's hint for the same region: it has a
// waiting request behind it. Both kinds run the same service, so this only
// moves the accounting, and spec 10.1 still charges the region once. It also
// drops the counter's demand seed: that seed stands in for a demand mask the
// counter cannot express, and a real fault can, at the 4KB granularity spec
// 11.7 asks for. Once the service has started there is nothing left to
// promote — the work is already under way and the fault simply rides it.
// sbin_codex
func (m *UVMManager) promoteToDemandFaultLocked(txn *FaultTransaction) {
	if txn.Trigger != TriggerAccessCounter || txn.State != FaultPending {
		return
	}

	txn.Trigger = TriggerFault
	txn.demandVAddrs = map[uint64]bool{}

	if m.stats.AccessCounterServices > 0 {
		m.stats.AccessCounterServices--
	}

	m.stats.UniqueFaultServices++
}

// parkFaultLocked suspends a transaction that cannot be serviced yet and
// releases its claim on the region.
//
// Releasing the claim matters: a region still marked as owned by a fault is
// never eligible for eviction, so a transaction waiting for capacity would
// keep alive the very condition that blocks it. // sbin_codex
func (m *UVMManager) parkFaultLocked(txn *FaultTransaction) {
	txn.State = FaultPending
	m.faultsDeferred[txn.Key] = append(m.faultsDeferred[txn.Key], txn.ID)

	if region := m.regions[txn.Key]; region != nil && region.FaultID == txn.ID {
		region.FaultID = ""

		if region.Phase != RegionEvicting {
			region.Phase = RegionIdle
		}
	}

	if m.activeFaultID == txn.ID {
		m.activeFaultID = ""
		m.startNextFaultServiceLocked()
	}
}

// resumeDeferredFaultsLocked re-admits the transactions that were parked
// behind an eviction of the region. // sbin_codex
func (m *UVMManager) resumeDeferredFaultsLocked(key RegionKey) {
	deferred := m.faultsDeferred[key]
	if len(deferred) == 0 {
		return
	}

	delete(m.faultsDeferred, key)

	m.faultServiceCue = append(m.faultServiceCue, deferred...)
	m.startNextFaultServiceLocked()
}

// startNextFaultServiceLocked admits the next queued transaction. Exactly one
// 64KB fault service is active at a time and the policy is FIFO by creation
// time (spec 8.4, 31). // sbin_codex
func (m *UVMManager) startNextFaultServiceLocked() {
	if m.activeFaultID != "" {
		return
	}

	for len(m.faultServiceCue) > 0 {
		id := m.faultServiceCue[0]
		m.faultServiceCue = m.faultServiceCue[1:]

		txn := m.faultsByID[id]
		if txn == nil {
			continue
		}

		m.activeFaultID = id
		m.scheduleFaultHandlingLocked(txn)

		return
	}
}

// scheduleFaultHandlingLocked charges the fixed software fault-handling
// latency exactly once per unique transaction, as one scheduled event rather
// than a cycle-by-cycle wait (spec 10.3).
//
// An access-counter service is charged the same way. The driver does the same
// work for it — capacity check, eviction, DMA, page-table update, replay — and
// spec 10 attaches the latency to that work rather than to the fault that may
// or may not have started it. Leaving it uncharged made the cost of admitting
// a region depend on whether the page happened to be REMOTE-mapped at the
// time. // sbin_codex
func (m *UVMManager) scheduleFaultHandlingLocked(txn *FaultTransaction) {
	now := m.d.TickScheduler.CurrentTime()
	readyAt := now

	if cycles := m.config.faultHandlingCycles(); cycles > 0 {
		readyAt = m.config.GPUCoreFrequency.NCyclesLater(cycles, now)
		m.stats.FaultHandlingTime += readyAt - now
	}

	txn.ReadyAt = readyAt

	if region := m.regions[txn.Key]; region != nil {
		region.FaultID = txn.ID

		// An eviction in progress owns the region; it must not be demoted to a
		// fault phase or the eviction would look idle and could be selected as
		// a victim a second time. // sbin_codex
		if region.Phase != RegionEvicting {
			region.Phase = RegionFaultPending
		}
	}

	m.d.Engine.Schedule(newFaultHandlingCompleteEvent(readyAt, m.d, txn.ID))
}

// handleFaultReady runs the service stage of the active transaction: TBN
// selection, capacity enforcement, and migration.
func (m *UVMManager) handleFaultReady(faultID string) {
	m.stateMu.Lock() // sbin_codex: fault-ready events mutate the shared UVM state.
	defer m.stateMu.Unlock()

	txn := m.faultsByID[faultID]
	if txn == nil || txn.State != FaultPending {
		return
	}

	txn.State = FaultHandling

	region := m.regions[txn.Key]

	// A region being evicted cannot be serviced yet: its pages are on their
	// way to host memory. Park the transaction until the eviction finishes and
	// let another region use the service slot meanwhile. // sbin_codex
	if region != nil && region.Phase == RegionEvicting {
		m.parkFaultLocked(txn)
		return
	}

	if region != nil {
		region.Phase = RegionFaultHandling
	}

	m.serviceFaultLocked(txn)
}

func (m *UVMManager) serviceFaultLocked(txn *FaultTransaction) {
	block := m.blocks[BlockKey{PID: txn.Key.PID, Base: txn.VABlockBase}]
	if block == nil {
		m.finishFaultLocked(txn)
		return
	}

	// A page already being brought in by another transaction is authoritative
	// for this region; join it rather than issuing a duplicate DMA (spec 11.8).
	if migID := m.migrationCoveringRegion(txn.Key); migID != "" {
		txn.MigrationID = migID
		txn.State = FaultMigrating
		mig := m.migrations[migID]
		mig.FaultIDs = append(mig.FaultIDs, txn.ID)

		return
	}

	sel := m.selectTBNRegion(txn.Key, block)
	txn.Selected = sel.pageKeys
	txn.DemandPages = sel.demandPages
	txn.PrefetchPages = sel.prefetchPages

	if len(sel.pageKeys) == 0 {
		// Everything the neighborhood covers is already resident.
		m.finishFaultLocked(txn)
		return
	}

	exclude := make(map[PageKey]bool, len(sel.pageKeys))
	for _, pk := range sel.pageKeys {
		exclude[pk] = true
	}

	m.withCapacity(uint64(len(sel.pageKeys)), exclude, func() {
		m.startCPUToGPUMigration(txn, sel.pageKeys, txn.Trigger)
	})
}

// migrationCoveringRegion returns the admission that already owns any page of
// the region, or the empty string. Only a CPU-to-GPU transfer can satisfy a
// fault; an eviction moves the region the other way. // sbin_codex
func (m *UVMManager) migrationCoveringRegion(key RegionKey) string {
	region := m.regions[key]
	if region == nil {
		return ""
	}

	for _, pk := range region.Pages {
		migID, found := m.migrationsByPage[pk]
		if !found {
			continue
		}

		if mig := m.migrations[migID]; mig != nil &&
			mig.Direction == CPUToGPU {
			return migID
		}
	}

	return ""
}

// finishFaultLocked makes the transaction replayable, releases the service
// slot, and admits the next queued transaction.
func (m *UVMManager) finishFaultLocked(txn *FaultTransaction) {
	if txn.State == FaultComplete {
		return
	}

	txn.State = FaultComplete

	m.sendFaultReplayLocked(txn.Key)
	m.retireFaultLocked(txn)
}

// retireFaultLocked releases the service slot without emitting a replay. The
// caller is responsible for having told the GPU what happened to the region.
// sbin_codex
func (m *UVMManager) retireFaultLocked(txn *FaultTransaction) {
	if region := m.regions[txn.Key]; region != nil && region.FaultID == txn.ID {
		region.FaultID = ""

		if region.Phase != RegionEvicting {
			region.Phase = RegionIdle
		}
	}

	delete(m.faults, txn.Key)
	delete(m.faultsByID, txn.ID)

	if m.activeFaultID == txn.ID {
		m.activeFaultID = ""
		m.startNextFaultServiceLocked()
	}
}

// hasPendingWorkInRange reports whether any outstanding fault or migration
// covers a page within [start, start+size). The D2H middleware waits until
// this returns false before reading managed buffers, so flush-triggered
// write-backs and their migrations complete first.
func (m *UVMManager) hasPendingWorkInRange(pid vm.PID, start, size uint64) bool {
	m.stateMu.RLock() // sbin_codex: D2H polling races with migration completion.
	defer m.stateMu.RUnlock()

	cfg := m.config
	begin := cfg.alignDown(start, cfg.PageSize)
	end := cfg.alignDown(start+size-1, cfg.PageSize)

	for vAddr := begin; vAddr <= end; vAddr += cfg.PageSize {
		if _, found := m.migrationsByPage[PageKey{PID: pid, VAddr: vAddr}]; found {
			return true
		}
	}

	for key := range m.faults {
		if key.PID != pid {
			continue
		}

		if key.Base+cfg.RegionSize > begin && key.Base <= end {
			return true
		}
	}

	return len(m.controlOps) > 0
}
