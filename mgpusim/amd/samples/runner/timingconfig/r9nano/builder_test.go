package r9nano

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

var _ = Describe("R9 Nano builder", func() {
	It("preserves the reduced baseline translation and physical-memory topology", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("baseline")

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
			WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
				BankSize:   2 * mem.GB,
				LowModules: []sim.RemotePort{"CPU"},
			}).
			Build("GPU")

		// Then
		componentNames := componentNamesByType(testSimulation.Components())
		Expect(componentNames.addressTranslators).To(ConsistOf(
			"GPU.SA[0].L1VAddrTrans[0]",
			"GPU.SA[0].L1SAddrTrans",
			"GPU.SA[0].L1IAddrTrans",
		))
		Expect(componentNames.tlbs).To(ConsistOf(
			"GPU.SA[0].L1VTLB[0]",
			"GPU.SA[0].L1STLB",
			"GPU.SA[0].L1ITLB",
			"GPU.L2TLB",
		))
		Expect(componentNames.gmmus).To(ConsistOf("GPU.GMMU"))
		Expect(componentNames.connections).To(ContainElements(
			"GPU.L1ToL2",       // sbin_codex
			"GPU.L1TLBToL2TLB", // sbin_codex
			"GPU.L2ToDRAM",
			"GPU.L2TLBToGMMU",
		))
		// Pre-edit code (commented per AGENTS.md convention):
		// Expect(componentNames.connections).NotTo(ContainElement("GPU.L1TLBToL2TLB"))
		// Expect(componentNames.connections).NotTo(ContainElement("GPU.L1ToL2"))
		probeConnection := directconnection.MakeBuilder().
			WithEngine(testSimulation.GetEngine()).
			WithFreq(1 * sim.GHz).
			Build("ProbeConnection")
		for _, port := range []sim.Port{
			testSimulation.GetComponentByName(
				"GPU.SA[0].L1ITLB").GetPortByName("Bottom"),
			testSimulation.GetComponentByName(
				"GPU.L2TLB").GetPortByName("Top"),
		} {
			Expect(func() {
				probeConnection.PlugIn(port)
			}).To(PanicWith(ContainSubstring(
				"connection already set to GPU.L1TLBToL2TLB")))
		}

		commandProcessor := testSimulation.GetComponentByName(
			"GPU.CommandProcessor").(*cp.CommandProcessor)
		Expect(commandProcessor.PreCacheTranslators.Ports).To(HaveLen(3)) // sbin_codex
		Expect(commandProcessor.PostCacheTranslators.Ports).To(BeEmpty()) // sbin_codex
		Expect(commandProcessor.TLBs).To(HaveLen(4))
		Expect(commandProcessor.L2Caches).To(HaveLen(2))

		l2Bottom := testSimulation.GetComponentByName(
			"GPU.L2Cache[0]").GetPortByName("Bottom")
		dramTop := testSimulation.GetComponentByName(
			"GPU.DRAM[0]").GetPortByName("Top")
		req := mem.ReadReqBuilder{}.
			WithSrc(l2Bottom.AsRemote()).
			WithDst(dramTop.AsRemote()).
			WithAddress(0).
			WithByteSize(64).
			Build()
		Expect(l2Bottom.Send(req)).To(BeNil())
		connection := testSimulation.GetComponentByName(
			"GPU.L2ToDRAM").(*directconnection.Comp)
		Expect(connection.Tick()).To(BeTrue())
		Expect(dramTop.PeekIncoming()).To(BeIdenticalTo(req))
	})

	It("places one translator per L2 slice at the physical-memory boundary", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("virtual")

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
			WithMemoryTopology(NewVirtualMemoryTopology()). // sbin_codex
			WithGPUID(1).
			WithPageTable(gpuPageTable).
			WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
				BankSize:   2 * mem.GB,
				LowModules: []sim.RemotePort{"CPU"},
			}).
			Build("GPU")

		// Then
		componentNames := componentNamesByType(testSimulation.Components())
		// Pre-edit code (commented per AGENTS.md convention):
		// Expect(componentNames.addressTranslators).To(ConsistOf(
		// 	"GPU.SA[0].L1IAddrTrans",
		// 	"GPU.L2AddrTrans[0]",
		// 	"GPU.L2AddrTrans[1]",
		// ))
		Expect(componentNames.addressTranslators).To(ConsistOf(
			"GPU.SA[0].L1VAddrTrans[0]", // sbin_codex
			"GPU.SA[0].L1SAddrTrans",    // sbin_codex
			"GPU.SA[0].L1IAddrTrans",
			"GPU.L2AddrTrans[0]",
			"GPU.L2AddrTrans[1]",
		))
		// Pre-edit code (commented per AGENTS.md convention):
		// Expect(componentNames.tlbs).To(ConsistOf(
		// 	"GPU.SA[0].L1ITLB",
		// 	"GPU.L2TLB",
		// ))
		Expect(componentNames.tlbs).To(ConsistOf(
			"GPU.SA[0].L1VTLB[0]", // sbin_codex
			"GPU.SA[0].L1STLB",    // sbin_codex
			"GPU.SA[0].L1ITLB",
			"GPU.L2TLB",
		))
		Expect(componentNames.gmmus).To(ConsistOf("GPU.GMMU"))
		Expect(componentNames.connections).To(ContainElements(
			"GPU.L1ToL2", // sbin_codex: virtual topology exposes its L1/L2 ingress.
			"GPU.L2ToL2AddrTrans[0]",
			"GPU.L2ToL2AddrTrans[1]",
			"GPU.L2AddrTransToDRAM",
			"GPU.TranslationToL2TLB",
			"GPU.L2TLBToGMMU",
		))
		Expect(componentNames.connections).NotTo(ContainElement("GPU.L2ToDRAM"))

		commandProcessor := testSimulation.GetComponentByName(
			"GPU.CommandProcessor").(*cp.CommandProcessor)
		// Pre-edit code (commented per AGENTS.md convention):
		// Expect(componentNamesForPorts(commandProcessor.PreCacheTranslators.Ports)).To(
		// 	ConsistOf("GPU.SA[0].L1IAddrTrans"))
		Expect(componentNamesForPorts(commandProcessor.PreCacheTranslators.Ports)).To(
			ConsistOf(
				"GPU.SA[0].L1VAddrTrans[0]",
				"GPU.SA[0].L1SAddrTrans",
				"GPU.SA[0].L1IAddrTrans",
			)) // sbin_codex
		Expect(componentNamesForPorts(commandProcessor.PostCacheTranslators.Ports)).To(
			ConsistOf(
				"GPU.L2AddrTrans[0]",
				"GPU.L2AddrTrans[1]",
			)) // sbin_codex: every per-slice L2 AT is tracked separately.
		Expect(componentNamesForPorts(commandProcessor.TLBs)).To(ConsistOf(
			"GPU.SA[0].L1VTLB[0]", // sbin_codex
			"GPU.SA[0].L1STLB",    // sbin_codex
			"GPU.SA[0].L1ITLB",
			"GPU.L2TLB",
		))
		Expect(commandProcessor.L1VCaches).To(HaveLen(1))
		Expect(commandProcessor.L1SCaches).To(HaveLen(1))
		Expect(commandProcessor.L1ICaches).To(HaveLen(1))
		Expect(commandProcessor.L2Caches).To(HaveLen(2))

		l1iTranslator := testSimulation.GetComponentByName(
			"GPU.SA[0].L1IAddrTrans").(*addresstranslator.Comp)
		expectTranslatorRoutesAccess(l1iTranslator, translationRouteExpectation{
			accessReq: mem.ReadReqBuilder{}.
				WithSrc("Requester.L1I").
				WithDst(l1iTranslator.GetPortByName("Top").AsRemote()).
				WithAddress(0x1000).
				WithByteSize(64).
				WithPID(7).
				Build(),
			tlb: testSimulation.GetComponentByName(
				"GPU.SA[0].L1ITLB").GetPortByName("Top").AsRemote(),
			// dram: testSimulation.GetComponentByName(
			// 	"GPU.RDMA").GetPortByName("RequestInside").AsRemote(),
			dram: testSimulation.GetComponentByName(
				"GPU.RDMA").GetPortByName("RDMARequestInside").AsRemote(), // sbin_codex
			physicalPage: 3 * mem.GB,
		}) // sbin_codex: L1I retains physical-range/RDMA mapping in virtual mode.

		sharedTranslationPorts := []sim.Port{
			testSimulation.GetComponentByName(
				"GPU.SA[0].L1VTLB[0]").GetPortByName("Bottom"), // sbin_codex
			testSimulation.GetComponentByName(
				"GPU.SA[0].L1STLB").GetPortByName("Bottom"), // sbin_codex
			testSimulation.GetComponentByName(
				"GPU.SA[0].L1ITLB").GetPortByName("Bottom"),
			testSimulation.GetComponentByName(
				"GPU.L2TLB").GetPortByName("Top"),
		}
		physicalMemoryPorts := []sim.Port{
			testSimulation.GetComponentByName("GPU.DMA").(*cp.DMAEngine).ToMem,
			testSimulation.GetComponentByName("GPU.PMC").GetPortByName("LocalMem"),
		}
		for i := range 2 {
			translator := testSimulation.GetComponentByName(
				fmt.Sprintf("GPU.L2AddrTrans[%d]", i))
			for _, port := range []sim.Port{
				testSimulation.GetComponentByName(
					fmt.Sprintf("GPU.L2Cache[%d]", i)).GetPortByName("Bottom"),
				translator.GetPortByName("Top"),
			} {
				expectPortConnectedTo(port,
					fmt.Sprintf("GPU.L2ToL2AddrTrans[%d]", i))
			}
			sharedTranslationPorts = append(sharedTranslationPorts,
				translator.GetPortByName("Translation"))
			physicalMemoryPorts = append(physicalMemoryPorts,
				translator.GetPortByName("Bottom"),
				testSimulation.GetComponentByName(
					fmt.Sprintf("GPU.DRAM[%d]", i)).GetPortByName("Top"))
		}
		for _, port := range sharedTranslationPorts {
			expectPortConnectedTo(port, "GPU.TranslationToL2TLB")
		}
		for _, port := range physicalMemoryPorts {
			expectPortConnectedTo(port, "GPU.L2AddrTransToDRAM")
		}

		for i, accessReq := range []mem.AccessReq{
			mem.ReadReqBuilder{}.
				WithSrc("Requester.Read").
				WithDst("GPU.L2AddrTrans[0].TopPort").
				WithAddress(0x1234).
				WithByteSize(64).
				WithPID(7).
				Build(),
			mem.WriteReqBuilder{}.
				WithSrc("Requester.Write").
				WithDst("GPU.L2AddrTrans[1].TopPort").
				WithAddress(0x5678).
				WithData(make([]byte, 64)).
				WithPID(7).
				Build(),
		} {
			translator := testSimulation.GetComponentByName(
				fmt.Sprintf("GPU.L2AddrTrans[%d]", i)).(*addresstranslator.Comp)
			expectTranslatorRoutesAccess(translator, translationRouteExpectation{
				accessReq: accessReq,
				tlb: testSimulation.GetComponentByName(
					"GPU.L2TLB").GetPortByName("Top").AsRemote(),
				dram: testSimulation.GetComponentByName(
					fmt.Sprintf("GPU.DRAM[%d]", i)).GetPortByName("Top").AsRemote(),
				physicalPage: 0x8000,
			})
		}
	})
})
