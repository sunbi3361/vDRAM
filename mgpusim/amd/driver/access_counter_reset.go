package driver

// sbin_codex: access-counter re-arming. After a 64KB region becomes
// GPU-resident its remote counter is meaningless, so the driver clears it on
// the GPU-side counter through the Command Processor.

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// SetAccessCounterResetDestination is retained for platforms that address the
// counter directly instead of going through the Command Processor.
func (d *Driver) SetAccessCounterResetDestination(destination sim.RemotePort) {
	if d.uvm == nil {
		return
	}

	d.uvm.stateMu.Lock()
	defer d.uvm.stateMu.Unlock()

	d.uvm.accessCounterResetDestination = destination
}

// AccessCounterResetDestination returns the configured reset target.
func (d *Driver) AccessCounterResetDestination() sim.RemotePort {
	if d.uvm == nil {
		return ""
	}

	d.uvm.stateMu.RLock()
	defer d.uvm.stateMu.RUnlock()

	return d.uvm.accessCounterResetDestination
}

// queueAccessCounterResets requires stateMu to be held by the caller.
func (m *UVMManager) queueAccessCounterResets(migration *Migration) {
	if migration.Direction != CPUToGPU || !m.config.AccessCounterEnabled {
		return
	}

	// The region set is derived from the migrated pages so a synthetic
	// migration without RegionKeys is handled identically. // sbin_codex
	for _, page := range migration.Pages {
		resetKey := AccessCounterResetKey{
			PID:        page.PID,
			RegionBase: m.config.alignDown(page.VAddr, m.config.RegionSize),
		}
		if m.pendingAccessCounterResetKeys[resetKey] {
			continue
		}

		m.pendingAccessCounterResetKeys[resetKey] = true
		m.pendingAccessCounterResets = append(
			m.pendingAccessCounterResets,
			pendingAccessCounterReset{
				Key: resetKey, DeviceID: migration.DeviceID,
			},
		)
	}
}

// queueRegionCounterResetLocked re-arms the counter of one 64KB region. It
// requires stateMu to be held by the caller. // sbin_codex
func (m *UVMManager) queueRegionCounterResetLocked(key RegionKey) {
	if !m.config.AccessCounterEnabled {
		return
	}

	resetKey := AccessCounterResetKey{PID: key.PID, RegionBase: key.Base}
	if m.pendingAccessCounterResetKeys[resetKey] {
		return
	}

	m.pendingAccessCounterResetKeys[resetKey] = true
	m.pendingAccessCounterResets = append(
		m.pendingAccessCounterResets,
		pendingAccessCounterReset{Key: resetKey, DeviceID: key.DeviceID},
	)
}

func (m *UVMManager) sendPendingAccessCounterReset() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	if len(m.pendingAccessCounterResets) == 0 || m.d.uvmPort == nil {
		return false
	}

	pending := m.pendingAccessCounterResets[0]

	dst := m.accessCounterResetDestination
	if dst == "" {
		dst = m.uvmControlPort(pending.DeviceID)
	}

	if dst == "" {
		// No counter is reachable; drop the reset rather than stalling.
		m.pendingAccessCounterResets = m.pendingAccessCounterResets[1:]
		delete(m.pendingAccessCounterResetKeys, pending.Key)

		return true
	}

	request := vm.NewAccessCounterResetReq(m.d.uvmPort.AsRemote(), dst)
	request.PID = pending.Key.PID
	request.RegionBase = pending.Key.RegionBase
	request.DeviceID = pending.DeviceID

	if err := m.d.uvmPort.Send(request); err != nil {
		return false
	}

	m.pendingAccessCounterResets = m.pendingAccessCounterResets[1:]
	delete(m.pendingAccessCounterResetKeys, pending.Key)
	m.stats.AccessCounterResets++

	return true
}

func (d *Driver) sendPendingAccessCounterReset() bool {
	if d.uvm == nil {
		return false
	}

	return d.uvm.sendPendingAccessCounterReset()
}
