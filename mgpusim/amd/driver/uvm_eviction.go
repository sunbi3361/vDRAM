package driver

// sbin_codex: oversubscription handling and the region-scoped eviction state
// machine (spec 18, 19).
//
// The eviction unit is one 64KB region. The sequence is
//
//	select LRU victim -> mark EVICTING -> 64KB cache WB+INV
//	 -> park PTEs -> 64KB TLB invalidate -> D2H DMA
//	 -> install final REMOTE/INVALID PTE -> free frames -> unblock
//
// No step flushes a whole cache or TLB and no step restarts the GPU.

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// addLRU inserts a region into the migration-recency list once it becomes
// GPU-resident.
func (m *UVMManager) addLRU(key RegionKey) {
	if _, found := m.lruMap[key]; found {
		m.lru.MoveToFront(m.lruMap[key])
		return
	}

	m.lruMap[key] = m.lru.PushFront(key)
}

// removeLRU takes a region out of the eviction candidate set.
func (m *UVMManager) removeLRU(key RegionKey) {
	if elem, found := m.lruMap[key]; found {
		m.lru.Remove(elem)
		delete(m.lruMap, key)
	}
}

// withCapacity runs migrate once the GPU has room for requiredFrames. When it
// does not, LRU victims are evicted first and migrate resumes afterwards.
func (m *UVMManager) withCapacity(
	required uint64,
	exclude map[PageKey]bool,
	migrate func(),
) {
	m.withCapacityFrom(m.d.TickScheduler.CurrentTime(), required, exclude, migrate)
}

// withCapacityFrom carries the time the admission first had to wait, so the
// stall a migration suffered for capacity can be reported (spec 17.1).
func (m *UVMManager) withCapacityFrom(
	waitStart sim.VTimeInSec,
	required uint64,
	exclude map[PageKey]bool,
	migrate func(),
) {
	if m.freeGPUFrames() >= required || m.config.EvictionDisabled {
		m.stats.MigrationWaitForCapacity +=
			m.d.TickScheduler.CurrentTime() - waitStart
		migrate()

		return
	}

	victims := m.selectLRUVictims(required, exclude)
	if len(victims) == 0 {
		// Nothing is eligible right now; admit whatever fits so the fault can
		// still make progress.
		m.stats.MigrationWaitForCapacity +=
			m.d.TickScheduler.CurrentTime() - waitStart
		migrate()

		return
	}

	m.evictVictimsThen(victims, func() {
		m.withCapacityFrom(waitStart, required, exclude, migrate)
	})
}

// preEvict keeps a 64KB free-capacity headroom ahead of the next admission
// (spec 17.1). It runs after a migration has already reserved its frames, so
// the pre-eviction D2H transfers overlap that migration's H2D transfer instead
// of serializing behind it. Several victims may be in flight at once and there
// is no UVM-side queue-depth limit.
func (m *UVMManager) preEvict(exclude map[PageKey]bool) {
	if m.config.EvictionDisabled {
		return
	}

	headroom := m.config.RegionSize / m.config.PageSize

	projected := m.freeGPUFrames() + m.evictingFrames
	if projected >= headroom {
		return
	}

	victims := m.selectLRUVictims(headroom-m.evictingFrames, exclude)

	for _, region := range victims {
		m.stats.PreEvictions++
		m.stats.PreEvictedBytes += m.regionGPUFrames(region) * m.config.PageSize

		if m.hasActiveH2D() {
			m.stats.PreEvictionsOverlappedH2D++
		}

		m.beginEviction(region, func() {})
	}
}

// hasActiveH2D reports whether an admission is currently transferring.
func (m *UVMManager) hasActiveH2D() bool {
	for _, mig := range m.migrations {
		if mig.Direction == CPUToGPU {
			return true
		}
	}

	return false
}

// evictVictimsThen evicts the selected regions one after another and resumes
// done when the last one has released its frames.
func (m *UVMManager) evictVictimsThen(victims []*RegionState, done func()) {
	remaining := append([]*RegionState(nil), victims...)

	var step func()

	step = func() {
		if len(remaining) == 0 {
			done()
			return
		}

		region := remaining[0]
		remaining = remaining[1:]

		m.beginEviction(region, step)
	}

	step()
}

// beginEviction starts the region-scoped eviction sequence.
//
// Victims are chosen up front but evicted one at a time through asynchronous
// control operations, so a region can become the target of an admission
// between selection and its turn. Eligibility is therefore re-checked here;
// evicting a region mid-admission would drop the incoming data. // sbin_codex
func (m *UVMManager) beginEviction(region *RegionState, done func()) {
	if !m.regionEligible(region, nil) || m.regionGPUFrames(region) == 0 {
		done()
		return
	}

	region.Phase = RegionEvicting
	m.removeLRU(region.Key)
	m.stats.Evictions++

	m.evictingFrames += m.regionGPUFrames(region)
	m.activeEvictions++

	if m.activeEvictions > m.stats.MaxConcurrentPreEvictions {
		m.stats.MaxConcurrentPreEvictions = m.activeEvictions
	}

	// Ordering note. Spec 19 lists cache WB+INV before the TLB invalidation
	// and relies on each cache refusing new matching transactions for the
	// duration. This model reaches the same guarantee from the translation
	// side instead: parking the PTEs and invalidating the 64KB range first
	// means no compute unit can obtain a GPU-local translation for the victim
	// any more, so the subsequent writeback observes a settled region and the
	// D2H copy cannot race a store that was issued afterwards. // sbin_codex
	dbgd("EVICT-BEGIN region=%#x frames=%d", region.Key.Base, m.regionGPUFrames(region))
	m.evictionParkPTEs(region)

	m.requestTLBInvalidateLocked(region.Key, func() {
		dbgd("EVICT-TLBINV-DONE region=%#x", region.Key.Base)
		m.requestCacheRangeFlushLocked(region.Key, func() {
			dbgd("EVICT-FLUSH-DONE region=%#x", region.Key.Base)
			m.evictionStartD2H(region, done)
		})
	})
}

// evictionParkPTEs removes GPU-local residency from the victim mapping before
// any data leaves the GPU. A page-table walk for the victim now faults and the
// request parks in the GMMU replay queue.
func (m *UVMManager) evictionParkPTEs(region *RegionState) {
	for _, pk := range region.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil || managedPage.State != GPUResident {
			continue
		}

		m.publishMigratingPTE(managedPage)
	}
}

// evictionStartD2H copies the victim back to host memory.
func (m *UVMManager) evictionStartD2H(region *RegionState, done func()) {
	pages := make([]PageKey, 0, len(region.Pages))

	for _, pk := range region.Pages {
		if managedPage := m.pages[pk]; managedPage != nil &&
			managedPage.State == GPUResident {
			pages = append(pages, pk)
		}
	}

	mig := m.newMigration(GPUToCPU, TriggerEviction, region.Key.DeviceID, pages)
	if mig == nil {
		m.finalizeEviction(region)
		done()

		return
	}

	m.evictionDone[mig.ID] = done
	m.beginTransfer(mig)
}

// completeEvictionMigrationLocked installs the final mapping of an evicted
// region and returns its frames to the UVM budget.
func (m *UVMManager) completeEvictionMigrationLocked(mig *Migration) {
	for _, key := range mig.RegionKeys {
		if region := m.regions[key]; region != nil {
			m.finalizeEviction(region)
		}
	}

	for _, pk := range mig.Pages {
		if m.migrationsByPage[pk] == mig.ID {
			delete(m.migrationsByPage, pk)
		}
	}

	delete(m.migrations, mig.ID)

	for _, faultID := range mig.FaultIDs {
		if txn := m.faultsByID[faultID]; txn != nil {
			m.finishFaultLocked(txn)
		}
	}

	done := m.evictionDone[mig.ID]
	delete(m.evictionDone, mig.ID)

	if done != nil {
		done()
	}
}

// finalizeEviction releases the GPU frames of a victim region and installs the
// mapping the access-counter mode calls for: REMOTE when remote access is
// enabled, INVALID otherwise (spec 19).
func (m *UVMManager) finalizeEviction(region *RegionState) {
	evicted := m.releaseEvictedPages(region)
	dbgd("EVICT-FINAL region=%#x evicted=%d", region.Key.Base, evicted)

	if m.activeEvictions > 0 {
		m.activeEvictions--
	}

	if evicted == 0 {
		region.Phase = RegionIdle
		m.resumeDeferredFaultsLocked(region.Key)

		return
	}

	region.Phase = RegionIdle
	region.Generation++
	region.MigrationID = ""

	m.resumeDeferredFaultsLocked(region.Key)

	m.removeLRU(region.Key)

	if m.gpuFramesInUse >= evicted {
		m.gpuFramesInUse -= evicted
	} else {
		m.gpuFramesInUse = 0
	}

	if m.evictingFrames >= evicted {
		m.evictingFrames -= evicted
	} else {
		m.evictingFrames = 0
	}

	m.stats.EvictedPages += evicted
	m.stats.EvictedBytes += evicted * m.config.PageSize

	if m.stats.GPUResidentPages >= evicted {
		m.stats.GPUResidentPages -= evicted
	}
}

// releaseEvictedPages returns the victim's frames and installs the mapping the
// access-counter mode calls for: REMOTE when remote access is enabled,
// INVALID otherwise.
func (m *UVMManager) releaseEvictedPages(region *RegionState) uint64 {
	var evicted uint64

	for _, pk := range region.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil || !managedPage.GPUFrameValid {
			continue
		}

		if managedPage.State != MigratingToCPU &&
			managedPage.State != GPUResident {
			continue
		}

		m.d.memAllocator.FreePhysicalPage(1, managedPage.GPUFramePAddr)
		managedPage.GPUFramePAddr = 0
		managedPage.GPUFrameValid = false
		managedPage.State = CPUResident
		managedPage.RemoteMapped = false

		if m.config.AccessCounterEnabled {
			m.installRemotePTE(managedPage)
		} else {
			m.installInvalidPTE(managedPage)
		}

		evicted++
	}

	return evicted
}

// installInvalidPTE publishes a mapping that faults on the next GPU access.
func (m *UVMManager) installInvalidPTE(managedPage *ManagedPage) {
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
		RemoteAccessible: false,
	})
}

// selectLRUVictims walks the migration-recency list from its oldest end and
// picks eligible 64KB regions until the requested frame count is covered.
func (m *UVMManager) selectLRUVictims(
	requiredFrames uint64,
	exclude map[PageKey]bool,
) []*RegionState {
	free := m.freeGPUFrames()
	if requiredFrames <= free {
		return nil
	}

	need := requiredFrames - free

	var (
		victims []*RegionState
		freed   uint64
	)

	for elem := m.lru.Back(); elem != nil; elem = elem.Prev() {
		key, ok := elem.Value.(RegionKey)
		if !ok {
			continue
		}

		region := m.regions[key]
		if region == nil || !m.regionEligible(region, exclude) {
			continue
		}

		frames := m.regionGPUFrames(region)
		if frames == 0 {
			continue
		}

		victims = append(victims, region)

		freed += frames
		if freed >= need {
			break
		}
	}

	return victims
}

// regionGPUFrames counts the GPU-resident pages of a region.
func (m *UVMManager) regionGPUFrames(region *RegionState) uint64 {
	var count uint64

	for _, pk := range region.Pages {
		if managedPage := m.pages[pk]; managedPage != nil &&
			managedPage.GPUFrameValid && managedPage.State == GPUResident {
			count++
		}
	}

	return count
}

// regionEligible enforces spec 18.2: a victim must be GPU resident, must not
// be migrating, and must not belong to an in-progress transaction.
func (m *UVMManager) regionEligible(
	region *RegionState,
	exclude map[PageKey]bool,
) bool {
	if region.busy() || region.MigrationID != "" || region.FaultID != "" {
		return false
	}

	for _, pk := range region.Pages {
		if exclude[pk] {
			return false
		}
	}

	return true
}
