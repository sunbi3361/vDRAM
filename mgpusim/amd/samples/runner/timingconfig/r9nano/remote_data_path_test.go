package r9nano

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/simulation" // sbin_claude_vc
)

type localRouteExpectation struct { // sbin_codex
	address uint64
	pid     vm.PID
}

type demandRouteExpectation struct { // sbin_codex
	translator  *addresstranslator.Comp
	request     mem.AccessReq
	remote      bool
	result      localRouteExpectation
	destination sim.RemotePort
}

var _ = Describe("R9 Nano CPU-remote data path", func() { // sbin_codex
	It("baseline classifies vector and scalar demands before data caches",
		func() {
			// Given
			testSimulation := buildRemoteDataPathGPU(
				NewBaselineDataPathTopology(), NewBaselineMemoryTopology())
			vectorAT := testSimulation.GetComponentByName(
				"GPU.SA[0].L1VAddrTrans[0]").(*addresstranslator.Comp)
			scalarAT := testSimulation.GetComponentByName(
				"GPU.SA[0].L1SAddrTrans").(*addresstranslator.Comp)
			rdmaRequestInside := testSimulation.GetComponentByName(
				"GPU.RDMA").GetPortByName("RDMARequestInside").AsRemote()
			local := localRouteExpectation{address: 0x11234}

			// When / Then
			expectDemandRoute(demandRouteExpectation{ // sbin_codex
				translator: vectorAT,
				request: mem.ReadReqBuilder{}.
					WithSrc("Vector").WithDst(vectorAT.GetPortByName("Top").AsRemote()).
					WithAddress(0x1234).WithByteSize(4).WithPID(7).Build(),
				result: local, destination: "GPU.SA[0].L1VCache[0].TopPort",
			})
			expectDemandRoute(demandRouteExpectation{ // sbin_codex
				translator: scalarAT,
				request: mem.WriteReqBuilder{}.
					WithSrc("Scalar").WithDst(scalarAT.GetPortByName("Top").AsRemote()).
					WithAddress(0x5678).WithData([]byte{1, 2, 3, 4}).WithPID(7).Build(),
				result:      localRouteExpectation{address: local.address + 0x4444, pid: local.pid},
				destination: "GPU.SA[0].L1SCache.TopPort",
			})
			expectDemandRoute(demandRouteExpectation{ // sbin_codex
				translator: vectorAT, remote: true,
				request: mem.ReadReqBuilder{}.
					WithSrc("VectorRemote").WithDst(vectorAT.GetPortByName("Top").AsRemote()).
					WithAddress(0x9234).WithByteSize(4).WithPID(7).Build(),
				result: localRouteExpectation{address: 0x21234}, destination: rdmaRequestInside,
			})
			expectDemandRoute(demandRouteExpectation{ // sbin_codex
				translator: scalarAT, remote: true,
				request: mem.WriteReqBuilder{}.
					WithSrc("ScalarRemote").WithDst(scalarAT.GetPortByName("Top").AsRemote()).
					WithAddress(0xd678).WithData([]byte{4, 3, 2, 1}).WithPID(7).Build(),
				result: localRouteExpectation{address: 0x25678}, destination: rdmaRequestInside,
			})
		})

	// sbin_claude_vc: virtual caching has no L1 data translators, so the
	// per-slice L2 translators are the first component that learns whether a
	// page is GPU-local or host-resident. They own the remote egress now.
	It("virtual-caching classifies demands at the L2 boundary", func() {
		// Given
		testSimulation := buildRemoteDataPathGPU(
			NewVirtualDataPathTopology(), NewVirtualMemoryTopology())
		componentNames := make([]string, 0, len(testSimulation.Components()))
		for _, component := range testSimulation.Components() {
			componentNames = append(componentNames, component.Name())
		}
		Expect(componentNames).NotTo(ContainElement("GPU.SA[0].L1VAddrTrans[0]"))
		Expect(componentNames).NotTo(ContainElement("GPU.SA[0].L1SAddrTrans"))

		sliceZero := testSimulation.GetComponentByName(
			"GPU.L2AddrTrans[0]").(*addresstranslator.Comp)
		sliceOne := testSimulation.GetComponentByName(
			"GPU.L2AddrTrans[1]").(*addresstranslator.Comp)
		rdmaRequestInside := testSimulation.GetComponentByName(
			"GPU.RDMA").GetPortByName("RDMARequestInside").AsRemote()

		// When / Then: a local page leaves as a physical DRAM access.
		expectDemandRoute(demandRouteExpectation{
			translator: sliceZero,
			request: mem.ReadReqBuilder{}.
				WithSrc("L2Fill").
				WithDst(sliceZero.GetPortByName("Top").AsRemote()).
				WithAddress(0x1234).WithByteSize(64).WithPID(7).Build(),
			result:      localRouteExpectation{address: 0x11234},
			destination: "GPU.DRAM[0].TopPort",
		})
		expectDemandRoute(demandRouteExpectation{
			translator: sliceOne,
			request: mem.WriteReqBuilder{}.
				WithSrc("L2Writeback").
				WithDst(sliceOne.GetPortByName("Top").AsRemote()).
				WithAddress(0x5678).WithData([]byte{1, 2, 3, 4}).
				WithPID(7).Build(),
			result:      localRouteExpectation{address: 0x15678},
			destination: "GPU.DRAM[1].TopPort",
		})

		// And a host-resident page leaves through the remote egress.
		expectDemandRoute(demandRouteExpectation{
			translator: sliceZero, remote: true,
			request: mem.ReadReqBuilder{}.
				WithSrc("L2FillRemote").
				WithDst(sliceZero.GetPortByName("Top").AsRemote()).
				WithAddress(0x9234).WithByteSize(64).WithPID(7).Build(),
			result:      localRouteExpectation{address: 0x21234},
			destination: rdmaRequestInside,
		})
		expectDemandRoute(demandRouteExpectation{
			translator: sliceOne, remote: true,
			request: mem.WriteReqBuilder{}.
				WithSrc("L2WritebackRemote").
				WithDst(sliceOne.GetPortByName("Top").AsRemote()).
				WithAddress(0xd678).WithData([]byte{4, 3, 2, 1}).
				WithPID(7).Build(),
			result:      localRouteExpectation{address: 0x25678},
			destination: rdmaRequestInside,
		})
	})
})

// buildRemoteDataPathGPU builds a two-slice GPU with the given topologies.
// sbin_claude_vc: extracted so the baseline and virtual specs, which now probe
// different components, can share the platform.
func buildRemoteDataPathGPU(
	dataPath DataPathTopology,
	memory MemoryTopology,
) *simulation.Simulation {
	testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("remote-data")
	MakeBuilder().
		WithSimulation(testSimulation).
		WithNumShaderArray(1).
		WithNumCUPerShaderArray(1).
		WithNumMemoryBank(2).
		WithL2CacheSize(32 * mem.KB).
		WithDramSize(2 * mem.GB).
		WithGlobalStorage(mem.NewStorage(4 * mem.GB)).
		WithMMU(cpuMMU).
		WithDataPathTopology(dataPath).
		WithMemoryTopology(memory).
		WithGPUID(1).
		WithPageTable(gpuPageTable).
		WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
			BankSize: 2 * mem.GB,
		}).
		Build("GPU")

	return testSimulation
}

func expectDemandRoute(want demandRouteExpectation) { // sbin_codex
	top := want.translator.GetPortByName("Top")
	translation := want.translator.GetPortByName("Translation")
	Expect(top.Deliver(want.request)).To(BeNil())
	Expect(want.translator.Tick()).To(BeTrue())
	translationRequest := translation.PeekOutgoing().(*vm.TranslationReq)
	translation.RetrieveOutgoing()
	page := vm.Page{
		PID: want.request.GetPID(), VAddr: want.request.GetAddress() &^ 0xfff,
		PAddr: want.result.address &^ 0xfff, PageSize: 4096, Valid: true,
		RemoteAccessible: want.remote,
	}
	Expect(translation.Deliver(translationRequest.GenerateRsp(page))).To(BeNil())
	Expect(want.translator.Tick()).To(BeTrue())
	portName := "Bottom"
	if want.remote {
		portName = "RemoteBottom"
	}
	forwarded := want.translator.GetPortByName(portName).PeekOutgoing().(mem.AccessReq)
	Expect(forwarded.Meta().Dst).To(Equal(want.destination), fmt.Sprintf("%s destination", portName))
	Expect(forwarded.GetAddress()).To(Equal(want.result.address))
	Expect(forwarded.GetPID()).To(Equal(want.result.pid))
	if want.remote {
		switch forwarded := forwarded.(type) {
		case *mem.ReadReq:
			Expect(forwarded.RemoteDemandInfo).NotTo(BeNil())
		case *mem.WriteReq:
			Expect(forwarded.RemoteDemandInfo).NotTo(BeNil())
		}
	}
	want.translator.GetPortByName(portName).RetrieveOutgoing()
}
