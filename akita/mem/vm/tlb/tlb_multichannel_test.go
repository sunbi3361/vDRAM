package tlb

// sbin_claude_vc: regression coverage for the top-channel split. A TLB whose
// top port is shared by two client classes deadlocks when one class cannot
// take its answers: the in-order port queue holds up the other class's
// answers too, and those answers are what would have unblocked the first
// class. These specs pin the property that makes the cycle impossible - a
// blocked channel blocks nothing but itself.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

var _ = Describe("TLB with two top channels", func() {
	var (
		mockCtrl      *gomock.Controller
		engine        *MockEngine
		tlb           *Comp
		tlbMW         *tlbMiddleware
		set           *MockSet
		demandPort    *MockPort
		fillPort      *MockPort
		bottomPort    *MockPort
		controlPort   *MockPort
		addressMapper *MockAddressToPortMapper
	)

	newMockPort := func(name string) *MockPort {
		port := NewMockPort(mockCtrl)
		port.EXPECT().AsRemote().Return(sim.RemotePort(name)).AnyTimes()
		port.EXPECT().Name().Return(name).AnyTimes()

		return port
	}

	reqTo := func(port *MockPort, vAddr uint64) *vm.TranslationReq {
		return vm.TranslationReqBuilder{}.
			WithSrc(sim.RemotePort("Requester")).
			WithDst(port.AsRemote()).
			WithPID(1).
			WithVAddr(vAddr).
			WithDeviceID(1).
			Build()
	}

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		engine = NewMockEngine(mockCtrl)
		set = NewMockSet(mockCtrl)
		demandPort = newMockPort("DemandPort")
		fillPort = newMockPort("FillPort")
		bottomPort = newMockPort("BottomPort")
		controlPort = newMockPort("ControlPort")
		addressMapper = NewMockAddressToPortMapper(mockCtrl)

		tlb = MakeBuilder().
			WithEngine(engine).
			WithTranslationProviderMapper(addressMapper).
			WithNumTopChannels(2).
			Build("TLB")
		tlb.topChannels[0].port = demandPort
		tlb.topChannels[1].port = fillPort
		tlb.topPort = demandPort
		tlb.bottomPort = bottomPort
		tlb.controlPort = controlPort
		tlb.sets = []internal.Set{set}
		tlb.state = tlbStateEnable

		tlbMW = tlb.Middlewares()[1].(*tlbMiddleware)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("exposes one port per channel", func() {
		Expect(tlb.GetPortByName("Top").Name()).To(Equal("TLB.TopPort"))
		Expect(tlb.GetPortByName("Top[1]").Name()).To(Equal("TLB.TopPort[1]"))
	})

	It("answers a free channel while the other channel cannot send", func() {
		page := vm.Page{PID: 1, VAddr: 0x1000, PAddr: 0x2000, Valid: true}
		set.EXPECT().Lookup(vm.PID(1), gomock.Any()).
			Return(1, page, true).AnyTimes()
		set.EXPECT().Visit(1).Times(1)

		demandPort.EXPECT().Send(gomock.Any()).
			Return(sim.NewSendError()).AnyTimes()
		fillPort.EXPECT().Send(gomock.Any()).
			Do(func(rsp sim.Msg) {
				Expect(rsp.Meta().Src).To(Equal(sim.RemotePort("FillPort")))
			}).
			Return(nil).Times(1)

		tlb.topChannels[0].buffer.Push(
			&pipelineTLBReq{req: reqTo(demandPort, 0x1000)})
		tlb.topChannels[1].buffer.Push(
			&pipelineTLBReq{req: reqTo(fillPort, 0x1000)})

		madeProgress := tlbMW.extractFromPipeline()

		Expect(madeProgress).To(BeTrue())
		Expect(tlb.topChannels[1].buffer.Peek()).To(BeNil())
		Expect(tlb.topChannels[0].buffer.Peek()).NotTo(BeNil())
	})

	It("keeps committing page walks while a channel cannot take its answer",
		func() {
			blockedReq := reqTo(demandPort, 0x1000)
			blockedEntry := tlb.mshr.Add(1, 0x1000)
			blockedEntry.Requests = append(blockedEntry.Requests, blockedReq)
			blockedEntry.reqToBottom = blockedReq

			freeReq := reqTo(fillPort, 0x2000)
			freeEntry := tlb.mshr.Add(1, 0x2000)
			freeEntry.Requests = append(freeEntry.Requests, freeReq)
			freeEntry.reqToBottom = freeReq

			set.EXPECT().Evict().Return(1, true).AnyTimes()
			set.EXPECT().Update(1, gomock.Any()).AnyTimes()
			set.EXPECT().Visit(1).AnyTimes()
			demandPort.EXPECT().Send(gomock.Any()).
				Return(sim.NewSendError()).AnyTimes()
			demandPort.EXPECT().RetrieveIncoming().Return(nil).AnyTimes()
			fillPort.EXPECT().RetrieveIncoming().Return(nil).AnyTimes()

			walkAnswer := func(req *vm.TranslationReq, page vm.Page) sim.Msg {
				return vm.TranslationRspBuilder{}.
					WithSrc(sim.RemotePort("Walker")).
					WithDst(sim.RemotePort("BottomPort")).
					WithRspTo(req.ID).
					WithPage(page).
					Build()
			}

			freePage := vm.Page{PID: 1, VAddr: 0x2000, PAddr: 0x9000}
			// The blocked class's walk answer arrives first. Pre-edit it
			// parked in the single responding-entry register and stopped
			// every later fill, the free class's included.
			bottom := []sim.Msg{
				walkAnswer(blockedReq,
					vm.Page{PID: 1, VAddr: 0x1000, PAddr: 0x8000}),
				walkAnswer(freeReq, freePage),
			}
			bottomPort.EXPECT().PeekIncoming().
				DoAndReturn(func() sim.Msg {
					if len(bottom) == 0 {
						return nil
					}

					return bottom[0]
				}).AnyTimes()
			bottomPort.EXPECT().RetrieveIncoming().
				DoAndReturn(func() sim.Msg {
					if len(bottom) == 0 {
						return nil
					}

					msg := bottom[0]
					bottom = bottom[1:]

					return msg
				}).AnyTimes()

			sent := make([]*vm.TranslationRsp, 0, 1)
			fillPort.EXPECT().Send(gomock.Any()).
				DoAndReturn(func(msg sim.Msg) *sim.SendError {
					sent = append(sent, msg.(*vm.TranslationRsp))

					return nil
				}).AnyTimes()

			// One fill per cycle, so: commit the blocked answer, commit the
			// free answer, then hand the free answer out.
			for range 3 {
				Expect(tlbMW.Tick()).To(BeTrue())
			}

			Expect(sent).To(HaveLen(1))
			Expect(sent[0].RespondTo).To(Equal(freeReq.ID))
			Expect(sent[0].Page).To(Equal(freePage))
			Expect(sent[0].Meta().Src).To(Equal(sim.RemotePort("FillPort")))
			Expect(tlb.topChannels[1].pending).To(BeEmpty())
			Expect(tlb.topChannels[0].pending).To(HaveLen(1))
		})
})
