package driver

// sbin_codex: access-counter driven migration (spec 12, 14, 16).
//
// The GPU-side counter observes remote accesses after translation and notifies
// the driver the moment a 64KB region crosses the threshold. If that region is
// already being brought to the GPU, the notification is ignored: the existing
// transaction is authoritative and no duplicate DMA is issued.

import (
	"github.com/sarchlab/akita/v4/mem/vm"
)

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

	m.admitAccessCounterServiceLocked(
		RegionKey{PID: pid, Base: regionBase, DeviceID: deviceID})
}

// admitAccessCounterServiceLocked queues one access-counter migration.
//
// An access-counter migration is a page fault: it goes through the same service
// queue, waits for the single service slot, is charged the fixed software
// latency, and runs TBN (spec 8.4, 10.1, 11). The only thing that separates it
// from a demand fault is what raised it, which the transaction's Trigger keeps
// so the two stay distinguishable in the statistics.
//
// Every notification is answered. The counter emits one per region and per
// residency episode and holds the region's remote writes until that answer
// arrives, so a notification that is dropped rather than answered strands the
// writes and the compute units waiting on them. A notification may only be
// swallowed when some other outstanding transaction has already taken
// responsibility for answering the region. // sbin_codex
func (m *UVMManager) admitAccessCounterServiceLocked(key RegionKey) {
	region := m.regions[key]
	if region == nil {
		// Not a managed region; nothing here will ever migrate it.
		m.sendRefusedReplayLocked(key)

		return
	}

	if !m.regionAdmissible(region, key) {
		return
	}

	cfg := m.config
	txn := &FaultTransaction{
		ID:           m.newID("acsvc"),
		Key:          key,
		Trigger:      TriggerAccessCounter,
		VABlockBase:  cfg.alignDown(key.Base, cfg.VABlockSize),
		CreatedAt:    m.d.TickScheduler.CurrentTime(),
		State:        FaultPending,
		demandVAddrs: m.regionDemandMask(region),
	}
	m.faults[key] = txn
	m.faultsByID[txn.ID] = txn
	m.faultServiceCue = append(m.faultServiceCue, txn.ID)
	m.stats.AccessCounterServices++

	m.startNextFaultServiceLocked()
}

// regionDemandMask is the demand mask of an access-counter service: the whole
// 64KB region the counter raised.
//
// Spec 11.7 keeps a demand-fault mask 4KB granular, but a counter has no finer
// granularity to offer — spec 14 counts per 64KB region, and that region is
// exactly what the notification asks for. Leaving the mask empty instead would
// book the requested region as prefetch and make the prefetch statistics read
// as if TBN had speculated on it. // sbin_codex
func (m *UVMManager) regionDemandMask(region *RegionState) map[uint64]bool {
	mask := make(map[uint64]bool, len(region.Pages))

	for _, pk := range region.Pages {
		mask[pk.VAddr] = true
	}

	return mask
}

// regionAdmissible reports whether the notification should open a new service.
//
// Spec 16: a region already in a fault, migration, or prefetch transaction
// swallows the notification, because that transaction is authoritative. What
// makes swallowing safe is that each of those transactions ends by answering
// the region — a fault and an admission with a replay, an eviction by
// re-arming the region's counter in finalizeEviction. // sbin_codex
func (m *UVMManager) regionAdmissible(
	region *RegionState,
	key RegionKey,
) bool {
	if _, found := m.faults[key]; found {
		m.stats.AccessCounterSuppressed++

		return false
	}

	if region.busy() || region.MigrationID != "" || region.FaultID != "" {
		m.stats.AccessCounterSuppressed++

		return false
	}

	return true
}
