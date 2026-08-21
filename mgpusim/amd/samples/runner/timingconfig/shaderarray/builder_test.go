package shaderarray

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/cache/writearound"
	"github.com/sarchlab/akita/v4/mem/cache/writethrough"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cu"
	"github.com/sarchlab/mgpusim/v4/amd/timing/rob"
)

var _ = Describe("Shader array builder", func() {
	It("builds data and instruction translation paths by default", func() {
		// Given
		outputPrefix := filepath.Join(GinkgoT().TempDir(), "default-shader-array")
		testSimulation := simulation.MakeBuilder().
			WithoutMonitoring().
			WithOutputFileName(outputPrefix).
			Build()
		DeferCleanup(func() {
			testSimulation.Terminate()
			artifacts, err := filepath.Glob(outputPrefix + "_*.sqlite3")
			Expect(err).NotTo(HaveOccurred())
			Expect(artifacts).To(HaveLen(1))
			for _, artifact := range artifacts {
				Expect(os.Remove(artifact)).To(Succeed())
			}
		})

		// When
		domain := MakeBuilder().
			WithSimulation(testSimulation).
			WithNumCUs(1).
			WithL1AddressMapper(mem.NewInterleavedAddressPortMapper(64)).
			WithL1TLBAddressMapper(&mem.SinglePortMapper{}).
			Build("ShaderArray")

		// Then
		componentNames := make([]string, 0, len(testSimulation.Components()))
		for _, component := range testSimulation.Components() {
			componentNames = append(componentNames, component.Name())
		}
		Expect(componentNames).To(ContainElements(
			"ShaderArray.L1VCache[0]",
			"ShaderArray.L1VAddrTrans[0]",
			"ShaderArray.L1VTLB[0]",
			"ShaderArray.L1SCache",
			"ShaderArray.L1SAddrTrans",
			"ShaderArray.L1STLB",
			"ShaderArray.L1IAddrTrans",
			"ShaderArray.L1ITLB",
		))

		for _, portName := range []string{
			"L1VAddrTransCtrl[0]",
			"L1VTLBCtrl[0]",
			"L1VCacheCtrl[0]",
			"L1VCacheBottom[0]",
			"L1VTLBBottom[0]",
			"L1SAddrTransCtrl",
			"L1STLBCtrl",
			"L1SCacheCtrl",
			"L1SCacheBottom",
			"L1STLBBottom",
			"L1IAddrTransCtrl",
			"L1ITLBCtrl",
			"L1ICacheCtrl",
			"L1ICacheBottom",
			"L1ITLBBottom",
		} {
			Expect(func() {
				_ = domain.GetPortByName(portName)
			}).NotTo(Panic(), portName)
		}
	})

	It("builds virtual data caches without data translation paths", func() {
		// Given
		outputPrefix := filepath.Join(GinkgoT().TempDir(), "virtual-shader-array")
		testSimulation := simulation.MakeBuilder().
			WithoutMonitoring().
			WithOutputFileName(outputPrefix).
			Build()
		DeferCleanup(func() {
			testSimulation.Terminate()
			artifacts, err := filepath.Glob(outputPrefix + "_*.sqlite3")
			Expect(err).NotTo(HaveOccurred())
			Expect(artifacts).To(HaveLen(1))
			for _, artifact := range artifacts {
				Expect(os.Remove(artifact)).To(Succeed())
			}
		})

		// When
		domain := MakeBuilder().
			WithSimulation(testSimulation).
			WithNumCUs(1).
			WithL1AddressMapper(mem.NewInterleavedAddressPortMapper(64)).
			WithL1TLBAddressMapper(&mem.SinglePortMapper{}).
			WithDataPathTopology(NewVirtualDataPathTopology()). // sbin_codex: prove the injected non-default strategy is consumed.
			Build("VirtualShaderArray")

		// Then
		componentNames := make([]string, 0, len(testSimulation.Components()))
		for _, component := range testSimulation.Components() {
			componentNames = append(componentNames, component.Name())
		}
		Expect(componentNames).To(ContainElements(
			"VirtualShaderArray.L1VCache[0]",
			"VirtualShaderArray.L1SCache",
			"VirtualShaderArray.L1IAddrTrans",
			"VirtualShaderArray.L1ITLB",
		))
		for _, componentName := range []string{
			"VirtualShaderArray.L1VAddrTrans[0]",
			"VirtualShaderArray.L1VTLB[0]",
			"VirtualShaderArray.L1SAddrTrans",
			"VirtualShaderArray.L1STLB",
		} {
			Expect(componentNames).NotTo(ContainElement(componentName))
		}

		for _, portName := range []string{
			"L1VCacheCtrl[0]",
			"L1VCacheBottom[0]",
			"L1SCacheCtrl",
			"L1SCacheBottom",
			"L1IAddrTransCtrl",
			"L1ITLBCtrl",
			"L1ICacheBottom",
			"L1ITLBBottom",
		} {
			Expect(func() {
				_ = domain.GetPortByName(portName)
			}).NotTo(Panic(), portName)
		}

		for _, portName := range []string{
			"L1VAddrTransCtrl[0]",
			"L1VTLBCtrl[0]",
			"L1VTLBBottom[0]",
			"L1SAddrTransCtrl",
			"L1STLBCtrl",
			"L1STLBBottom",
		} {
			Expect(func() {
				_ = domain.GetPortByName(portName)
			}).To(Panic(), portName)
		}

		computeUnit := testSimulation.GetComponentByName(
			"VirtualShaderArray.CU[0]").(*cu.ComputeUnit)
		vectorROB := testSimulation.GetComponentByName(
			"VirtualShaderArray.L1VROB[0]").(*rob.ReorderBuffer)
		vectorCache := testSimulation.GetComponentByName(
			"VirtualShaderArray.L1VCache[0]").(*writearound.Comp)
		Expect(computeUnit.VectorMemModules.Find(0)).To(Equal(
			vectorROB.GetPortByName("Top").AsRemote()))
		Expect(vectorROB.BottomUnit).To(Equal(
			vectorCache.GetPortByName("Top").AsRemote()))

		scalarROB := testSimulation.GetComponentByName(
			"VirtualShaderArray.L1SROB").(*rob.ReorderBuffer)
		scalarCache := testSimulation.GetComponentByName(
			"VirtualShaderArray.L1SCache").(*writethrough.Comp)
		Expect(computeUnit.ScalarMem).To(Equal(scalarROB.GetPortByName("Top")))
		Expect(scalarROB.BottomUnit).To(Equal(
			scalarCache.GetPortByName("Top").AsRemote()))

		instructionROB := testSimulation.GetComponentByName(
			"VirtualShaderArray.L1IROB").(*rob.ReorderBuffer)
		instructionCache := testSimulation.GetComponentByName(
			"VirtualShaderArray.L1ICache").(*writethrough.Comp)
		Expect(instructionROB.BottomUnit).To(Equal(
			instructionCache.GetPortByName("Top").AsRemote()))
	})
})
