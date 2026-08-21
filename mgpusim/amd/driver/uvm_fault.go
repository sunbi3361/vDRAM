package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
)

// registerManagedAllocation registers the residency metadata for a managed
// allocation produced by the allocator.
func (m *UVMManager) registerManagedAllocation(pid vm.PID, res internal.ManagedAllocationResult) {
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

	for i := uint64(0); i < res.PageCount; i++ {
		vAddr := res.Base + i*cfg.PageSize
		pageKey := PageKey{PID: pid, VAddr: vAddr}
		regionBase := cfg.alignDown(vAddr, cfg.RegionSize)
		blockBase := cfg.alignDown(vAddr, cfg.VABlockSize)

		mp := &ManagedPage{
			Key:             pageKey,
			AllocationID:    alloc.ID,
			CPUBackingPAddr: res.CPUBackingPages[i],
			State:           CPUResident,
			RegionBase:      regionBase,
			VABlockBase:     blockBase,
			TimesMigrated:   0,
		}
		m.pages[pageKey] = mp

		blockKey := BlockKey{PID: pid, Base: blockBase}
		block := m.blocks[blockKey]
		if block == nil {
			block = &VABlock{
				Key:          blockKey,
				AllocationID: alloc.ID,
				Regions:      make([]*RegionState, cfg.regionsPerBlock()),
				Activity:     make([]uint32, cfg.regionsPerBlock()),
			}
			m.blocks[blockKey] = block
		}
		regionIndex := (regionBase - blockBase) / cfg.RegionSize
		regionKey := RegionKey{PID: pid, Base: regionBase, DeviceID: 1}
		region := block.Regions[regionIndex]
		if region == nil {
			region = &RegionState{
				Key:        regionKey,
				LastAccess: 0,
			}
			block.Regions[regionIndex] = region
			m.regions[regionKey] = region
		}
		region.Pages = append(region.Pages, pageKey)
	}

	m.freeGPUFrames = m.resolveCapacity() / m.config.PageSize
}

func (m *UVMManager) computeFreeFrames() uint64 {
	return m.resolveCapacity() / m.config.PageSize
}

// onManagedAccess records a GPU access to a managed page. It is invoked from
// the GPU translation path. It increments the raw fault-request counter for
// CPU-resident (non-remotely-accessible) pages and, on first touch, creates
// the unique page fault and schedules fault handling. For remotely-accessible
// pages it increments the 64KB access counter.
func (m *UVMManager) onManagedAccess(
	pid vm.PID,
	vAddr uint64,
	deviceID uint64,
	requestID string,
	replyTo sim.RemotePort,
) {
	now := m.d.TickScheduler.CurrentTime()
	cfg := m.config
	pageKey := PageKey{PID: pid, VAddr: cfg.alignDown(vAddr, cfg.PageSize)}
	regionBase := cfg.alignDown(vAddr, cfg.RegionSize)

	mp := m.pages[pageKey]
	if mp == nil {
		return
	}

	// Update LRU recency for the containing region.
	if block := m.blocks[BlockKey{PID: pid, Base: cfg.alignDown(vAddr, cfg.VABlockSize)}]; block != nil {
		idx := (regionBase - block.Key.Base) / cfg.RegionSize
		if idx < uint64(len(block.Regions)) && block.Regions[idx] != nil {
			block.Regions[idx].LastAccess = now
			block.Activity[idx]++
		}
	}

	switch mp.State {
	case GPUResident:
		// The page is already GPU-resident (e.g. migrated by another fault's
		// TBN region). Reply to the waiting GMMU translation immediately.
		// sbin_codex: a GPU-local access refreshes the driver LRU recency.
		m.touchLRU(RegionKey{PID: pid, Base: regionBase, DeviceID: deviceID})
		if requestID != "" {
			m.replyFaultWaiter(FaultWaiter{
				RequestID: requestID,
				ReplyTo:   replyTo,
				DeviceID:  deviceID,
				PID:       pid,
				VAddr:     pageKey.VAddr,
			})
		}
		return
	case MigratingToGPU, MigratingToCPU:
		// The page is being migrated; treat the access as a coalesced fault.
		m.coalesceFault(pageKey, deviceID, requestID, replyTo)
		return
	case CPUResident:
		// The GMMU decides read vs write: writes to remotely-accessible pages
		// fault immediately; reads are counted GPU-side. Every fault that
		// reaches the driver is a migration request.
		m.stats.PageFaultRequests++
		m.coalesceFault(pageKey, deviceID, requestID, replyTo)
	}
}

func (m *UVMManager) coalesceFault(pageKey PageKey, deviceID uint64, requestID string, replyTo sim.RemotePort) {
	fkey := FaultKey{Page: pageKey, DeviceID: deviceID}
	fault, found := m.faults[fkey]
	if found {
		m.stats.CoalescedFaultReqs++
		fault.Waiters = append(fault.Waiters, FaultWaiter{
			RequestID: requestID,
			ReplyTo:   replyTo,
			DeviceID:  deviceID,
		})
		return
	}

	now := m.d.TickScheduler.CurrentTime()
	cfg := m.config
	fault = &PageFault{
		ID:          m.newID("fault"),
		Key:         fkey,
		RegionBase:  cfg.alignDown(pageKey.VAddr, cfg.RegionSize),
		VABlockBase: cfg.alignDown(pageKey.VAddr, cfg.VABlockSize),
		CreatedAt:   now,
		State:       FaultPending,
	}
	fault.Waiters = append(fault.Waiters, FaultWaiter{
		RequestID: requestID,
		ReplyTo:   replyTo,
		DeviceID:  deviceID,
	})
	m.faults[fkey] = fault
	m.faultsByID[fault.ID] = fault
	m.stats.UniquePageFaults++

	region := m.regionForKey(RegionKey{PID: pageKey.PID, Base: fault.RegionBase, DeviceID: deviceID})
	if region != nil {
		region.ActiveFaults++
	}

	// Schedule the fixed fault-handling latency as an async event.
	cycles := m.config.faultHandlingCycles()
	readyAt := now
	if cycles > 0 {
		readyAt = m.config.GPUCoreFrequency.NCyclesLater(cycles, now)
	}
	fault.ReadyAt = readyAt
	if !m.config.Ideal {
		m.stats.FaultHandlingTime += readyAt - now
	}
	m.d.Engine.Schedule(newFaultHandlingCompleteEvent(readyAt, m.d, fault.ID))
}

// handleFaultReady runs the fault-ready stage: TBN selection, capacity check,
// eviction, and migration.
func (m *UVMManager) handleFaultReady(faultID string) {
	fault := m.faultsByID[faultID]
	if fault == nil || fault.State != FaultPending {
		return
	}
	fault.State = FaultReady

	block := m.blocks[BlockKey{PID: fault.Key.Page.PID, Base: fault.VABlockBase}]
	if block == nil {
		return
	}

	sel := m.selectTBNRegion(fault.Key.Page, block)
	fault.DemandPages = sel.demandPages
	fault.PrefetchPages = sel.prefetchPages

	// Merge with an existing overlapping migration if any.
	if migID, ok := m.pageToMig[fault.Key.Page]; ok {
		mig := m.migrations[migID]
		if mig != nil {
			fault.MigrationID = migID
			mig.FaultIDs = append(mig.FaultIDs, fault.ID)
			fault.State = FaultMigrating
			return
		}
	}

	// Ensure GPU capacity: required frames = non-resident selected pages.
	var required uint64
	for _, pk := range sel.pageKeys {
		mp := m.pages[pk]
		if mp == nil || !mp.GPUFrameValid {
			required++
		}
	}
	exclude := make(map[PageKey]bool)
	for _, pk := range sel.pageKeys {
		exclude[pk] = true
	}
	victims := m.selectLRUVictims(required, exclude)
	if len(victims) > 0 { // sbin_codex: TLB shootdown before finalizing eviction.
		m.beginEviction(victims, func() {
			m.startCPUGPUMigration(fault, sel)
		})
		return
	}
	m.startCPUGPUMigration(fault, sel)
}

// startCPUGPUMigration allocates GPU frames, performs the transfer, updates
// PTEs, and replays waiters.
func (m *UVMManager) startCPUGPUMigration(fault *PageFault, sel tbnSelection) {
	cfg := m.config
	fault.State = FaultMigrating

	mig := &Migration{
		ID:            m.newID("mig"),
		Direction:     CPUToGPU,
		Trigger:       TriggerFault,
		DeviceID:      fault.Key.DeviceID,
		CreatedAt:     m.d.TickScheduler.CurrentTime(),
		DemandPages:   sel.demandPages,
		PrefetchPages: sel.prefetchPages,
	}
	m.migrations[mig.ID] = mig
	m.pageToMig[fault.Key.Page] = mig.ID
	mig.FaultIDs = append(mig.FaultIDs, fault.ID)
	// sbin_codex: remember the requesting GMMU for access-counter resets.
	if len(fault.Waiters) > 0 {
		mig.GMMUPort = fault.Waiters[0].ReplyTo
	}

	// Reserve GPU frames and set pages to MigratingToGPU.
	for _, pk := range sel.pageKeys {
		mp := m.pages[pk]
		if mp == nil {
			continue
		}
		if mp.GPUFrameValid && mp.State == GPUResident {
			continue
		}
		if m.freeGPUFrames == 0 {
			// Budget exhausted without an eligible victim; leave the page
			// CPU-resident rather than over-committing GPU frames.
			continue
		}
		frame, ok := m.d.memAllocator.TryAllocatePhysicalPage(1)
		if !ok {
			continue
		}
		mp.GPUFramePAddr = frame
		mp.GPUFrameValid = true
		mp.State = MigratingToGPU
		if mp.TimesMigrated > 0 {
			m.stats.RepeatedMigrations++
		}
		m.stats.GPUResidentPages++
		m.stats.GPUResidentBytes += cfg.PageSize
		m.freeGPUFrames--
		mig.Pages = append(mig.Pages, pk)
		mig.Bytes += cfg.PageSize
	}
	m.stats.DemandMigPages += sel.demandPages
	m.stats.PrefetchPages += sel.prefetchPages
	m.stats.CPUToGPUMigrations++
	m.stats.MigratedPages += uint64(len(mig.Pages))
	m.stats.MigratedBytes += mig.Bytes

	// Migration data transfer (CPU -> GPU). In ideal mode this is zero-latency.
	m.migrateData(mig)
	m.updateResidencyPeak()
}

// migrateData performs the data plane for a migration. In normal mode it
// schedules a completion event at the interconnect-transfer latency; in ideal
// mode it completes at the current time through the same state transitions.
func (m *UVMManager) migrateData(mig *Migration) {
	mig.DataStartedAt = m.d.TickScheduler.CurrentTime()
	m.copyMigrationData(mig)

	if m.config.Ideal {
		m.d.Engine.Schedule(newIdealMigrationCompleteEvent(mig.DataStartedAt, m.d, mig.ID))
		return
	}

	// Model migration latency as transfer size / effective CPU-GPU bandwidth.
	// A 16 GB/s PCIe reference is used; the resulting delay advances the
	// simulated clock without per-byte events.
	bandwidth := 16.0 * 1e9
	delay := sim.VTimeInSec(float64(mig.Bytes) / bandwidth)
	doneAt := m.config.GPUCoreFrequency.NoEarlierThan(mig.DataStartedAt + delay)
	m.d.Engine.Schedule(newMigrationCompleteEvent(doneAt, m.d, mig.ID))
}

// copyMigrationData performs the byte-level data plane for a migration between
// the CPU backing frame and the GPU frame in globalStorage.
func (m *UVMManager) copyMigrationData(mig *Migration) {
	for _, pk := range mig.Pages {
		mp := m.pages[pk]
		if mp == nil || !mp.GPUFrameValid {
			continue
		}
		if mig.Direction == CPUToGPU {
			data, err := m.d.globalStorage.Read(mp.CPUBackingPAddr, m.config.PageSize)
			if err != nil {
				continue
			}
			_ = m.d.globalStorage.Write(mp.GPUFramePAddr, data)
		} else {
			data, err := m.d.globalStorage.Read(mp.GPUFramePAddr, m.config.PageSize)
			if err != nil {
				continue
			}
			_ = m.d.globalStorage.Write(mp.CPUBackingPAddr, data)
		}
	}
}

// completeMigration finalizes a CPU->GPU migration: updates PTEs, resets
// access counters, updates LRU/residency, and replays all fault waiters.
func (m *UVMManager) completeMigration(migID string) {
	mig := m.migrations[migID]
	if mig == nil {
		return
	}
	mig.DataFinishedAt = m.d.TickScheduler.CurrentTime()
	if !m.config.Ideal {
		m.stats.MigrationTime += mig.DataFinishedAt - mig.DataStartedAt
	}

	for _, pk := range mig.Pages {
		mp := m.pages[pk]
		if mp == nil {
			continue
		}
		mp.State = GPUResident

		// sbin_codex: the region joins the driver LRU list on GPU residency.
		m.addLRU(RegionKey{PID: mp.Key.PID, Base: m.config.alignDown(mp.Key.VAddr, m.config.RegionSize), DeviceID: mig.DeviceID})

		page := vm.Page{
			PID:              mp.Key.PID,
			VAddr:            mp.Key.VAddr,
			PAddr:            mp.GPUFramePAddr,
			PageSize:         m.config.PageSize,
			Valid:            true,
			DeviceID:         mig.DeviceID,
			Unified:          false,
			Managed:          true,
			IsMigrating:      false,
			RemoteAccessible: false,
		}
		m.d.memAllocator.UpdatePage(page)

		// Reset the access counter for the region.
		regionBase := m.config.alignDown(mp.Key.VAddr, m.config.RegionSize)
		ack := AccessCounterKey{PID: mp.Key.PID, RegionBase: regionBase, DeviceID: mig.DeviceID}
		if cs := m.accessCounts[ack]; cs != nil {
			cs.Count = 0
			cs.Epoch++
			cs.Notification = false
			m.stats.AccessCounterResets++
		}
	}

	// sbin_codex: sync the GPU-side access counter with the driver: clear the
	// counters of the migrated regions so the new residency epoch starts clean.
	m.resetGPUAccessCounters(mig)

	// Replay waiters.
	for _, fid := range mig.FaultIDs {
		m.replayFault(fid)
	}
	delete(m.migrations, migID)
	for _, pk := range mig.Pages {
		delete(m.pageToMig, pk)
	}
}

// replayFault responds to all GPU translation waiters of a completed fault.
func (m *UVMManager) replayFault(faultID string) {
	fault := m.faultsByID[faultID]
	if fault == nil {
		return
	}
	fault.State = FaultComplete
	for _, w := range fault.Waiters {
		m.d.uvmReplyFault(w)
	}
	region := m.regionForKey(RegionKey{PID: fault.Key.Page.PID, Base: fault.RegionBase, DeviceID: fault.Key.DeviceID})
	if region != nil && region.ActiveFaults > 0 {
		region.ActiveFaults--
	}
	delete(m.faults, fault.Key)
	delete(m.faultsByID, faultID)
}

// replyFaultWaiter sends a fault-completion response to a single GMMU waiter.
func (m *UVMManager) replyFaultWaiter(w FaultWaiter) {
	m.d.uvmReplyFault(w)
}

// hasPendingWorkInRange reports whether any outstanding fault or migration
// covers a page within [start, start+size). The D2H middleware waits until
// this returns false before reading managed buffers, so the flush-triggered
// write-backs (and their migrations) complete first.
func (m *UVMManager) hasPendingWorkInRange(pid vm.PID, start, size uint64) bool {
	cfg := m.config
	begin := cfg.alignDown(start, cfg.PageSize)
	end := cfg.alignDown(start+size-1, cfg.PageSize)
	for vAddr := begin; vAddr <= end; vAddr += cfg.PageSize {
		pk := PageKey{PID: pid, VAddr: vAddr}
		if _, ok := m.pageToMig[pk]; ok {
			return true
		}
	}
	for fk := range m.faults {
		if fk.Page.PID == pid && fk.Page.VAddr >= begin && fk.Page.VAddr <= end {
			return true
		}
	}
	return false
}

func (m *UVMManager) newID(prefix string) string {
	m.nextID++
	return prefix + "-" + sim.GetIDGenerator().Generate() + "-" + itoa(m.nextID)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
