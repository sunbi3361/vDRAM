// sbin_claude_fbt
package r9nano

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/mgpusim/v4/amd/timing/virtualcaching/fbt"
)

var _ = Describe("R9 Nano FBT translation topology", func() {
	It("puts the FBT between the shared L2 TLB and the GMMU", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("fbt")

		// When
		MakeBuilder().
			WithSimulation(testSimulation).
			WithNumShaderArray(1).
			WithNumCUPerShaderArray(1).
			WithNumMemoryBank(2).
			WithL2CacheSize(32 * mem.KB).
			WithDramSize(2 * mem.GB).
			WithGlobalStorage(mem.NewStorage(4 * mem.GB)).
			WithMMU(cpuMMU).
			WithDataPathTopology(NewVirtualDataPathTopology()).
			WithMemoryTopology(NewVirtualMemoryTopology()).
			WithTranslationTopology(NewFBTTranslationTopology(FBTSettings{})).
			WithGPUID(1).
			WithPageTable(gpuPageTable).
			Build("GPU")

		// Then
		table := testSimulation.GetComponentByName("GPU.FBT").(*fbt.Comp)
		Expect(table).NotTo(BeNil())

		// The L2 TLB sends its misses to the FBT, not to the GMMU.
		expectPortConnectedTo(
			testSimulation.GetComponentByName("GPU.L2TLB").
				GetPortByName("Bottom"), "GPU.L2TLBToFBT")
		expectPortConnectedTo(table.GetPortByName("Top"), "GPU.L2TLBToFBT")

		// And the FBT is what reaches the page walker.
		expectPortConnectedTo(table.GetPortByName("Bottom"), "GPU.FBTToGMMU")
		expectPortConnectedTo(
			testSimulation.GetComponentByName("GPU.GMMU").
				GetPortByName("Top"), "GPU.FBTToGMMU")

		l2TLB := testSimulation.GetComponentByName("GPU.L2TLB").(*tlb.Comp)
		Expect(l2TLB).NotTo(BeNil())

		componentNames := componentNamesByType(testSimulation.Components())
		Expect(componentNames.connections).NotTo(
			ContainElement("GPU.L2TLBToGMMU"))
	})

	It("leaves the baseline walker chain untouched", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("no-fbt")

		// When
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
			Build("GPU")

		// Then
		componentNames := componentNamesByType(testSimulation.Components())
		Expect(componentNames.connections).To(
			ContainElement("GPU.L2TLBToGMMU"))
		Expect(componentNames.connections).NotTo(
			ContainElement("GPU.L2TLBToFBT"))
	})
})
