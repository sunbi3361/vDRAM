package driver

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
)

// enqueueShootDownReqs is the shared synchronized producer for legacy page
// migration and UVM migration/eviction shootdowns. // sbin_codex
func (d *Driver) enqueueShootDownReqs( // sbin_codex
	pid vm.PID,
	virtualAddresses []uint64,
	accessingGPUs []uint64,
) {
	for _, gpuID := range accessingGPUs {
		request := protocol.NewShootdownCommand(
			d.gpuPort,
			d.GPUs[gpuID-1],
			virtualAddresses,
			pid,
		)
		d.enqueueRequestsToSend(request)
	}
}
