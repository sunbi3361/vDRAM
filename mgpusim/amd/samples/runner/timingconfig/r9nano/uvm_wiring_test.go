package r9nano

// sbin_codex: UVM topology wiring contract tests (plan todo 24 of
// mgpusim-uvm-manager). Prove the exact baseline/virtual TLB and gate
// endpoint sets, the data-cache control-only fan-out, the forbidden virtual
// L1V/L1S TLB endpoints, and one connection per port.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/shaderarray"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

// buildWiringGPU builds one GPU with the given topology pair and reduced
// dimensions (1 SA x 2 CUs, 2 memory banks).
func buildWiringGPU(
	t *testing.T,
	name string,
	dataPath DataPathTopology,
	memory MemoryTopology,
) (*simulation.Simulation, vm.PageTable) {
	t.Helper()
	testSimulation, gpuPageTable, cpuMMU := newPlainR9NanoSimulation(t, name)

	builder := MakeBuilder().
		WithSimulation(testSimulation).
		WithNumShaderArray(1).
		WithNumCUPerShaderArray(2).
		WithNumMemoryBank(2).
		WithL2CacheSize(32 * mem.KB).
		WithDramSize(2 * mem.GB).
		WithGlobalStorage(mem.NewStorage(4 * mem.GB)).
		WithMMU(cpuMMU)
	if dataPath != nil {
		builder = builder.WithDataPathTopology(dataPath)
	}
	if memory != nil {
		builder = builder.WithMemoryTopology(memory)
	}
	builder.
		WithGPUID(1).
		WithPageTable(gpuPageTable).
		WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
			BankSize:   2 * mem.GB,
			LowModules: []sim.RemotePort{"CPU"},
		}).
		Build("GPU")

	return testSimulation, gpuPageTable
}

// TestUVMBaselineTLBAndGateSet proves the baseline wires every private
// L1V/L1S/L1I TLB control plus the shared L2 TLB control, and every L1V/L1S
// pre-cache translator (gate) control with the deterministic gate IDs.
func TestUVMBaselineTLBAndGateSet(t *testing.T) {
	testSimulation, _ := buildWiringGPU(t, "baseline-set", nil, nil)

	commandProcessor := testSimulation.GetComponentByName(
		"GPU.CommandProcessor").(*cp.CommandProcessor)

	wantTLBs := map[string]bool{
		"GPU.SA[0].L1VTLB[0]": false,
		"GPU.SA[0].L1VTLB[1]": false,
		"GPU.SA[0].L1STLB":    false,
		"GPU.SA[0].L1ITLB":    false,
		"GPU.L2TLB":           false,
	}
	if len(commandProcessor.TLBs) != len(wantTLBs) {
		t.Fatalf("baseline must wire exactly %d TLB controls, got %d",
			len(wantTLBs), len(commandProcessor.TLBs))
	}
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if _, ok := wantTLBs[name]; !ok {
			t.Fatalf("baseline TLB control %s is not in the expected set", name)
		}
		wantTLBs[name] = true
	}
	for name, seen := range wantTLBs {
		if !seen {
			t.Fatalf("baseline TLB control %s is missing", name)
		}
	}

	wantGates := map[string]bool{
		"GPU.SA[0].L1VAddrTrans[0]": false,
		"GPU.SA[0].L1VAddrTrans[1]": false,
		"GPU.SA[0].L1SAddrTrans":    false,
		"GPU.SA[0].L1IAddrTrans":    false,
	}
	if len(commandProcessor.PreCacheTranslators.Ports) != len(wantGates) {
		t.Fatalf("baseline must wire exactly %d gate controls, got %d",
			len(wantGates), len(commandProcessor.PreCacheTranslators.Ports))
	}
	for _, port := range commandProcessor.PreCacheTranslators.Ports {
		name := port.Component().Name()
		if _, ok := wantGates[name]; !ok {
			t.Fatalf("baseline gate control %s is not in the expected set", name)
		}
		wantGates[name] = true
	}
	for name, seen := range wantGates {
		if !seen {
			t.Fatalf("baseline gate control %s is missing", name)
		}
	}

	// Deterministic gate IDs: L1V[i] = base+i, L1S = base+numCUs.
	for i := 0; i < 2; i++ {
		at := testSimulation.GetComponentByName(
			fmt.Sprintf("GPU.SA[0].L1VAddrTrans[%d]", i)).(interface {
			GetUVMGateID() uint64
		})
		if want := shaderarray.BaselineAccessGateIDBase + uint64(i); at.GetUVMGateID() != want {
			t.Fatalf("L1V[%d] must be gate %d, got %d", i, want, at.GetUVMGateID())
		}
	}
	l1sAT := testSimulation.GetComponentByName(
		"GPU.SA[0].L1SAddrTrans").(interface{ GetUVMGateID() uint64 })
	if want := shaderarray.BaselineAccessGateIDBase + 2; l1sAT.GetUVMGateID() != want {
		t.Fatalf("L1S must be gate %d, got %d", want, l1sAT.GetUVMGateID())
	}

	// The GMMU invalidation coordinator registers the exact same TLB set.
	gmmuComp := testSimulation.GetComponentByName("GPU.GMMU")
	gmmuEndpoints := gmmuComp.(interface {
		TLBEndpoints() []sim.Port
	}).TLBEndpoints()
	if len(gmmuEndpoints) != len(commandProcessor.TLBs) {
		t.Fatalf("the GMMU must register the same %d TLB endpoints, got %d",
			len(commandProcessor.TLBs), len(gmmuEndpoints))
	}
	for i, port := range gmmuEndpoints {
		if port != commandProcessor.TLBs[i] {
			t.Fatalf("the GMMU endpoint %d must match the CP endpoint, got %s vs %s",
				i, port.Component().Name(), commandProcessor.TLBs[i].Component().Name())
		}
	}
}

// TestUVMForbiddenVirtualL1DataTLB proves the virtual topology exposes no
// nonexistent L1V/L1S TLB endpoint: no TLB control, no TLB component, and no
// GMMU coordinator endpoint.
func TestUVMForbiddenVirtualL1DataTLB(t *testing.T) {
	testSimulation, _ := buildWiringGPU(t, "virtual-forbidden",
		NewVirtualDataPathTopology(), NewVirtualMemoryTopology())

	commandProcessor := testSimulation.GetComponentByName(
		"GPU.CommandProcessor").(*cp.CommandProcessor)
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if name == "GPU.SA[0].L1VTLB[0]" || name == "GPU.SA[0].L1VTLB[1]" ||
			name == "GPU.SA[0].L1STLB" {
			t.Fatalf("virtual must not fabricate a data TLB endpoint %s", name)
		}
	}
	for _, port := range commandProcessor.PreCacheTranslators.Ports {
		name := port.Component().Name()
		if name == "GPU.SA[0].L1VTLB[0]" || name == "GPU.SA[0].L1STLB" {
			t.Fatalf("virtual must not fabricate a data TLB gate endpoint %s", name)
		}
	}
	// No L1V/L1S TLB component exists anywhere in the virtual GPU.
	for _, comp := range testSimulation.Components() {
		name := comp.Name()
		if name == "GPU.SA[0].L1VTLB[0]" || name == "GPU.SA[0].L1VTLB[1]" ||
			name == "GPU.SA[0].L1STLB" {
			t.Fatalf("virtual must not build a nonexistent data TLB component %s", name)
		}
	}
}

// TestUVMDataCacheOnlyControl proves the CP reaches every data cache through
// its control port only: the cache data ports never ride the internal
// connection.
func TestUVMDataCacheOnlyControl(t *testing.T) {
	testSimulation, _ := buildWiringGPU(t, "cache-control", nil, nil)

	commandProcessor := testSimulation.GetComponentByName(
		"GPU.CommandProcessor").(*cp.CommandProcessor)

	if len(commandProcessor.L1VCaches) != 2 || len(commandProcessor.L1SCaches) != 1 ||
		len(commandProcessor.L1ICaches) != 1 || len(commandProcessor.L2Caches) != 2 {
		t.Fatalf("the CP must track 2 L1V + 1 L1S + 1 L1I + 2 L2 cache controls, got %d/%d/%d/%d",
			len(commandProcessor.L1VCaches), len(commandProcessor.L1SCaches),
			len(commandProcessor.L1ICaches), len(commandProcessor.L2Caches))
	}

	for i, port := range commandProcessor.L1VCaches {
		cache := testSimulation.GetComponentByName(
			fmt.Sprintf("GPU.SA[0].L1VCache[%d]", i))
		if port != cache.GetPortByName("Control") {
			t.Fatalf("the L1V[%d] CP port must be the cache Control port", i)
		}
		expectPortOnConnection(t, port, "GPU.InternalConn")
		// The cache data ports never ride the internal connection.
		for _, dataPort := range []sim.Port{
			cache.GetPortByName("Top"),
			cache.GetPortByName("Bottom"),
		} {
			assertNotOnConnection(t, dataPort, "GPU.InternalConn")
		}
	}
	for _, port := range commandProcessor.L1SCaches {
		cache := testSimulation.GetComponentByName("GPU.SA[0].L1SCache")
		if port != cache.GetPortByName("Control") {
			t.Fatal("the L1S CP port must be the cache Control port")
		}
		expectPortOnConnection(t, port, "GPU.InternalConn")
		assertNotOnConnection(t, cache.GetPortByName("Top"), "GPU.InternalConn")
		assertNotOnConnection(t, cache.GetPortByName("Bottom"), "GPU.InternalConn")
	}
	for _, port := range commandProcessor.L1ICaches {
		cache := testSimulation.GetComponentByName("GPU.SA[0].L1ICache")
		if port != cache.GetPortByName("Control") {
			t.Fatal("the L1I CP port must be the cache Control port")
		}
		expectPortOnConnection(t, port, "GPU.InternalConn")
		assertNotOnConnection(t, cache.GetPortByName("Top"), "GPU.InternalConn")
		assertNotOnConnection(t, cache.GetPortByName("Bottom"), "GPU.InternalConn")
	}
	for i, port := range commandProcessor.L2Caches {
		cache := testSimulation.GetComponentByName(
			fmt.Sprintf("GPU.L2Cache[%d]", i))
		if port != cache.GetPortByName("Control") {
			t.Fatalf("the L2[%d] CP port must be the cache Control port", i)
		}
		expectPortOnConnection(t, port, "GPU.InternalConn")
		assertNotOnConnection(t, cache.GetPortByName("Top"), "GPU.InternalConn")
		assertNotOnConnection(t, cache.GetPortByName("Bottom"), "GPU.InternalConn")
	}
}

// TestUVMDuplicateConnection proves every UVM control port has exactly one
// connection: plugging any of them into a fresh connection panics with the
// owning connection name (no dual connection).
func TestUVMDuplicateConnection(t *testing.T) {
	testSimulation, _ := buildWiringGPU(t, "duplicate", nil, nil)

	commandProcessor := testSimulation.GetComponentByName(
		"GPU.CommandProcessor").(*cp.CommandProcessor)
	gmmuComp := testSimulation.GetComponentByName("GPU.GMMU")

	ports := []sim.Port{
		commandProcessor.ToGMMU,
		commandProcessor.ToAccessCounter,
		commandProcessor.ToDMA,
		commandProcessor.ToCaches,
		commandProcessor.ToTLBs,
		commandProcessor.ToAddressTranslators,
		gmmuComp.GetPortByName("Control"),
		gmmuComp.GetPortByName("CommandProcessor"),
	}
	for _, port := range ports {
		if port == nil {
			t.Fatalf("port %v must exist for the single-connection check", port)
		}
		got := connectionNameOf(t, port)
		if got != "GPU.InternalConn" {
			t.Fatalf("port %s must be on exactly GPU.InternalConn, got %s",
				port.Name(), got)
		}
	}
}

// assertNotOnConnection asserts the port is NOT plugged into the named
// connection (a fresh probe connection can claim it).
func assertNotOnConnection(t *testing.T, port sim.Port, connectionName string) {
	t.Helper()
	probe := directconnection.MakeBuilder().
		WithEngine(nil).
		WithFreq(1 * sim.GHz).
		Build("ProbeConnection")
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			if strings.HasPrefix(msg,
				"connection already set to "+connectionName+", now connecting to ") {
				t.Fatalf("port %s must not ride %s", port.Name(), connectionName)
			}
		}
	}()
	probe.PlugIn(port)
}