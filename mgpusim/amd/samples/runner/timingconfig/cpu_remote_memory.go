package timingconfig

import (
	"github.com/sarchlab/akita/v4/mem/idealmemcontroller"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/timing/accesscounter"
)

func (b *Builder) buildCPURemoteMemory(gpuDriver *driver.Driver) { // sbin_codex
	cpuMemory := idealmemcontroller.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(sim.GHz).
		WithStorage(b.globalStorage).
		Build("CPU.Memory")
	counterBuilder := accesscounter.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(sim.GHz).
		WithThreshold(b.uvmACThresh).
		WithBottomDestination(cpuMemory.GetPortByName("Top").AsRemote())
	if b.uvmEnabled {
		counterBuilder = counterBuilder.WithDriverDestination(
			gpuDriver.GetPortByName("UVM").AsRemote())
	}
	counter := counterBuilder.Build("CPU.AccessCounter")
	if b.uvmEnabled { // sbin_codex: resolve the driver/counter build cycle.
		gpuDriver.SetAccessCounterResetDestination(counter.Top.AsRemote())
	}
	connection := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(sim.GHz).
		Build("CPU.AccessCounterToMemory")
	connection.PlugIn(counter.Bottom)
	connection.PlugIn(cpuMemory.GetPortByName("Top"))

	b.simulation.RegisterComponent(cpuMemory)
	b.simulation.RegisterComponent(counter)
	b.simulation.RegisterComponent(connection)
	b.cpuRemoteTop = counter.Top
}
