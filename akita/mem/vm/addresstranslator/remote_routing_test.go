package addresstranslator

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

// sbin_codex: opt-in remote and virtual-local routing behavior.
var _ = Describe("Address Translator remote routing", func() {
	var (
		mockCtrl         *gomock.Controller
		topPort          *MockPort // sbin_codex
		bottomPort       *MockPort
		remoteBottomPort *MockPort
		translationPort  *MockPort
		localMapper      *MockAddressToPortMapper
		remoteMapper     *MockAddressToPortMapper
		translator       *Comp
		translatorMW     *middleware
		queueTranslation func(mem.AccessReq, vm.Page)
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		topPort = NewMockPort(mockCtrl) // sbin_codex
		topPort.EXPECT().AsRemote().Return(sim.RemotePort("TopPort")).AnyTimes()
		topPort.EXPECT().Name().Return("TopPort").AnyTimes()
		bottomPort = NewMockPort(mockCtrl)
		bottomPort.EXPECT().AsRemote().Return(sim.RemotePort("BottomPort")).AnyTimes()
		bottomPort.EXPECT().Name().Return("BottomPort").AnyTimes()
		remoteBottomPort = NewMockPort(mockCtrl)
		remoteBottomPort.EXPECT().AsRemote().Return(sim.RemotePort("RemoteBottomPort")).AnyTimes()
		remoteBottomPort.EXPECT().Name().Return("RemoteBottomPort").AnyTimes()
		translationPort = NewMockPort(mockCtrl)
		translationPort.EXPECT().AsRemote().Return(sim.RemotePort("TranslationPort")).AnyTimes()
		localMapper = NewMockAddressToPortMapper(mockCtrl)
		remoteMapper = NewMockAddressToPortMapper(mockCtrl)

		translator = MakeBuilder().
			WithLog2PageSize(12).
			WithDeviceID(9).
			WithMemoryProviderMapper(localMapper).
			WithRemoteMemoryProviderMapper(remoteMapper).
			WithTranslationProviderMapper(localMapper).
			WithVirtualAddressForLocalMemory().
			Build("RemoteAddressTranslator")
		translator.bottomPort = bottomPort
		translator.topPort = topPort // sbin_codex
		translator.remoteBottomPort = remoteBottomPort
		translator.translationPort = translationPort
		translatorMW = translator.Middlewares()[0].(*middleware)

		queueTranslation = func(req mem.AccessReq, page vm.Page) {
			transReq := vm.TranslationReqBuilder{}.
				WithPID(req.GetPID()).
				WithVAddr(req.GetAddress() &^ uint64(0xfff)).
				WithDeviceID(9).
				Build()
			translator.transactions = []*transaction{{
				incomingReqs:   []mem.AccessReq{req},
				translationReq: transReq,
			}}
			rsp := vm.TranslationRspBuilder{}.
				WithRspTo(transReq.ID).
				WithPage(page).
				Build()
			translationPort.EXPECT().PeekIncoming().Return(rsp)
			translationPort.EXPECT().RetrieveIncoming()
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("routes a remote read physically with original demand metadata", func() {
		// Given // sbin_codex
		req := mem.ReadReqBuilder{}.
			WithPID(7).
			WithAddress(0x12340).
			WithByteSize(64).
			Build()
		page := vm.Page{PID: 7, VAddr: 0x12000, PAddr: 0x80000, RemoteAccessible: true}
		queueTranslation(req, page)
		remoteMapper.EXPECT().Find(uint64(0x80340)).Return(sim.RemotePort("RemoteMemory"))
		remoteBottomPort.EXPECT().Send(gomock.Any()).DoAndReturn(func(sent *mem.ReadReq) *sim.SendError {
			Expect(sent.Address).To(Equal(uint64(0x80340)))
			Expect(sent.PID).To(Equal(vm.PID(0)))
			Expect(sent.Src).To(Equal(remoteBottomPort.AsRemote()))
			Expect(sent.RemoteDemandInfo).To(Equal(&mem.RemoteDemandInfo{
				PID: 7, VAddr: 0x12340, DeviceID: 9,
			}))
			return nil
		})

		// When / Then // sbin_codex
		Expect(translatorMW.parseTranslation()).To(BeTrue())
	})

	It("routes a remote write physically with original demand metadata", func() {
		// Given // sbin_codex
		req := mem.WriteReqBuilder{}.
			WithPID(8).
			WithAddress(0x23450).
			WithData([]byte{1, 2, 3, 4}).
			Build()
		page := vm.Page{PID: 8, VAddr: 0x23000, PAddr: 0x90000, RemoteAccessible: true}
		queueTranslation(req, page)
		remoteMapper.EXPECT().Find(uint64(0x90450)).Return(sim.RemotePort("RemoteMemory"))
		remoteBottomPort.EXPECT().Send(gomock.Any()).DoAndReturn(func(sent *mem.WriteReq) *sim.SendError {
			Expect(sent.Address).To(Equal(uint64(0x90450)))
			Expect(sent.PID).To(Equal(vm.PID(0)))
			Expect(sent.Src).To(Equal(remoteBottomPort.AsRemote()))
			Expect(sent.RemoteDemandInfo).To(Equal(&mem.RemoteDemandInfo{
				PID: 8, VAddr: 0x23450, DeviceID: 9,
			}))
			return nil
		})

		// When / Then // sbin_codex
		Expect(translatorMW.parseTranslation()).To(BeTrue())
	})

	It("keeps a local read virtual without remote metadata", func() {
		// Given // sbin_codex
		req := mem.ReadReqBuilder{}.
			WithPID(7).
			WithAddress(0x12340).
			WithByteSize(64).
			Build()
		queueTranslation(req, vm.Page{PID: 7, VAddr: 0x12000, PAddr: 0x80000})
		localMapper.EXPECT().Find(uint64(0x12340)).Return(sim.RemotePort("LocalMemory"))
		bottomPort.EXPECT().Send(gomock.Any()).DoAndReturn(func(sent *mem.ReadReq) *sim.SendError {
			Expect(sent.Address).To(Equal(uint64(0x12340)))
			Expect(sent.PID).To(Equal(vm.PID(7)))
			Expect(sent.RemoteDemandInfo).To(BeNil())
			return nil
		})

		// When / Then // sbin_codex
		Expect(translatorMW.parseTranslation()).To(BeTrue())
	})

	It("keeps a local write virtual without remote metadata", func() {
		// Given // sbin_codex
		req := mem.WriteReqBuilder{}.
			WithPID(8).
			WithAddress(0x23450).
			WithData([]byte{1, 2, 3, 4}).
			Build()
		queueTranslation(req, vm.Page{PID: 8, VAddr: 0x23000, PAddr: 0x90000})
		localMapper.EXPECT().Find(uint64(0x23450)).Return(sim.RemotePort("LocalMemory"))
		bottomPort.EXPECT().Send(gomock.Any()).DoAndReturn(func(sent *mem.WriteReq) *sim.SendError {
			Expect(sent.Address).To(Equal(uint64(0x23450)))
			Expect(sent.PID).To(Equal(vm.PID(8)))
			Expect(sent.RemoteDemandInfo).To(BeNil())
			return nil
		})

		// When / Then // sbin_codex
		Expect(translatorMW.parseTranslation()).To(BeTrue())
	})

	It("returns a remote read response through the top port", func() {
		// Given // sbin_codex
		fromTop := mem.ReadReqBuilder{}.Build()
		toRemote := mem.ReadReqBuilder{}.Build()
		translator.inflightReqToBottom = []reqToBottom{{
			reqFromTop: fromTop, reqToBottom: toRemote,
		}}
		rsp := mem.DataReadyRspBuilder{}.
			WithRspTo(toRemote.ID).
			WithData([]byte{4, 3, 2, 1}).
			Build()
		bottomPort.EXPECT().PeekIncoming().Return(nil)
		remoteBottomPort.EXPECT().PeekIncoming().Return(rsp)
		remoteBottomPort.EXPECT().RetrieveIncoming()
		topPort.EXPECT().Send(gomock.Any()).DoAndReturn(func(sent *mem.DataReadyRsp) *sim.SendError {
			Expect(sent.RespondTo).To(Equal(fromTop.ID))
			Expect(sent.Data).To(Equal(rsp.Data))
			return nil
		})

		// When / Then // sbin_codex
		Expect(translatorMW.respond()).To(BeTrue())
	})

	It("returns a remote write response through the top port", func() {
		// Given // sbin_codex
		fromTop := mem.WriteReqBuilder{}.Build()
		toRemote := mem.WriteReqBuilder{}.Build()
		translator.inflightReqToBottom = []reqToBottom{{
			reqFromTop: fromTop, reqToBottom: toRemote,
		}}
		rsp := mem.WriteDoneRspBuilder{}.WithRspTo(toRemote.ID).Build()
		bottomPort.EXPECT().PeekIncoming().Return(nil)
		remoteBottomPort.EXPECT().PeekIncoming().Return(rsp)
		remoteBottomPort.EXPECT().RetrieveIncoming()
		topPort.EXPECT().Send(gomock.Any()).DoAndReturn(func(sent *mem.WriteDoneRsp) *sim.SendError {
			Expect(sent.RespondTo).To(Equal(fromTop.ID))
			return nil
		})

		// When / Then // sbin_codex
		Expect(translatorMW.respond()).To(BeTrue())
	})
})
