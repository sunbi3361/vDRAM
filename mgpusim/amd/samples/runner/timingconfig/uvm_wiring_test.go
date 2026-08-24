package timingconfig

// sbin_codex: UVM platform wiring contract tests (plan todo 24 of
// mgpusim-uvm-manager). Prove the full timing platform wires driver↔CP,
// CP↔GMMU, CP↔data caches/DMA/counter, and remote endpoint↔CPU memory for
// both selectors (r9nano, virtual-caching) and both normal/ideal adapters,
// with one connection per port and named failures for every missing wire.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm/gmmu"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rdma"
	"github.com/sarchlab/mgpusim/v4/amd/timing/uvm"
)

// newUVMPlatform builds the full timing platform with UVM enabled and
// returns the simulation and the builder (for the adapter accessor).
func newUVMPlatform(
	t *testing.T, gpuType string, ideal bool,
) (*simulation.Simulation, Builder) {
	t.Helper()
	outputPrefix := filepath.Join(t.TempDir(), "uvm-platform")
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

	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = true
	cfg.Ideal = ideal

	builder := MakeBuilder().
		WithSimulation(testSimulation).
		WithGPUType(gpuType).
		WithUVMConfig(cfg)
	builder.Build()

	return testSimulation, builder
}

// connectionNameOf returns the direct-connection name that a port is already
// plugged into, by probing a fresh connection and recovering the panic.
func connectionNameOf(t *testing.T, port sim.Port) (name string) {
	t.Helper()
	probe := directconnection.MakeBuilder().
		WithEngine(nil).
		WithFreq(1 * sim.GHz).
		Build("ProbeConnection")
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			const prefix = "connection already set to "
			const suffix = ", now connecting to "
			if len(msg) > len(prefix)+len(suffix) &&
				msg[:len(prefix)] == prefix {
				name = msg[len(prefix):]
				if i := indexOf(msg, suffix); i >= 0 {
					name = msg[len(prefix):i]
				}
				return
			}
			t.Fatalf("unexpected probe panic: %v", r)
		}
		t.Fatalf("port %s must already be connected", port.Name())
	}()
	probe.PlugIn(port)
	return
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// expectPortOnConnection asserts the port is plugged into the named
// connection (exactly one connection per port).
func expectPortOnConnection(t *testing.T, port sim.Port, connectionName string) {
	t.Helper()
	if got := connectionNameOf(t, port); got != connectionName {
		t.Fatalf("port %s must be on %s, got %s", port.Name(),
			connectionName, got)
	}
}

// expectUVMPlatformWiring asserts the shared UVM wiring of both selectors:
// driver↔CP over PCIe, CP↔GMMU, CP↔data caches/DMA/counter, and remote
// endpoint↔CPU memory. The selector-specific TLB/gate sets are asserted by
// the caller. // sbin_codex
func expectUVMPlatformWiring(t *testing.T, testSimulation *simulation.Simulation) {
	t.Helper()

	if testSimulation.GetComponentByName("GPU[1].AccessCounter") == nil {
		t.Fatal("missing wire: the AccessCounter must be registered")
	}
	if testSimulation.GetComponentByName("GPU[1].RemoteEndpoint") == nil {
		t.Fatal("missing wire: the RemoteEndpoint must be registered")
	}

	gpuDriver := testSimulation.GetComponentByName("Driver").(*driver.Driver)
	commandProcessor := testSimulation.GetComponentByName(
		"GPU[1].CommandProcessor").(*cp.CommandProcessor)
	gmmuComp := testSimulation.GetComponentByName(
		"GPU[1].GMMU").(*gmmu.Comp)
	counter := testSimulation.GetComponentByName(
		"GPU[1].AccessCounter").(*uvm.AccessCounter)
	endpoint := testSimulation.GetComponentByName(
		"GPU[1].RemoteEndpoint").(*uvm.RemoteEndpoint)
	rdmaEngine := testSimulation.GetComponentByName(
		"GPU[1].RDMA").(*rdma.Comp)
	dmaEngine := testSimulation.GetComponentByName(
		"GPU[1].DMA").(*cp.DMAEngine)

	// driver↔CP: the driver GPU port and the CP ToDriver port are both on
	// the PCIe network (the root complex and the device links).
	driverConn := connectionNameOf(t, gpuDriver.GetPortByName("GPU"))
	if len(driverConn) < 4 || driverConn[:4] != "PCIe" {
		t.Fatalf("missing wire: the driver GPU port must be on the PCIe network, got %q", driverConn)
	}
	cpDriverConn := connectionNameOf(t, commandProcessor.ToDriver)
	if len(cpDriverConn) < 4 || cpDriverConn[:4] != "PCIe" {
		t.Fatalf("missing wire: the CP ToDriver port must be on the PCIe network, got %q", cpDriverConn)
	}

	// CP↔GMMU: the shared ToGMMU seam, the GMMU control target, and the
	// pre-registered translation gate ID.
	if commandProcessor.GMMUControl !=
		gmmuComp.GetPortByName("Control").AsRemote() {
		t.Fatal("missing wire: the CP GMMUControl must target the GMMU control port")
	}
	expectPortOnConnection(t, commandProcessor.ToGMMU, "GPU[1].InternalConn")
	expectPortOnConnection(t, gmmuComp.GetPortByName("Control"), "GPU[1].InternalConn")
	expectPortOnConnection(t, gmmuComp.GetPortByName("CommandProcessor"), "GPU[1].InternalConn")
	foundGate := false
	for _, gateID := range commandProcessor.UVMGateIDs {
		if gateID == gmmu.TranslationGateID {
			foundGate = true
		}
	}
	if !foundGate {
		t.Fatalf("missing wire: the CP must pre-register the GMMU gate ID %d, got %v",
			gmmu.TranslationGateID, commandProcessor.UVMGateIDs)
	}

	// CP↔counter: the counter's ToCP field must BE the CP's ToAccessCounter
	// shared port, which rides the internal connection.
	if counter.ToCP != commandProcessor.ToAccessCounter {
		t.Fatal("missing wire: the AccessCounter ToCP must be the CP's ToAccessCounter shared port")
	}
	expectPortOnConnection(t, commandProcessor.ToAccessCounter, "GPU[1].InternalConn")

	// CP↔data caches: every L1V/L1S/L1I and L2 cache control port is tracked.
	if len(commandProcessor.L1VCaches) != 64 {
		t.Fatalf("missing wire: the CP must track 64 L1V cache controls, got %d",
			len(commandProcessor.L1VCaches))
	}
	if len(commandProcessor.L1SCaches) != 16 || len(commandProcessor.L1ICaches) != 16 {
		t.Fatalf("missing wire: the CP must track 16 L1S and 16 L1I cache controls, got %d/%d",
			len(commandProcessor.L1SCaches), len(commandProcessor.L1ICaches))
	}
	if len(commandProcessor.L2Caches) != 16 {
		t.Fatalf("missing wire: the CP must track 16 L2 cache controls, got %d",
			len(commandProcessor.L2Caches))
	}
	for i, port := range commandProcessor.L1VCaches {
		expectPortOnConnection(t, port, "GPU[1].InternalConn")
		if i == 0 {
			cache := testSimulation.GetComponentByName("GPU[1].SA[0].L1VCache[0]")
			if port != cache.GetPortByName("Control") {
				t.Fatal("missing wire: the L1V cache control must be the cache Control port")
			}
		}
	}
	for _, port := range commandProcessor.L1SCaches {
		expectPortOnConnection(t, port, "GPU[1].InternalConn")
	}
	for _, port := range commandProcessor.L1ICaches {
		expectPortOnConnection(t, port, "GPU[1].InternalConn")
	}
	for _, port := range commandProcessor.L2Caches {
		expectPortOnConnection(t, port, "GPU[1].InternalConn")
	}

	// CP↔DMA: the CP drives the DMA engine through its ToCP seam.
	if commandProcessor.DMAEngine != dmaEngine.ToCP {
		t.Fatal("missing wire: the CP DMAEngine must be the DMA engine's ToCP port")
	}
	expectPortOnConnection(t, dmaEngine.ToCP, "GPU[1].InternalConn")

	// remote endpoint↔CPU memory: the endpoint forwards through the RDMA
	// engine's request seam (modeled PCIe), bypassing the data caches.
	if endpoint.ToRDMA != rdmaEngine.RDMARequestInside {
		t.Fatal("missing wire: the RemoteEndpoint ToRDMA must be the RDMA engine's RDMARequestInside shared port")
	}
	if got := rdmaEngine.RemoteRDMAAddressTable.Find(0x1000); got != sim.RemotePort("CPU") {
		t.Fatalf("missing wire: a CPU PA must map to the CPU memory bank, got %v", got)
	}
}

// TestUVMTopologyR9Nano proves the full r9nano platform wires every UVM
// connection and registers the AccessCounter and RemoteEndpoint components.
func TestUVMTopologyR9Nano(t *testing.T) {
	testSimulation, builder := newUVMPlatform(t, "r9nano", false)
	if got := builder.UVMMode(); got != uvm.ModeNormal {
		t.Fatalf("the r9nano platform must select the normal adapter, got %v", got)
	}

	expectUVMPlatformWiring(t, testSimulation)

	commandProcessor := testSimulation.GetComponentByName(
		"GPU[1].CommandProcessor").(*cp.CommandProcessor)
	// Baseline TLB endpoint set: private L1V/L1S/L1I plus the shared L2 TLB.
	if len(commandProcessor.TLBs) != 64+16+16+1 {
		t.Fatalf("baseline must wire 64 L1V + 16 L1S + 16 L1I + 1 L2 TLB controls, got %d",
			len(commandProcessor.TLBs))
	}
}

// TestUVMTopologyVirtualCaching proves the full virtual-caching platform
// wires every UVM connection with the virtual TLB/gate endpoint sets and no
// fabricated L1V/L1S TLB endpoint.
func TestUVMTopologyVirtualCaching(t *testing.T) {
	testSimulation, builder := newUVMPlatform(t, "virtual-caching", false)
	if got := builder.UVMMode(); got != uvm.ModeNormal {
		t.Fatalf("the virtual-caching platform must select the normal adapter, got %v", got)
	}

	expectUVMPlatformWiring(t, testSimulation)

	commandProcessor := testSimulation.GetComponentByName(
		"GPU[1].CommandProcessor").(*cp.CommandProcessor)
	// Virtual TLB endpoint set: private L1I plus the shared L2 TLB only.
	if len(commandProcessor.TLBs) != 16+1 {
		t.Fatalf("virtual must wire 16 L1I + 1 L2 TLB controls, got %d",
			len(commandProcessor.TLBs))
	}
	for _, port := range commandProcessor.TLBs {
		name := port.Component().Name()
		if name == "GPU[1].SA[0].L1VTLB[0]" || name == "GPU[1].SA[0].L1STLB" {
			t.Fatalf("virtual must not fabricate a data TLB endpoint %s", name)
		}
	}
	// Virtual gates replace the leaf data TLB endpoints: 64 L1V + 16 L1S.
	gateCount := 0
	for _, port := range commandProcessor.PreCacheTranslators.Ports {
		name := port.Component().Name()
		if strings.Contains(name, "L1VGate[") || strings.Contains(name, "L1SGate") {
			gateCount++
		}
	}
	if gateCount != 80 {
		t.Fatalf("virtual must wire 64 L1V gates + 16 L1S gates, got %d", gateCount)
	}
}

// TestUVMRemoteBeforeCache proves the remote endpoint's CPU-memory path
// bypasses the GPU data caches: the endpoint forwards through the RDMA
// engine, whose address table maps CPU PAs to the CPU bank (never to a
// cache), and the endpoint's ToGPU seam is not dual-connected to any cache.
func TestUVMRemoteBeforeCache(t *testing.T) {
	testSimulation, _ := newUVMPlatform(t, "r9nano", false)

	endpoint := testSimulation.GetComponentByName(
		"GPU[1].RemoteEndpoint").(*uvm.RemoteEndpoint)
	rdmaEngine := testSimulation.GetComponentByName(
		"GPU[1].RDMA").(*rdma.Comp)

	if endpoint.ToRDMA != rdmaEngine.RDMARequestInside {
		t.Fatal("the remote endpoint must forward through the RDMA engine (before any cache)")
	}
	if got := rdmaEngine.RemoteRDMAAddressTable.Find(0x1000); got != sim.RemotePort("CPU") {
		t.Fatalf("a CPU PA must route to the CPU bank, got %v", got)
	}

	// The endpoint's ToGPU seam is a free gate-facing port: no dual
	// connection to any cache.
	probe := directconnection.MakeBuilder().
		WithEngine(nil).
		WithFreq(1 * sim.GHz).
		Build("ProbeConnection")
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the endpoint ToGPU must not be dual-connected, got %v", r)
			}
		}()
		probe.PlugIn(endpoint.ToGPU)
	}()
}

// TestUVMIdealTransportSelection proves the normal/ideal adapter selection:
// -uvm selects ModeNormal and -uvm -uvm-ideal selects ModeIdeal, while the
// platform wiring is mode-neutral (both modes wire the same control and
// remote data paths, uvm-manager.md §1.2).
func TestUVMIdealTransportSelection(t *testing.T) {
	normalSim, normalBuilder := newUVMPlatform(t, "r9nano", false)
	if got := normalBuilder.UVMMode(); got != uvm.ModeNormal {
		t.Fatalf("-uvm must select the normal adapter, got %v", got)
	}

	idealSim, idealBuilder := newUVMPlatform(t, "r9nano", true)
	if got := idealBuilder.UVMMode(); got != uvm.ModeIdeal {
		t.Fatalf("-uvm -uvm-ideal must select the ideal adapter, got %v", got)
	}

	// Mode-neutral wiring: the ideal platform wires the exact same seams.
	normalCP := normalSim.GetComponentByName(
		"GPU[1].CommandProcessor").(*cp.CommandProcessor)
	normalCounter := normalSim.GetComponentByName(
		"GPU[1].AccessCounter").(*uvm.AccessCounter)
	normalEndpoint := normalSim.GetComponentByName(
		"GPU[1].RemoteEndpoint").(*uvm.RemoteEndpoint)
	normalRDMA := normalSim.GetComponentByName(
		"GPU[1].RDMA").(*rdma.Comp)

	idealCP := idealSim.GetComponentByName(
		"GPU[1].CommandProcessor").(*cp.CommandProcessor)
	idealCounter := idealSim.GetComponentByName(
		"GPU[1].AccessCounter").(*uvm.AccessCounter)
	idealEndpoint := idealSim.GetComponentByName(
		"GPU[1].RemoteEndpoint").(*uvm.RemoteEndpoint)
	idealRDMA := idealSim.GetComponentByName(
		"GPU[1].RDMA").(*rdma.Comp)

	if idealCounter.ToCP != idealCP.ToAccessCounter ||
		normalCounter.ToCP != normalCP.ToAccessCounter {
		t.Fatal("both modes must wire the counter to the CP ToAccessCounter seam")
	}
	if idealEndpoint.ToRDMA != idealRDMA.RDMARequestInside ||
		normalEndpoint.ToRDMA != normalRDMA.RDMARequestInside {
		t.Fatal("both modes must wire the endpoint to the RDMA engine seam")
	}
	if len(idealCP.TLBs) != len(normalCP.TLBs) {
		t.Fatal("both modes must wire the same TLB endpoint set")
	}
}

// TestUVMMissingWire audits every required UVM wire with a named failure
// message: an omitted wire fails with its exact name.
func TestUVMMissingWire(t *testing.T) {
	testSimulation, _ := newUVMPlatform(t, "r9nano", false)

	commandProcessor := testSimulation.GetComponentByName(
		"GPU[1].CommandProcessor").(*cp.CommandProcessor)
	gmmuComp := testSimulation.GetComponentByName(
		"GPU[1].GMMU").(*gmmu.Comp)
	counter := testSimulation.GetComponentByName(
		"GPU[1].AccessCounter").(*uvm.AccessCounter)
	endpoint := testSimulation.GetComponentByName(
		"GPU[1].RemoteEndpoint").(*uvm.RemoteEndpoint)
	rdmaEngine := testSimulation.GetComponentByName(
		"GPU[1].RDMA").(*rdma.Comp)

	if commandProcessor.GMMUControl == "" {
		t.Fatal("missing wire: CP.GMMUControl")
	}
	if commandProcessor.ToGMMU == nil {
		t.Fatal("missing wire: CP.ToGMMU")
	}
	if commandProcessor.ToAccessCounter == nil {
		t.Fatal("missing wire: CP.ToAccessCounter")
	}
	if counter.ToCP == nil {
		t.Fatal("missing wire: AccessCounter.ToCP")
	}
	if counter.ToCP != commandProcessor.ToAccessCounter {
		t.Fatal("missing wire: AccessCounter.ToCP -> CP.ToAccessCounter")
	}
	if endpoint.ToRDMA == nil {
		t.Fatal("missing wire: RemoteEndpoint.ToRDMA")
	}
	if endpoint.ToRDMA != rdmaEngine.RDMARequestInside {
		t.Fatal("missing wire: RemoteEndpoint.ToRDMA -> RDMA.RDMARequestInside")
	}
	if commandProcessor.DMAEngine == nil {
		t.Fatal("missing wire: CP.DMAEngine")
	}
	if len(commandProcessor.L1VCaches) == 0 || len(commandProcessor.L1SCaches) == 0 ||
		len(commandProcessor.L1ICaches) == 0 || len(commandProcessor.L2Caches) == 0 {
		t.Fatal("missing wire: CP data-cache control fan-out")
	}
	if len(commandProcessor.TLBs) == 0 {
		t.Fatal("missing wire: CP TLB endpoint set")
	}
	if len(commandProcessor.PreCacheTranslators.Ports) == 0 {
		t.Fatal("missing wire: CP gate endpoint set")
	}
	if gmmuComp.GetPortByName("Control") == nil {
		t.Fatal("missing wire: GMMU.Control")
	}
}