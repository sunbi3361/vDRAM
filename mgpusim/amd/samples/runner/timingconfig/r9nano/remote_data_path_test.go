package r9nano

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/sim"
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
	DescribeTable("classifies vector and scalar demands before data caches",
		func(dataPath DataPathTopology, memory MemoryTopology, local localRouteExpectation) {
			// Given
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
			vectorAT := testSimulation.GetComponentByName(
				"GPU.SA[0].L1VAddrTrans[0]").(*addresstranslator.Comp)
			scalarAT := testSimulation.GetComponentByName(
				"GPU.SA[0].L1SAddrTrans").(*addresstranslator.Comp)
			rdmaRequestInside := testSimulation.GetComponentByName(
				"GPU.RDMA").GetPortByName("RDMARequestInside").AsRemote()

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
		},
		Entry("baseline uses physical local requests",
			NewBaselineDataPathTopology(), NewBaselineMemoryTopology(),
			localRouteExpectation{address: 0x11234}),
		Entry("virtual-caching preserves virtual local requests",
			NewVirtualDataPathTopology(), NewVirtualMemoryTopology(),
			localRouteExpectation{address: 0x1234, pid: 7}),
	)
})

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
