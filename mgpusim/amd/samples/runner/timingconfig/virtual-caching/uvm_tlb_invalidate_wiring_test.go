package virtualcaching

// sbin_codex: UVM range TLB invalidation wiring contract test (plan todo 14
// of mgpusim-uvm-manager). Proves the virtual-caching builder populates the
// CP's TLB endpoint set with the private L1I TLB plus the shared L2 TLB only:
// no fabricated L1V/L1S TLB endpoints (the virtual L1V/L1S data gates are
// handled by BlockRange, not TLB invalidation). The GMMU invalidation
// coordinator registers the exact same set (uvm-manager.md §21.1).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/sim"
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

func TestUVMTLBInvalidateWiring(t *testing.T) {
	testSimulation, gpuPageTable, cpuMMU := newPlainR9NanoSimulation(t, "uvm-tlb-inv")

	MakeBuilder().
		WithSimulation(testSimulation).
		WithNumShaderArray(1).
		WithNumCUPerShaderArray(2).
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

	// Virtual endpoint set: private L1I plus the shared L2 TLB only.
	want := map[string]bool{
		"GPU.SA[0].L1ITLB": false,
		"GPU.L2TLB":        false,
	}
	if len(commandProcessor.TLBs) != len(want) {
		t.Fatalf("virtual must wire exactly %d TLB endpoints, got %d",
			len(want), len(commandProcessor.TLBs))
	}
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if _, ok := want[name]; !ok {
			t.Fatalf("virtual TLB endpoint %s is not in the expected set", name)
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("virtual TLB endpoint %s is missing", name)
		}
	}

	// No fabricated L1V/L1S TLB endpoints in the virtual topology.
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if name == "GPU.SA[0].L1VTLB[0]" || name == "GPU.SA[0].L1VTLB[1]" ||
			name == "GPU.SA[0].L1STLB" {
			t.Fatalf("virtual must not fabricate a data TLB endpoint %s", name)
		}
	}

	// The GMMU invalidation coordinator registers the exact same endpoint set.
	gmmuComp := testSimulation.GetComponentByName("GPU.GMMU")
	gmmuEndpoints := gmmuComp.(interface {
		TLBEndpoints() []sim.Port
	}).TLBEndpoints()
	if len(gmmuEndpoints) != len(want) {
		t.Fatalf("the GMMU must register exactly %d TLB endpoints, got %d",
			len(want), len(gmmuEndpoints))
	}
	for i, port := range gmmuEndpoints {
		if port != commandProcessor.TLBs[i] {
			t.Fatalf("the GMMU endpoint %d must match the CP endpoint, got %s vs %s",
				i, port.Component().Name(), commandProcessor.TLBs[i].Component().Name())
		}
	}
}