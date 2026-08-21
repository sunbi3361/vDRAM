package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
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

// onAccessCounterNotify handles a GPU-side 64KB access-counter threshold
// notification. The GPU GMMU counts remote (PCIe) accesses to a
// CPU-resident region; at the threshold it asks the driver to migrate the
// region to the GPU. // sbin_codex
func (m *UVMManager) onAccessCounterNotify(pid vm.PID, regionBase uint64, deviceID uint64) {
	m.stats.AccessCounterNotif++
	m.triggerAccessCounterMigration(AccessCounterKey{
		PID:        pid,
		RegionBase: regionBase,
		DeviceID:   deviceID,
	})
}

// triggerAccessCounterMigration migrates a hot 64KB CPU-resident region to the
// GPU without charging the fixed fault latency.
func (m *UVMManager) triggerAccessCounterMigration(key AccessCounterKey) {
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
	if len(victims) > 0 { // sbin_codex: TLB shootdown before finalizing eviction.
		m.beginEviction(victims, func() {
			m.finishAccessCounterMigration(key, pages)
		})
		return
	}
	m.finishAccessCounterMigration(key, pages)
}

// finishAccessCounterMigration runs the access-counter-triggered migration
// after capacity has been ensured. // sbin_codex
func (m *UVMManager) finishAccessCounterMigration(key AccessCounterKey, pages []PageKey) {
	cfg := m.config
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
