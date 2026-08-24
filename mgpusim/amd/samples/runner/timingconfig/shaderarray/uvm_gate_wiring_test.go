package shaderarray

// sbin_codex: baseline UVM access-gate wiring test (plan todo 9 of
// mgpusim-uvm-manager). Proves the baseline L1V/L1S pre-cache address
// translators are wired as UVM access gates: each carries a nonzero gate ID
// and the gate control ports are exposed for BlockRange commands.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/simulation"
)

// TestBaselineUVMAccessGateWiring proves the baseline data path wires every
// L1V/L1S pre-cache address translator as a UVM access gate with a
// deterministic gate ID and exposes the gate control ports.
func TestBaselineUVMAccessGateWiring(t *testing.T) {
	outputPrefix := filepath.Join(t.TempDir(), "baseline-gate-shader-array")
	testSimulation := simulation.MakeBuilder().
		WithoutMonitoring().
		WithOutputFileName(outputPrefix).
		Build()
	t.Cleanup(func() {
		testSimulation.Terminate()
		artifacts, err := filepath.Glob(outputPrefix + "_*.sqlite3")
		if err != nil {
			t.Fatalf("glob artifacts: %v", err)
		}
		for _, artifact := range artifacts {
			_ = os.Remove(artifact)
		}
	})

	domain := MakeBuilder().
		WithSimulation(testSimulation).
		WithNumCUs(2).
		WithL1AddressMapper(mem.NewInterleavedAddressPortMapper(64)).
		WithL1TLBAddressMapper(&mem.SinglePortMapper{}).
		Build("ShaderArray")

	for i := 0; i < 2; i++ {
		at := testSimulation.GetComponentByName(
			fmt.Sprintf("ShaderArray.L1VAddrTrans[%d]", i)).(*addresstranslator.Comp)
		want := baselineAccessGateIDBase + uint64(i)
		if at.GetUVMGateID() != want {
			t.Fatalf("L1V[%d] must be wired as gate %d, got %d",
				i, want, at.GetUVMGateID())
		}
	}

	l1s := testSimulation.GetComponentByName(
		"ShaderArray.L1SAddrTrans").(*addresstranslator.Comp)
	if want := baselineAccessGateIDBase + 2; l1s.GetUVMGateID() != want {
		t.Fatalf("L1S must be wired as gate %d, got %d", want, l1s.GetUVMGateID())
	}

	for _, portName := range []string{
		"L1VAddrTransCtrl[0]",
		"L1VAddrTransCtrl[1]",
		"L1SAddrTransCtrl",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("gate control port %s must be exposed: %v",
						portName, r)
				}
			}()
			_ = domain.GetPortByName(portName)
		}()
	}
}
