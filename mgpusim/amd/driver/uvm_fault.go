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

	if m.config.AccessCounterEnabled {
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
		txn.demandVAddrs[pageBase] = true
		m.stats.CoalescedFaults++

		return
	}

	txn := &FaultTransaction{
		ID:           m.newID("fault"),
		Key:          key,
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
		m.startCPUToGPUMigration(txn, sel.pageKeys, TriggerFault)
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
