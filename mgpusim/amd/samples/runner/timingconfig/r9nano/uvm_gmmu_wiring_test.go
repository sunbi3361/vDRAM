package r9nano

// sbin_codex: UVM GMMU control wiring contract test (plan todo 8 of
// mgpusim-uvm-manager). Proves the r9nano builder wires the CP to the GMMU:
// the CP exposes a ToGMMU port, the GMMU control and command-processor ports
// share the internal connection, and the CP pre-registers the GMMU gate ID.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/gmmu"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

func newPlainR9NanoSimulation(
	t *testing.T, name string,
) (*simulation.Simulation, vm.PageTable, *mmu.Comp) {
	t.Helper()
	outputPrefix := filepath.Join(t.TempDir(), name)
	testSimulation := simulation.MakeBuilder().
		WithoutMonitoring().
		WithOutputFileName(outputPrefix).
		Build()
	t.Cleanup(func() {
		testSimulation.Terminate()
		artifacts, _ := filepath.Glob(outputPrefix + "_*.sqlite3")
		for _, artifact := range artifacts {
			os.Remove(artifact)
		}
	})

	cpuPageTable := vm.NewPageTable(12)
	gpuPageTable := vm.NewPageTable(12)
	cpuMMU := mmu.MakeBuilder().
		WithEngine(testSimulation.GetEngine()).
		WithFreq(1 * sim.GHz).
		WithLog2PageSize(12).
		WithPageWalkingLatency(100).
		WithPageTable(cpuPageTable).
		Build("MMU")
	testSimulation.RegisterComponent(cpuMMU)

	return testSimulation, gpuPageTable, cpuMMU
}

func TestUVMGMMUControlWiring(t *testing.T) {
	testSimulation, gpuPageTable, cpuMMU := newPlainR9NanoSimulation(t, "uvm-wiring")

	MakeBuilder().
		WithSimulation(testSimulation).
		WithNumShaderArray(1).
		WithNumCUPerShaderArray(1).
		WithNumMemoryBank(2).
		WithL2CacheSize(32 * mem.KB).
		WithDramSize(2 * mem.GB).
		WithGlobalStorage(mem.NewStorage(4 * mem.GB)).
		WithMMU(cpuMMU).
		WithGPUID(1).
		WithPageTable(gpuPageTable).
		WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
			BankSize:   2 * mem.GB,
			LowModules: []sim.RemotePort{"CPU"},
		}).
		Build("GPU")

	commandProcessor := testSimulation.GetComponentByName(
		"GPU.CommandProcessor").(*cp.CommandProcessor)
	if commandProcessor.ToGMMU == nil {
		t.Fatal("the CP must expose a ToGMMU port")
	}
	gmmuComp := testSimulation.GetComponentByName(
		"GPU.GMMU").(*gmmu.Comp)

	// The CP ToGMMU port, the GMMU control port, and the GMMU
	// command-processor port all share the internal connection.
	probeConnection := directconnection.MakeBuilder().
		WithEngine(testSimulation.GetEngine()).
		WithFreq(1 * sim.GHz).
		Build("ProbeConnection")
	for _, port := range []sim.Port{
		commandProcessor.ToGMMU,
		gmmuComp.GetPortByName("Control"),
		gmmuComp.GetPortByName("CommandProcessor"),
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("port %s must already be connected to GPU.InternalConn",
						port.Name())
				}
			}()
			probeConnection.PlugIn(port)
		}()
	}

	// The CP pre-registers the exact GMMU gate ID for block commands.
	found := false
	for _, gateID := range commandProcessor.UVMGateIDs {
		if gateID == gmmu.TranslationGateID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the CP must pre-register the GMMU gate ID %d, got %v",
			gmmu.TranslationGateID, commandProcessor.UVMGateIDs)
	}
}
