package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// Handle dispatches UVM scheduled events. All other events fall through to the
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
	case *idealMigrationCompleteEvent:
		if d.uvm != nil {
			d.uvm.completeMigration(event.migrationID)
		}
		d.TickLater()
		return nil
	default:
		return d.TickingComponent.Handle(event)
	}
}

// uvmReplyFault responds to a GPU translation request that was waiting on a
// page fault. The GPU GMMU re-reads the page table and completes the pending
// translation.
func (d *Driver) uvmReplyFault(waiter FaultWaiter) {
	if d.uvm == nil || d.uvmPort == nil || waiter.ReplyTo == "" {
		return
	}
	rsp := vm.NewPageFaultRsp(
		d.uvmPort.AsRemote(), waiter.ReplyTo, waiter.RequestID)
	rsp.PID = waiter.PID
	rsp.VAddr = waiter.VAddr
	if err := d.uvmPort.Send(rsp); err == nil {
		d.uvm.stats.pageFaultReplies++
	}
}

// UVMEnabled reports whether UVM demand-paging is active on the driver.
func (d *Driver) UVMEnabled() bool {
	return d.uvm != nil && d.uvm.config.Enabled
}

// UVMStats returns a snapshot of the UVM statistics. When UVM is disabled it
// returns a zero-valued snapshot.
func (d *Driver) UVMStats() UVMStats {
	if d.uvm == nil {
		return UVMStats{}
	}
	stats := d.uvm.stats
	stats.Enabled = d.uvm.config.Enabled
	stats.Ideal = d.uvm.config.Ideal
	stats.GPUResidentBytes = d.uvm.stats.GPUResidentPages * d.uvm.config.PageSize
	return stats
}

// processUVMFaultReq receives a PageFaultReq from a GPU GMMU and feeds it into
// the UVM manager's access-recording / fault path.
func (d *Driver) processUVMFaultReq(req *vm.PageFaultReq) {
	if d.uvm == nil {
		return
	}
	d.uvm.onManagedAccess(
		req.PID, req.VAddr, req.DeviceID, req.WaitRequestID, req.Src)
}
