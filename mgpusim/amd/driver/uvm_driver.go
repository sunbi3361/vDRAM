package driver

// sbin_codex: driver-side plumbing of the UVM control plane. Every message
// travels through the GPU Command Processor; the driver never addresses a
// GPU-internal component directly.

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// Handle dispatches UVM scheduled events. Other events fall through to the
// default TickingComponent handler.
func (d *Driver) Handle(event sim.Event) error {
	switch event := event.(type) {
	case *faultHandlingCompleteEvent:
		if d.uvm != nil {
			d.uvm.handleFaultReady(event.faultID)
		}

		d.TickLater()

		return nil
	case *migrationCompleteEvent:
		if d.uvm != nil {
			d.uvm.completeMigration(event.migrationID)
		}

		d.TickLater()

		return nil
	case *remoteMapCompleteEvent: // sbin_claude_uvm
		if d.uvm != nil {
			d.uvm.completeRemoteMap(event.region)
		}

		d.TickLater()

		return nil
	default:
		return d.TickingComponent.Handle(event)
	}
}

// RegisterUVMGPU records the GPU-side UVM control endpoint of one device. The
// devices are registered in order, so index i is device i+1.
func (d *Driver) RegisterUVMGPU(port sim.RemotePort) {
	d.uvmGPUPorts = append(d.uvmGPUPorts, port)
}

// UVMEnabled reports whether UVM is active on the driver.
func (d *Driver) UVMEnabled() bool {
	return d.uvm != nil && d.uvm.config.Enabled
}

// UVMStats returns a snapshot of the UVM statistics. When UVM is disabled it
// returns a zero-valued snapshot.
func (d *Driver) UVMStats() UVMStats {
	if d.uvm == nil {
		return UVMStats{}
	}

	d.uvm.stateMu.RLock() // sbin_codex: report a coherent snapshot under parallel execution.
	defer d.uvm.stateMu.RUnlock()

	stats := d.uvm.stats
	stats.Enabled = d.uvm.config.Enabled
	stats.Ideal = d.uvm.config.Ideal
	stats.GPUResidentBytes = d.uvm.stats.GPUResidentPages * d.uvm.config.PageSize

	return stats
}

// parseFromUVM consumes one message from the UVM control port.
func (d *Driver) parseFromUVM() bool {
	if d.uvmPort == nil || d.uvm == nil {
		return false
	}

	msg := d.uvmPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *vm.PageFaultReq:
		d.uvmPort.RetrieveIncoming()
		d.uvm.onPageFault(msg.PID, msg.VAddr, msg.DeviceID, msg.IsWrite)

		return true
	case *vm.AccessCounterNotifyReq:
		d.uvmPort.RetrieveIncoming()
		d.uvm.onAccessCounterNotify(msg.PID, msg.RegionBase, msg.DeviceID)

		return true
	case *vm.UVMTLBInvalidateRsp:
		d.uvmPort.RetrieveIncoming()
		d.uvm.onControlRsp(msg.RespondTo)

		return true
	case *protocol.UVMCacheRangeFlushRsp:
		d.uvmPort.RetrieveIncoming()
		d.uvm.onControlRsp(msg.RespondTo)

		return true
	case *protocol.UVMRemoteDrainRsp:
		d.uvmPort.RetrieveIncoming()
		d.uvm.onControlRsp(msg.RespondTo)

		return true
	default:
		// An unknown message must not wedge the port.
		d.uvmPort.RetrieveIncoming()

		return true
	}
}

// sendUVMControl drains the UVM control send queue.
func (d *Driver) sendUVMControl() bool {
	if d.uvm == nil {
		return false
	}

	return d.uvm.sendPendingControl()
}

// processUVMDMAReturn consumes the MemCopy responses of UVM migrations before
// the user-facing copy middleware sees them.
func (d *Driver) processUVMDMAReturn() bool {
	if d.uvm == nil {
		return false
	}

	msg := d.gpuPort.PeekIncoming()
	if msg == nil {
		return false
	}

	rsp, ok := msg.(*sim.GeneralRsp)
	if !ok {
		return false
	}

	return d.ClaimUVMDMAReturn(rsp.OriginalReq)
}

// ClaimUVMDMAReturn consumes the head of the GPU port when it answers a
// MemCopy the UVM manager issued for a migration, and reports whether it did.
//
// Running before the copy middleware is not enough on its own to keep those
// responses away from it: the middleware peeks the port again, and by then the
// head may be a migration response that was not there a moment earlier. So the
// middleware asks here too, and ownership — not tick order — decides who
// consumes the response. // sbin_codex
func (d *Driver) ClaimUVMDMAReturn(original sim.Msg) bool {
	if d.uvm == nil {
		return false
	}

	switch original := original.(type) {
	case *protocol.MemCopyH2DReq:
		if !d.uvm.ownsDMARequest(original.ID) {
			return false
		}

		d.gpuPort.RetrieveIncoming()
		d.uvm.onMigrationDMADone(original.ID, nil, original.DstAddress)

		return true
	case *protocol.MemCopyD2HReq:
		if !d.uvm.ownsDMARequest(original.ID) {
			return false
		}

		d.gpuPort.RetrieveIncoming()
		d.uvm.onMigrationDMADone(
			original.ID, original.DstBuffer, original.SrcAddress)

		return true
	default:
		return false
	}
}

// ownsDMARequest reports whether a MemCopy request belongs to a UVM migration.
func (m *UVMManager) ownsDMARequest(reqID string) bool {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()

	_, found := m.dmaToMigration[reqID]

	return found
}

// resetUVMAccessCounters clears every remote-access counter of one GPU. The
// driver issues it at a kernel boundary (spec 14.2). // sbin_codex
func (d *Driver) resetUVMAccessCounters(pid vm.PID, deviceID uint64) {
	if d.uvm == nil || !d.uvm.config.AccessCounterEnabled || d.uvmPort == nil {
		return
	}

	d.uvm.stateMu.Lock()
	defer d.uvm.stateMu.Unlock()

	dst := d.uvm.uvmControlPort(deviceID)
	if dst == "" {
		return
	}

	req := vm.NewAccessCounterResetReq(d.uvmPort.AsRemote(), dst)
	req.PID = pid
	req.DeviceID = deviceID
	req.ResetAll = true

	d.uvm.stats.AccessCounterResets++
	d.uvm.enqueueControlLocked(req)
}
