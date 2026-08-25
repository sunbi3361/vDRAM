package driver

// sbin_codex: UVM page migration data plane.
//
// In normal mode a migration is carried by the existing GPU DMA engine over
// PCIe: the driver picks the pages, forms maximal runs whose source and
// destination physical addresses are both contiguous, and emits one MemCopy
// request per run (spec 23.1, 23.1.2). The UVM driver imposes no concurrency
// cap of its own; queueing and bandwidth are the DMA/PCIe model's job.
//
// In ideal mode the same functional sequence runs with zero transfer time: the
// bytes are moved through globalStorage and the completion event is scheduled
// at the current time. Every counter is updated identically (spec 1.2).

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// uvmDeviceID is the only GPU UVM supports today. Device 0 is the CPU.
const uvmDeviceID uint64 = 1

// startCPUToGPUMigration reserves GPU frames for the selected pages, publishes
// the migrating mapping, and starts the transfer.
func (m *UVMManager) startCPUToGPUMigration(
	txn *FaultTransaction,
	pages []PageKey,
	trigger MigrationTrigger,
) {
	deviceID := uvmDeviceID
	if txn != nil {
		deviceID = txn.Key.DeviceID
	}

	mig := m.newMigration(CPUToGPU, trigger, deviceID, pages)
	if mig == nil {
		m.deferAdmission(txn, pages, deviceID)
		return
	}

	if txn != nil {
		txn.MigrationID = mig.ID
		txn.State = FaultMigrating
		mig.FaultIDs = append(mig.FaultIDs, txn.ID)
	}

	m.beginTransfer(mig)

	// sbin_codex: the admission already holds its frames, so pre-eviction can
	// run concurrently with this H2D transfer (spec 17.1).
	exclude := make(map[PageKey]bool, len(pages))
	for _, pk := range pages {
		exclude[pk] = true
	}

	m.preEvict(exclude)
}

// deferAdmission handles an admission that could not reserve a GPU frame.
//
// The region is reported as refused, which releases whatever the GPU stalled
// on it: a write held only to force a migration is then performed over PCIe
// rather than waiting for a mapping that is not coming. Keeping it stalled
// instead would be closer to spec 15, but nothing would ever release it,
// because the capacity that would satisfy it is exactly what is missing.
// sbin_codex
func (m *UVMManager) deferAdmission(
	txn *FaultTransaction,
	pages []PageKey,
	deviceID uint64,
) {
	for _, key := range m.regionsOf(pages, deviceID) {
		m.sendRefusedReplayLocked(key)
	}

	if txn == nil {
		return
	}

	txn.State = FaultComplete
	m.retireFaultLocked(txn)
}

// regionsOf returns the distinct regions the pages belong to.
func (m *UVMManager) regionsOf(
	pages []PageKey,
	deviceID uint64,
) []RegionKey {
	seen := make(map[RegionKey]bool, len(pages))

	var keys []RegionKey

	for _, pk := range pages {
		managedPage := m.pages[pk]
		if managedPage == nil {
			continue
		}

		key := RegionKey{
			PID:      pk.PID,
			Base:     managedPage.RegionBase,
			DeviceID: deviceID,
		}
		if seen[key] {
			continue
		}

		seen[key] = true
		keys = append(keys, key)
	}

	return keys
}

// newMigration reserves frames and moves the pages into the migrating state.
// It returns nil when no page needs to move.
func (m *UVMManager) newMigration(
	direction MigrationDirection,
	trigger MigrationTrigger,
	deviceID uint64,
	pages []PageKey,
) *Migration {
	cfg := m.config
	mig := &Migration{
		ID:        m.newID("mig"),
		Direction: direction,
		Trigger:   trigger,
		DeviceID:  deviceID,
		CreatedAt: m.d.TickScheduler.CurrentTime(),
	}

	regionSeen := make(map[RegionKey]bool)

	for _, pk := range pages {
		managedPage := m.pages[pk]
		if managedPage == nil {
			continue
		}

		if !m.admitPageToMigration(mig, managedPage) {
			continue
		}

		mig.PID = pk.PID
		mig.Pages = append(mig.Pages, pk)
		mig.Bytes += cfg.PageSize
		m.migrationsByPage[pk] = mig.ID

		key := RegionKey{
			PID:      pk.PID,
			Base:     managedPage.RegionBase,
			DeviceID: deviceID,
		}
		if !regionSeen[key] {
			regionSeen[key] = true
			mig.RegionKeys = append(mig.RegionKeys, key)
		}
	}

	if len(mig.Pages) == 0 {
		return nil
	}

	m.migrations[mig.ID] = mig

	m.markRegionsMigrating(mig)
	m.recordMigrationStats(mig)

	return mig
}

// admitPageToMigration moves one page into the transient migrating state and
// reserves the physical frame the transfer needs.
func (m *UVMManager) admitPageToMigration(
	mig *Migration,
	managedPage *ManagedPage,
) bool {
	if mig.Direction == CPUToGPU {
		if managedPage.State != CPUResident {
			return false
		}

		if !managedPage.GPUFrameValid {
			if m.freeGPUFrames() == 0 {
				return false
			}

			frame, ok := m.d.memAllocator.TryAllocatePhysicalPage(1)
			if !ok {
				return false
			}

			managedPage.GPUFramePAddr = frame
			managedPage.GPUFrameValid = true
			m.gpuFramesInUse++
		}

		if managedPage.RemoteMapped {
			mig.NeedsTLBInvalidate = true
		}

		managedPage.State = MigratingToGPU
		m.publishMigratingPTE(managedPage)

		return true
	}

	if managedPage.State != GPUResident || !managedPage.GPUFrameValid {
		return false
	}

	managedPage.State = MigratingToCPU

	return true
}

// publishMigratingPTE parks the mapping so a page-table walk during the
// transfer produces a fault instead of a usable translation.
func (m *UVMManager) publishMigratingPTE(managedPage *ManagedPage) {
	m.d.memAllocator.UpdatePage(vm.Page{
		PID:              managedPage.Key.PID,
		VAddr:            managedPage.Key.VAddr,
		PAddr:            managedPage.CPUBackingPAddr,
		PageSize:         m.config.PageSize,
		Valid:            true,
		DeviceID:         0,
		Unified:          false,
		Managed:          true,
		IsMigrating:      true,
		RemoteAccessible: false,
	})
}

func (m *UVMManager) markRegionsMigrating(mig *Migration) {
	phase := RegionMigratingToGPU
	if mig.Direction == GPUToCPU {
		phase = RegionEvicting
	}

	for _, key := range mig.RegionKeys {
		region := m.regions[key]
		if region == nil {
			continue
		}

		region.Phase = phase
		region.MigrationID = mig.ID
	}
}

func (m *UVMManager) recordMigrationStats(mig *Migration) {
	pages := uint64(len(mig.Pages))

	if mig.Direction == CPUToGPU {
		m.stats.CPUToGPUMigrations++
		m.stats.BytesCPUToGPU += mig.Bytes

		switch mig.Trigger {
		case TriggerFault:
			m.stats.DemandMigrations++
		case TriggerAccessCounter:
			m.stats.AccessCounterMigrations++
			m.stats.BytesAccessCounterMigrated += mig.Bytes
		case TriggerEviction:
		}

		for _, pk := range mig.Pages {
			if managedPage := m.pages[pk]; managedPage != nil &&
				managedPage.TimesMigrated > 0 {
				m.stats.RepeatedMigrations++
			}
		}

		m.stats.MigratedPages += pages
		m.stats.MigratedBytes += mig.Bytes

		return
	}

	m.stats.GPUToCPUMigrations++
	m.stats.BytesGPUToCPU += mig.Bytes
	m.stats.MigratedPages += pages
	m.stats.MigratedBytes += mig.Bytes
}

// beginTransfer starts the data plane for a migration. An admission first
// drains the region's outstanding remote accesses so that the host-memory
// snapshot it takes is authoritative. // sbin_codex
func (m *UVMManager) beginTransfer(mig *Migration) {
	if mig.Direction == CPUToGPU {
		m.drainRegionsThen(mig.RegionKeys, func() {
			m.startTransfer(mig)
		})

		return
	}

	m.startTransfer(mig)
}

func (m *UVMManager) startTransfer(mig *Migration) {
	mig.DataStartedAt = m.d.TickScheduler.CurrentTime()

	if m.config.Ideal {
		// Ideal mode charges no transfer time but still moves the bytes and
		// counts them, so the migration decisions stay observable.
		m.copyMigrationData(mig)
		m.d.Engine.Schedule(
			newMigrationCompleteEvent(mig.DataStartedAt, m.d, mig.ID))

		return
	}

	m.issueMigrationDMA(mig)
}

// issueMigrationDMA emits one MemCopy request per maximal contiguous run.
func (m *UVMManager) issueMigrationDMA(mig *Migration) {
	runs := m.buildContiguousRuns(mig)
	mig.PendingDMA = len(runs)

	if mig.PendingDMA == 0 {
		m.d.Engine.Schedule(
			newMigrationCompleteEvent(mig.DataStartedAt, m.d, mig.ID))

		return
	}

	gpuPort := m.d.GPUs[mig.DeviceID-1]

	for _, run := range runs {
		if mig.Direction == CPUToGPU {
			data, err := m.d.globalStorage.Read(run.cpuBase, run.size)
			if err != nil {
				panic(err)
			}

			h2d := protocol.NewMemCopyH2DReq(
				m.d.gpuPort, gpuPort, data, run.gpuBase)
			m.dmaToMigration[h2d.ID] = mig.ID
			m.d.enqueueRequestsToSend(h2d)

			continue
		}

		d2h := protocol.NewMemCopyD2HReq(
			m.d.gpuPort, gpuPort, run.gpuBase, make([]byte, run.size))
		m.dmaToMigration[d2h.ID] = mig.ID
		m.d.enqueueRequestsToSend(d2h)
	}
}

// migrationRun is one maximal run of pages whose CPU-side and GPU-side
// physical addresses are both contiguous.
type migrationRun struct {
	cpuBase uint64
	gpuBase uint64
	size    uint64
}

func (m *UVMManager) buildContiguousRuns(mig *Migration) []migrationRun {
	pageSize := m.config.PageSize

	var runs []migrationRun

	for _, pk := range mig.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil || !managedPage.GPUFrameValid {
			continue
		}

		if len(runs) > 0 {
			last := &runs[len(runs)-1]
			if last.cpuBase+last.size == managedPage.CPUBackingPAddr &&
				last.gpuBase+last.size == managedPage.GPUFramePAddr {
				last.size += pageSize
				continue
			}
		}

		runs = append(runs, migrationRun{
			cpuBase: managedPage.CPUBackingPAddr,
			gpuBase: managedPage.GPUFramePAddr,
			size:    pageSize,
		})
	}

	return runs
}

// copyMigrationData moves the bytes through globalStorage. It backs the ideal
// mode transfer and the D2H write-back of an eviction.
func (m *UVMManager) copyMigrationData(mig *Migration) {
	for _, pk := range mig.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil || !managedPage.GPUFrameValid {
			continue
		}

		src, dst := managedPage.CPUBackingPAddr, managedPage.GPUFramePAddr
		if mig.Direction == GPUToCPU {
			src, dst = dst, src
		}

		data, err := m.d.globalStorage.Read(src, m.config.PageSize)
		if err != nil {
			continue
		}

		_ = m.d.globalStorage.Write(dst, data)
	}
}

// onMigrationDMADone consumes one DMA completion. When the last run of a
// migration lands, the functional completion event is scheduled.
func (m *UVMManager) onMigrationDMADone(reqID string, data []byte, gpuBase uint64) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	migID, found := m.dmaToMigration[reqID]
	if !found {
		return
	}

	delete(m.dmaToMigration, reqID)

	mig := m.migrations[migID]
	if mig == nil {
		return
	}

	if mig.Direction == GPUToCPU && data != nil {
		m.writeBackEvictedRun(mig, gpuBase, data)
	}

	mig.PendingDMA--
	if mig.PendingDMA > 0 {
		return
	}

	now := m.d.TickScheduler.CurrentTime()
	m.d.Engine.Schedule(newMigrationCompleteEvent(now, m.d, mig.ID))
}

// writeBackEvictedRun stores an evicted run into the CPU backing frames it
// came from. The run is contiguous on both sides by construction.
func (m *UVMManager) writeBackEvictedRun(
	mig *Migration,
	gpuBase uint64,
	data []byte,
) {
	pageSize := m.config.PageSize

	for _, pk := range mig.Pages {
		managedPage := m.pages[pk]
		if managedPage == nil || !managedPage.GPUFrameValid {
			continue
		}

		if managedPage.GPUFramePAddr < gpuBase {
			continue
		}

		offset := managedPage.GPUFramePAddr - gpuBase
		if offset+pageSize > uint64(len(data)) {
			continue
		}

		_ = m.d.globalStorage.Write(
			managedPage.CPUBackingPAddr, data[offset:offset+pageSize])
	}
}
