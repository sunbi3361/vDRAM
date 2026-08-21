package r9nano

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/mem/vm/gmmu"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
)

type typedComponentNames struct {
	addressTranslators []string
	tlbs               []string
	gmmus              []string
	connections        []string
}

func componentNamesByType(components []sim.Component) typedComponentNames {
	var names typedComponentNames
	for _, component := range components {
		switch component := component.(type) {
		case *addresstranslator.Comp:
			names.addressTranslators = append(names.addressTranslators, component.Name())
		case *tlb.Comp:
			names.tlbs = append(names.tlbs, component.Name())
		case *gmmu.Comp:
			names.gmmus = append(names.gmmus, component.Name())
		case *directconnection.Comp:
			names.connections = append(names.connections, component.Name())
		}
	}
	return names
}

func componentNamesForPorts(ports []sim.Port) []string {
	names := make([]string, 0, len(ports))
	for _, port := range ports {
		names = append(names, port.Component().Name())
	}
	return names
}

func expectPortConnectedTo(port sim.Port, connectionName string) {
	probeConnection := directconnection.MakeBuilder().
		WithEngine(nil).
		WithFreq(1 * sim.GHz).
		Build("ProbeConnection")
	Expect(func() {
		probeConnection.PlugIn(port)
	}).To(PanicWith(ContainSubstring(
		"connection already set to " + connectionName)))
}

type translationRouteExpectation struct {
	accessReq    mem.AccessReq
	tlb          sim.RemotePort
	dram         sim.RemotePort
	physicalPage uint64
}

func expectTranslatorRoutesAccess(
	translator *addresstranslator.Comp,
	want translationRouteExpectation,
) {
	topPort := translator.GetPortByName("Top")
	translationPort := translator.GetPortByName("Translation")
	bottomPort := translator.GetPortByName("Bottom")
	Expect(topPort.Deliver(want.accessReq)).To(BeNil())
	Expect(translator.Tick()).To(BeTrue())
	translationReq := translationPort.PeekOutgoing().(*vm.TranslationReq)
	Expect(translationReq.Dst).To(Equal(want.tlb))
	translationPort.RetrieveOutgoing()

	translationRsp := translationReq.GenerateRsp(vm.Page{
		PID:      want.accessReq.GetPID(),
		VAddr:    want.accessReq.GetAddress() &^ 0xfff,
		PAddr:    want.physicalPage,
		PageSize: 4096,
		Valid:    true,
	}).(*vm.TranslationRsp)
	Expect(translationPort.Deliver(translationRsp)).To(BeNil())
	Expect(translator.Tick()).To(BeTrue())
	translatedReq := bottomPort.PeekOutgoing().(mem.AccessReq)
	Expect(translatedReq.Meta().Dst).To(Equal(want.dram))
	Expect(translatedReq.GetAddress()).To(Equal(
		want.physicalPage + want.accessReq.GetAddress()%4096))
	Expect(translatedReq.GetPID()).To(Equal(vm.PID(0)))
	Expect(translatedReq).To(BeAssignableToTypeOf(want.accessReq))
	bottomPort.RetrieveOutgoing()
}

func newR9NanoTestSimulation(name string) (*simulation.Simulation, vm.PageTable, *mmu.Comp) {
	outputPrefix := filepath.Join(GinkgoT().TempDir(), name)
	testSimulation := simulation.MakeBuilder().
		WithoutMonitoring().
		WithOutputFileName(outputPrefix).
		Build()
	DeferCleanup(func() {
		testSimulation.Terminate()
		artifacts, err := filepath.Glob(outputPrefix + "_*.sqlite3")
		Expect(err).NotTo(HaveOccurred())
		Expect(artifacts).To(HaveLen(1))
		Expect(os.Remove(artifacts[0])).To(Succeed())
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
