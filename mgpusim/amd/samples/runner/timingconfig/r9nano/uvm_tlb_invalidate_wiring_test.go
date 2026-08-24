package r9nano

// sbin_codex: UVM range TLB invalidation wiring contract test (plan todo 14
// of mgpusim-uvm-manager). Proves the r9nano baseline builder populates the
// CP's TLB endpoint set with every private L1V/L1S/L1I TLB plus the shared L2
// TLB, and registers the exact same set with the GMMU invalidation
// coordinator (uvm-manager.md §21.1).

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

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

	// Baseline endpoint set: private L1V (per CU), L1S, L1I TLBs plus the
	// shared L2 TLB.
	want := map[string]bool{
		"GPU.SA[0].L1VTLB[0]": false,
		"GPU.SA[0].L1VTLB[1]": false,
		"GPU.SA[0].L1STLB":    false,
		"GPU.SA[0].L1ITLB":    false,
		"GPU.L2TLB":           false,
	}
	if len(commandProcessor.TLBs) != len(want) {
		t.Fatalf("baseline must wire exactly %d TLB endpoints, got %d",
			len(want), len(commandProcessor.TLBs))
	}
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if _, ok := want[name]; !ok {
			t.Fatalf("baseline TLB endpoint %s is not in the expected set", name)
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("baseline TLB endpoint %s is missing", name)
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