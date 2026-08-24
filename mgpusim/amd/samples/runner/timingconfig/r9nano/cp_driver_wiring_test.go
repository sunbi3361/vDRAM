package r9nano

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

// sbin_codex: lock the CP-to-driver control-response route used by UVM drains.
var _ = Describe("R9 Nano CP driver wiring", func() {
	It("targets the registered driver port when sending control responses", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("cp-driver")
		globalStorage := mem.NewStorage(4 * mem.GB)
		gpuDriver := driver.MakeBuilder().
			WithEngine(testSimulation.GetEngine()).
			WithPageTable(vm.NewPageTable(12)).
			WithGPUPageTables([]vm.PageTable{gpuPageTable}).
			WithLog2PageSize(12).
			WithGlobalStorage(globalStorage).
			Build("Driver")
		driverPort := gpuDriver.GetPortByName("GPU")

		// When
		MakeBuilder().
			WithSimulation(testSimulation).
			WithNumShaderArray(1).
			WithNumCUPerShaderArray(1).
			WithNumMemoryBank(1).
			WithL2CacheSize(32 * mem.KB).
			WithDramSize(2 * mem.GB).
			WithGlobalStorage(globalStorage).
			WithMMU(cpuMMU).
			WithGPUID(1).
			WithPageTable(gpuPageTable).
			WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
				BankSize:   2 * mem.GB,
				LowModules: []sim.RemotePort{"CPU"},
			}).
			WithDriverPort(driverPort).
			Build("GPU")

		// Then
		commandProcessor := testSimulation.GetComponentByName(
			"GPU.CommandProcessor").(*cp.CommandProcessor)
		Expect(commandProcessor.Driver).To(BeIdenticalTo(driverPort))
	})
})
