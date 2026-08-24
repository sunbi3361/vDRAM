package virtualcaching

// sbin_codex: UVM virtual-caching topology wiring contract test (plan todo
// 24 of mgpusim-uvm-manager). Proves the virtual-caching selector wires the
// private L1I plus shared L2 TLB controls and the separate L1V/L1S gate
// controls with the deterministic gate IDs, and exposes no nonexistent
// L1V/L1S TLB endpoint.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/shaderarray"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

// TestUVMVirtualTLBAndGateSet proves the virtual-caching topology wires the
// private L1I plus shared L2 TLB controls and the separate L1V/L1S gate
// controls with the deterministic gate IDs.
func TestUVMVirtualTLBAndGateSet(t *testing.T) {
	outputPrefix := filepath.Join(t.TempDir(), "uvm-virtual-set")
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

	gpuPageTable := vm.NewPageTable(12)
	cpuMMU := mmu.MakeBuilder().
		WithEngine(testSimulation.GetEngine()).
		WithFreq(1 * sim.GHz).
		WithLog2PageSize(12).
		WithPageWalkingLatency(100).
		WithPageTable(vm.NewPageTable(12)).
		Build("MMU")
	testSimulation.RegisterComponent(cpuMMU)

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

	wantTLBs := map[string]bool{
		"GPU.SA[0].L1ITLB": false,
		"GPU.L2TLB":        false,
	}
	if len(commandProcessor.TLBs) != len(wantTLBs) {
		t.Fatalf("virtual must wire exactly %d TLB controls, got %d",
			len(wantTLBs), len(commandProcessor.TLBs))
	}
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if _, ok := wantTLBs[name]; !ok {
			t.Fatalf("virtual TLB control %s is not in the expected set", name)
		}
		wantTLBs[name] = true
	}
	for name, seen := range wantTLBs {
		if !seen {
			t.Fatalf("virtual TLB control %s is missing", name)
		}
	}

	wantGates := map[string]bool{
		"GPU.SA[0].L1IAddrTrans": false,
		"GPU.SA[0].L1VGate[0]":   false,
		"GPU.SA[0].L1VGate[1]":   false,
		"GPU.SA[0].L1SGate":      false,
	}
	if len(commandProcessor.PreCacheTranslators.Ports) != len(wantGates) {
		t.Fatalf("virtual must wire exactly %d gate controls, got %d",
			len(wantGates), len(commandProcessor.PreCacheTranslators.Ports))
	}
	for _, port := range commandProcessor.PreCacheTranslators.Ports {
		name := port.Component().Name()
		if _, ok := wantGates[name]; !ok {
			t.Fatalf("virtual gate control %s is not in the expected set", name)
		}
		wantGates[name] = true
	}
	for name, seen := range wantGates {
		if !seen {
			t.Fatalf("virtual gate control %s is missing", name)
		}
	}

	for i := 0; i < 2; i++ {
		gate := testSimulation.GetComponentByName(
			fmt.Sprintf("GPU.SA[0].L1VGate[%d]", i)).(*shaderarray.VirtualAccessGate)
		if want := shaderarray.VirtualAccessGateIDBase + uint64(i); gate.GetUVMGateID() != want {
			t.Fatalf("L1V gate[%d] must be gate %d, got %d", i, want, gate.GetUVMGateID())
		}
	}
	l1sGate := testSimulation.GetComponentByName(
		"GPU.SA[0].L1SGate").(*shaderarray.VirtualAccessGate)
	if want := shaderarray.VirtualAccessGateIDBase + 2; l1sGate.GetUVMGateID() != want {
		t.Fatalf("L1S gate must be gate %d, got %d", want, l1sGate.GetUVMGateID())
	}
}