package driver

import (
	"log"

	"github.com/sarchlab/mgpusim/v4/amd/protocol" // sbin_codex
)

// quiescencePendingLocked reports global UVM drain ownership. The caller must
// hold stateMu. // sbin_codex
func (m *UVMManager) quiescencePendingLocked() bool { // sbin_codex
	return m.evictACK > 0 || m.activeMigrationID != ""
}

// publishMigratingPagesLocked mirrors CPU-backed parked PTEs before drain. // sbin_codex
func (m *UVMManager) publishMigratingPagesLocked(migration *Migration) { // sbin_codex
	for _, key := range migration.Pages {
		managedPage := m.pages[key]
		if managedPage == nil {
			continue
		}
		page, found := m.d.pageTable.Find(key.PID, key.VAddr)
		if !found {
			continue
		}
		page.PAddr = managedPage.CPUBackingPAddr
		page.DeviceID = 0
		page.IsMigrating = true
		m.d.memAllocator.UpdatePage(page)
	}
}

// beginMigrationQuiescenceLocked starts the existing RDMA drain protocol. // sbin_codex
func (m *UVMManager) beginMigrationQuiescenceLocked(migration *Migration) { // sbin_codex
	if m.quiescencePendingLocked() {
		m.pendingResumes = append(m.pendingResumes, func() {
			m.beginMigrationQuiescenceLocked(migration)
		})
		return
	}
	if len(migration.Pages) == 0 {
		m.migrateData(migration)
		m.updateResidencyPeak()
		return
	}

	m.activeMigrationID = migration.ID
	m.activeMigrationDeviceID = migration.DeviceID
	m.migrationDrainACK = 1
	log.Printf("DEBUG UVM quiescence begin migration=%s", migration.ID)
	m.d.enqueueRequestsToSend(protocol.NewRDMADrainCmdFromDriver(
		m.d.gpuPort,
		m.d.GPUs[migration.DeviceID-1],
	))
}

func (m *UVMManager) hasPendingMigrationDrain() bool { // sbin_codex
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.migrationDrainACK > 0
}

func (m *UVMManager) processMigrationDrainComplete() { // sbin_codex
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.migrationDrainACK == 0 {
		return
	}
	m.migrationDrainACK = 0
	log.Printf("DEBUG UVM RDMA drain complete migration=%s", m.activeMigrationID)
	migration := m.migrations[m.activeMigrationID]
	if migration == nil {
		return
	}
	virtualAddresses := make([]uint64, 0, len(migration.Pages))
	for _, key := range migration.Pages {
		virtualAddresses = append(virtualAddresses, key.VAddr)
	}
	m.migrationQuiesceACK = 1
	m.d.enqueueShootDownReqs(
		migration.Pages[0].PID,
		virtualAddresses,
		[]uint64{migration.DeviceID},
	)
}

func (m *UVMManager) hasPendingMigrationQuiescence() bool { // sbin_codex
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.migrationQuiesceACK > 0
}

func (m *UVMManager) finalizeMigrationQuiescence() { // sbin_codex
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.migrationQuiesceACK == 0 {
		return
	}
	m.migrationQuiesceACK = 0
	log.Printf("DEBUG UVM shootdown complete migration=%s", m.activeMigrationID)
	if migration := m.migrations[m.activeMigrationID]; migration != nil {
		m.migrateData(migration)
		m.updateResidencyPeak()
	}
}

func (m *UVMManager) beginMigrationRestartLocked(migration *Migration) { // sbin_codex
	if m.activeMigrationID != migration.ID {
		return
	}
	m.migrationGPURestartACK = 1
	log.Printf("DEBUG UVM GPU restart begin migration=%s", migration.ID)
	m.d.enqueueRequestsToSend(protocol.NewGPURestartReq(
		m.d.gpuPort,
		m.d.GPUs[migration.DeviceID-1],
	))
}

func (m *UVMManager) hasPendingMigrationGPURestart() bool { // sbin_codex
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.migrationGPURestartACK > 0
}

func (m *UVMManager) processMigrationGPURestartComplete() { // sbin_codex
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.migrationGPURestartACK == 0 {
		return
	}
	m.migrationGPURestartACK = 0
	log.Printf("DEBUG UVM GPU restart complete migration=%s", m.activeMigrationID)
	m.migrationRDMARestartACK = 1
	m.d.enqueueRequestsToSend(protocol.NewRDMARestartCmdFromDriver(
		m.d.gpuPort,
		m.d.GPUs[m.activeMigrationDeviceID-1],
	))
}

func (m *UVMManager) hasPendingMigrationRDMARestart() bool { // sbin_codex
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.migrationRDMARestartACK > 0
}

func (m *UVMManager) processMigrationRDMARestartComplete() { // sbin_codex
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.migrationRDMARestartACK == 0 {
		return
	}
	m.migrationRDMARestartACK = 0
	log.Printf("DEBUG UVM RDMA restart complete migration=%s", m.activeMigrationID)
	m.activeMigrationID = ""
	m.activeMigrationDeviceID = 0
	if len(m.pendingResumes) > 0 {
		next := m.pendingResumes[0]
		m.pendingResumes = m.pendingResumes[1:]
		next()
	}
}
