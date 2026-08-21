package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// RemoteAccessible reports whether a CPU-resident managed page may be returned
// to the GPU for a remote access instead of faulting.
func (mp *ManagedPage) RemoteAccessible() bool {
	return mp.State == CPUResident && mp.TimesMigrated > 0
}

// regionForKey returns the 64KB region state for a key, or nil.
func (m *UVMManager) regionForKey(key RegionKey) *RegionState {
	return m.regions[key]
}

// blockForKey returns the 2MB VA block state for a key, or nil.
func (m *UVMManager) blockForKey(key BlockKey) *VABlock {
	return m.blocks[key]
}

// recordRemoteAccess increments the 64KB access counter for a remotely
// accessed CPU-resident region and triggers a migration at the threshold.
func (m *UVMManager) recordRemoteAccess(pid vm.PID, regionBase uint64, deviceID uint64, now sim.VTimeInSec) {
	cfg := m.config
	key := AccessCounterKey{PID: pid, RegionBase: regionBase, DeviceID: deviceID}

	cs := m.accessCounts[key]
	if cs == nil {
		cs = &AccessCounterState{}
		m.accessCounts[key] = cs
	}
	cs.Count++
	cs.LastAccess = now
	m.stats.RemoteAccesses++

	// Touch LRU recency for the remote region.
	region := m.regions[RegionKey{PID: pid, Base: regionBase, DeviceID: deviceID}]
	if region != nil {
		region.LastAccess = now
	}

	if cs.Notification {
		return
	}
	if cs.Count >= cfg.AccessCounterThreshold {
		cs.Notification = true
		m.stats.AccessCounterNotif++
		m.triggerAccessCounterMigration(key)
	}
}

// triggerAccessCounterMigration migrates a hot 64KB CPU-resident region to the
// GPU without charging the fixed fault latency.
func (m *UVMManager) triggerAccessCounterMigration(key AccessCounterKey) {
	cfg := m.config
	region := m.regions[RegionKey{PID: key.PID, Base: key.RegionBase, DeviceID: key.DeviceID}]
	if region == nil {
		return
	}
	// Only migrate pages that are CPU-resident and remotely accessible.
	var pages []PageKey
	for _, pk := range region.Pages {
		mp := m.pages[pk]
		if mp != nil && mp.State == CPUResident {
			pages = append(pages, pk)
		}
	}
	if len(pages) == 0 {
		return
	}

	var required uint64
	for _, pk := range pages {
		mp := m.pages[pk]
		if mp == nil || !mp.GPUFrameValid {
			required++
		}
	}
	exclude := make(map[PageKey]bool)
	for _, pk := range pages {
		exclude[pk] = true
	}
	victims := m.selectLRUVictims(required, exclude)
	for _, v := range victims {
		m.evictRegion(v)
	}

	mig := &Migration{
		ID:            m.newID("acmig"),
		Direction:     CPUToGPU,
		Trigger:       TriggerAccessCounter,
		DeviceID:      key.DeviceID,
		CreatedAt:     m.d.TickScheduler.CurrentTime(),
		DemandPages:   uint64(len(pages)),
		PrefetchPages: 0,
	}
	m.migrations[mig.ID] = mig

	for _, pk := range pages {
		mp := m.pages[pk]
		if mp == nil || mp.GPUFrameValid {
			continue
		}
		if m.freeGPUFrames == 0 {
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
	m.stats.DemandMigPages += uint64(len(mig.Pages))
	m.stats.CPUToGPUMigrations++
	m.stats.MigratedPages += uint64(len(mig.Pages))
	m.stats.MigratedBytes += mig.Bytes
	m.stats.AccessCounterMigr++

	m.migrateData(mig)
	m.updateResidencyPeak()
}
