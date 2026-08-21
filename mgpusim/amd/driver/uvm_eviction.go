package driver

import (
	"sort"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// regionGPUFrames returns the number of GPU-resident 4KB pages in a region.
func (m *UVMManager) regionGPUFrames(region *RegionState) uint64 {
	var count uint64
	for _, pk := range region.Pages {
		mp := m.pages[pk]
		if mp != nil && mp.GPUFrameValid && mp.State == GPUResident {
			count++
		}
	}
	return count
}

// evictRegion migrates a GPU-resident 64KB region back to CPU. It returns the
// number of pages evicted. All pages in the region must be eligible.
func (m *UVMManager) evictRegion(region *RegionState) uint64 {
	var evicted uint64

	for _, pk := range region.Pages {
		mp := m.pages[pk]
		if mp == nil || !mp.GPUFrameValid || mp.State != GPUResident {
			continue
		}

		page := vm.Page{
			PID:              mp.Key.PID,
			VAddr:            mp.Key.VAddr,
			PAddr:            mp.CPUBackingPAddr,
			PageSize:         m.config.PageSize,
			Valid:            true,
			DeviceID:         0,
			Unified:          false,
			Managed:          true,
			IsMigrating:      false,
			RemoteAccessible: true,
		}
		// sbin_codex: conservatively copy the GPU frame back to the CPU backing
		// since per-page dirty state is not modeled.
		if data, err := m.d.globalStorage.Read(mp.GPUFramePAddr, m.config.PageSize); err == nil {
			_ = m.d.globalStorage.Write(mp.CPUBackingPAddr, data)
		}

		m.d.memAllocator.FreePhysicalPage(1, mp.GPUFramePAddr)
		mp.GPUFramePAddr = 0
		mp.GPUFrameValid = false
		mp.State = CPUResident
		mp.TimesMigrated++
		m.d.memAllocator.UpdatePage(page)

		m.stats.Evictions++
		m.stats.EvictedPages++
		m.stats.EvictedBytes += m.config.PageSize
		evicted++
	}

	ack := AccessCounterKey{
		PID:        region.Key.PID,
		RegionBase: region.Key.Base,
		DeviceID:   region.Key.DeviceID,
	}
	cs := m.accessCounts[ack]
	if cs == nil {
		cs = &AccessCounterState{}
		m.accessCounts[ack] = cs
	}
	cs.Count = 0
	cs.Epoch++
	cs.Notification = false

	region.Generation++
	m.stats.GPUResidentPages -= evicted
	m.freeGPUFrames += evicted

	return evicted
}

// selectLRUVictims selects regions until requiredFrames GPU frames are freed.
// Ineligible regions are skipped. Victim selection is deterministic by
// (LastAccess, PID, RegionBase).
func (m *UVMManager) selectLRUVictims(requiredFrames uint64, exclude map[PageKey]bool) []*RegionState {
	if requiredFrames == 0 || m.freeGPUFrames >= requiredFrames {
		return nil
	}
	need := requiredFrames - m.freeGPUFrames

	var candidates []*RegionState
	for _, region := range m.regions {
		if m.regionEligible(region, exclude) {
			candidates = append(candidates, region)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := candidates[i], candidates[j]
		if ri.LastAccess != rj.LastAccess {
			return ri.LastAccess < rj.LastAccess
		}
		if ri.Key.PID != rj.Key.PID {
			return ri.Key.PID < rj.Key.PID
		}
		return ri.Key.Base < rj.Key.Base
	})

	var victims []*RegionState
	freed := uint64(0)
	for _, region := range candidates {
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

func (m *UVMManager) regionEligible(region *RegionState, exclude map[PageKey]bool) bool {
	if region.EvictionLocked {
		return false
	}
	if region.MigrationID != "" {
		return false
	}
	if region.ActiveFaults > 0 {
		return false
	}
	for _, pk := range region.Pages {
		if exclude[pk] {
			return false
		}
	}
	return true
}
