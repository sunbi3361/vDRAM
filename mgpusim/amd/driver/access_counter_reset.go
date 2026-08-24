package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// SetAccessCounterResetDestination configures the PCIe counter reset target.
// sbin_codex
func (d *Driver) SetAccessCounterResetDestination(destination sim.RemotePort) {
	if d.uvm == nil {
		return
	}
	d.uvm.stateMu.Lock()
	defer d.uvm.stateMu.Unlock()
	d.uvm.accessCounterResetDestination = destination
}

// AccessCounterResetDestination returns the configured reset target. // sbin_codex
func (d *Driver) AccessCounterResetDestination() sim.RemotePort {
	if d.uvm == nil {
		return ""
	}
	d.uvm.stateMu.RLock()
	defer d.uvm.stateMu.RUnlock()
	return d.uvm.accessCounterResetDestination
}

// queueAccessCounterResets requires stateMu to be held by the caller. // sbin_codex
func (m *UVMManager) queueAccessCounterResets(migration *Migration) {
	if migration.Direction != CPUToGPU {
		return
	}
	for _, page := range migration.Pages {
		key := AccessCounterResetKey{
			PID: page.PID,
			RegionBase: m.config.alignDown(
				page.VAddr, m.config.RegionSize),
		}
		if m.pendingAccessCounterResetKeys[key] {
			continue
		}
		m.pendingAccessCounterResetKeys[key] = true
		m.pendingAccessCounterResets = append(
			m.pendingAccessCounterResets,
			pendingAccessCounterReset{Key: key, DeviceID: migration.DeviceID},
		)
	}
}

func (m *UVMManager) sendPendingAccessCounterReset() bool { // sbin_codex
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if len(m.pendingAccessCounterResets) == 0 ||
		m.d.uvmPort == nil || m.accessCounterResetDestination == "" {
		return false
	}

	pending := m.pendingAccessCounterResets[0]
	request := vm.NewAccessCounterResetReq(
		m.d.uvmPort.AsRemote(), m.accessCounterResetDestination)
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

func (d *Driver) sendPendingAccessCounterReset() bool { // sbin_codex
	if d.uvm == nil {
		return false
	}
	return d.uvm.sendPendingAccessCounterReset()
}
