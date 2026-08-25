package driver

// sbin_codex: access-counter driven migration (spec 12, 14, 16).
//
// The GPU-side counter observes remote accesses after translation and notifies
// the driver the moment a 64KB region crosses the threshold. If that region is
// already being brought to the GPU, the notification is ignored: the existing
// transaction is authoritative and no duplicate DMA is issued.

import (
	"fmt"
	"os"

	"github.com/sarchlab/akita/v4/mem/vm"
)

var uvmDbg = os.Getenv("UVM_DEBUG") != ""

func dbgd(format string, args ...interface{}) {
	if uvmDbg {
		fmt.Fprintf(os.Stderr, "[drvdbg] "+format+"\n", args...)
	}
}

// RemoteAccessible reports whether a CPU-resident managed page may be reached
// over PCIe instead of faulting.
func (mp *ManagedPage) RemoteAccessible() bool {
	return mp.State == CPUResident && mp.RemoteMapped
}

// regionForKey returns the 64KB region state for a key, or nil.
func (m *UVMManager) regionForKey(key RegionKey) *RegionState {
	return m.regions[key]
}

// blockForKey returns the 2MB VA block state for a key, or nil.
func (m *UVMManager) blockForKey(key BlockKey) *VABlock {
	return m.blocks[key]
}

// onAccessCounterNotify handles a 64KB remote-access threshold crossing.
func (m *UVMManager) onAccessCounterNotify(
	pid vm.PID,
	regionBase uint64,
	deviceID uint64,
) {
	m.stateMu.Lock() // sbin_codex: serialize with the fault and migration paths.
	defer m.stateMu.Unlock()

	m.stats.AccessCounterNotify++

	m.migrateRegionLocked(
		RegionKey{PID: pid, Base: regionBase, DeviceID: deviceID})
}

// migrateRegionLocked admits one 64KB region to the GPU.
func (m *UVMManager) migrateRegionLocked(key RegionKey) {
	region := m.regions[key]
	if region == nil {
		dbgd("SUPPRESS-NOREGION region=%#x", key.Base)
		return
	}

	// Spec 16: a region already in a fault, migration, or prefetch transaction
	// swallows the notification.
	if region.busy() || region.MigrationID != "" || region.FaultID != "" {
		m.stats.AccessCounterSuppressed++
		dbgd("SUPPRESS-BUSY region=%#x phase=%d mig=%q fault=%q",
			key.Base, region.Phase, region.MigrationID, region.FaultID)

		return
	}

	pages := make([]PageKey, 0, len(region.Pages))

	for _, pk := range region.Pages {
		if managedPage := m.pages[pk]; managedPage != nil &&
			managedPage.State == CPUResident {
			if _, inFlight := m.migrationsByPage[pk]; !inFlight {
				pages = append(pages, pk)
			}
		}
	}

	if len(pages) == 0 {
		m.stats.AccessCounterSuppressed++
		dbgd("SUPPRESS-NOPAGES region=%#x phase=%d", key.Base, region.Phase)

		return
	}

	exclude := make(map[PageKey]bool, len(pages))
	for _, pk := range pages {
		exclude[pk] = true
	}

	m.withCapacity(uint64(len(pages)), exclude, func() {
		m.startCPUToGPUMigration(nil, pages, TriggerAccessCounter)
	})
}
