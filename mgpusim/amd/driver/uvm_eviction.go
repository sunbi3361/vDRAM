package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
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

// touchLRU moves a GPU-resident region to the MRU end of the driver LRU list
// on a GPU access. The access counter is independent of this list. // sbin_codex
func (m *UVMManager) touchLRU(key RegionKey) {
	if elem, ok := m.lruMap[key]; ok {
		m.lru.MoveToFront(elem)
	}
}

// addLRU inserts a region into the driver LRU list once it becomes
// GPU-resident. // sbin_codex
func (m *UVMManager) addLRU(key RegionKey) {
	if _, ok := m.lruMap[key]; ok {
		return
	}
	m.lruMap[key] = m.lru.PushFront(key)
}

// removeLRU removes a region from the driver LRU list when it leaves GPU
// residency. // sbin_codex
func (m *UVMManager) removeLRU(key RegionKey) {
	if elem, ok := m.lruMap[key]; ok {
		m.lru.Remove(elem)
		delete(m.lruMap, key)
	}
}

// hasPendingEvictions reports whether a TLB-shootdown eviction is awaiting its
// GPU ACK. // sbin_codex
func (m *UVMManager) hasPendingEvictions() bool {
	m.stateMu.RLock() // sbin_codex: eviction ACK polling overlaps parallel UVM events.
	defer m.stateMu.RUnlock()

	return m.evictACK > 0
}

// withCapacity ensures the GPU has room for requiredFrames before running
// migrate. If evictions are needed, it reserves victims, performs a TLB
// shootdown, and resumes migrate only after the ACK. While an eviction is
// pending, further capacity requests queue and re-evaluate capacity when
// resumed. // sbin_codex
func (m *UVMManager) withCapacity(required uint64, exclude map[PageKey]bool, migrate func()) {
	if m.evictACK > 0 {
		m.pendingResumes = append(m.pendingResumes, func() {
			m.withCapacity(required, exclude, migrate)
		})
		return
	}
	victims := m.selectLRUVictims(required, exclude)
	if len(victims) > 0 {
		m.beginEviction(victims, func() {
			m.withCapacity(required, exclude, migrate)
		})
		return
	}
	migrate()
}

// beginEviction reserves the victim regions, sends a ShootDownCommand to the
// GPU to flush its TLB, and defers the PTE/frame finalization until the ACK.
// onDone resumes the migration that triggered the eviction. While an eviction
// is pending, further eviction requests queue their resumptions. // sbin_codex
func (m *UVMManager) beginEviction(victims []*RegionState, onDone func()) {
	if m.evictACK > 0 {
		m.pendingResumes = append(m.pendingResumes, onDone)
		return
	}
	if len(victims) == 0 {
		if onDone != nil {
			onDone()
		}
		return
	}
	m.evicting = victims
	m.evictOnDone = onDone

	var vAddrs []uint64
	var pid vm.PID
	for _, region := range victims {
		for _, pk := range region.Pages {
			vAddrs = append(vAddrs, pk.VAddr)
			pid = pk.PID
		}
	}
	if len(vAddrs) == 0 {
		m.finalizeEviction()
		return
	}

	req := protocol.NewShootdownCommand(
		m.d.gpuPort, m.d.GPUs[0], vAddrs, pid)
	// m.d.requestsToSend = append(m.d.requestsToSend, req) // sbin_codex: parallel eviction events use the synchronized queue.
	m.d.enqueueRequestsToSend(req) // sbin_codex
	m.evictACK = 1
}

// finalizeEviction applies the reserved evictions after the GPU TLB has been
// flushed, then resumes the pending migration and any queued resumptions. // sbin_codex
func (m *UVMManager) finalizeEviction() {
	m.stateMu.Lock() // sbin_codex: the ACK transition mutates the complete UVM state machine.
	defer m.stateMu.Unlock()

	for _, region := range m.evicting {
		m.evictRegion(region)
	}
	m.evicting = nil
	m.evictACK = 0
	onDone := m.evictOnDone
	m.evictOnDone = nil
	if onDone != nil {
		onDone()
	}
	if len(m.pendingResumes) > 0 {
		next := m.pendingResumes[0]
		m.pendingResumes = m.pendingResumes[1:]
		next()
	}
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

	m.removeLRU(region.Key)

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

// selectLRUVictims walks the driver LRU list from the LRU end and selects
// eligible regions until requiredFrames GPU frames are freed. The access
// counter does not influence victim selection. // sbin_codex
func (m *UVMManager) selectLRUVictims(requiredFrames uint64, exclude map[PageKey]bool) []*RegionState {
	if requiredFrames == 0 || m.freeGPUFrames >= requiredFrames {
		return nil
	}
	need := requiredFrames - m.freeGPUFrames

	var victims []*RegionState
	freed := uint64(0)
	for elem := m.lru.Back(); elem != nil; elem = elem.Prev() {
		key := elem.Value.(RegionKey)
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
