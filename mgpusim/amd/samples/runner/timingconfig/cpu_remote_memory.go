package timingconfig

// sbin_codex: host memory as seen by the GPU over PCIe.
//
// Pre-edit design (commented per AGENTS.md convention): the UVM access counter
// used to live here, on the CPU side of the root complex, and notified the
// driver directly. The specification places the counter on the GPU, right
// after translation identifies a request as CPU-remote, and routes its
// notifications through the Command Processor (spec 14, 16). The CPU endpoint
// is therefore a plain memory controller again.

import (
	"github.com/sarchlab/akita/v4/mem/idealmemcontroller"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
)

func (b *Builder) buildCPURemoteMemory(_ *driver.Driver) {
	cpuMemory := idealmemcontroller.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(sim.GHz).
		WithStorage(b.globalStorage).
		Build("CPU.Memory")

	b.simulation.RegisterComponent(cpuMemory)

	b.cpuRemoteTop = cpuMemory.GetPortByName("Top")
}
