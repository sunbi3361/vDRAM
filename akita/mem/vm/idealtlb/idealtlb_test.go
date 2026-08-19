package idealtlb

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("IdealTLB", func() {
	var (
		mockCtrl    *gomock.Controller
		engine      *MockEngine
		comp        *Comp
		topPort     *MockPort
		bottomPort  *MockPort
		controlPort *MockPort
		pageTable   vm.PageTable
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		engine = NewMockEngine(mockCtrl)

		topPort = NewMockPort(mockCtrl)
		topPort.EXPECT().
			AsRemote().
			Return(sim.RemotePort("TopPort")).
			AnyTimes()
		topPort.EXPECT().
			Name().
			Return("TopPort").
			AnyTimes()

		bottomPort = NewMockPort(mockCtrl)
		bottomPort.EXPECT().
			AsRemote().
			Return(sim.RemotePort("BottomPort")).
			AnyTimes()
		bottomPort.EXPECT().
			Name().
			Return("BottomPort").
			AnyTimes()

		controlPort = NewMockPort(mockCtrl)
		controlPort.EXPECT().
			AsRemote().
			Return(sim.RemotePort("ControlPort")).
			AnyTimes()
		controlPort.EXPECT().
			Name().
			Return("ControlPort").
			AnyTimes()

		pageTable = vm.NewPageTable(12)
		pageTable.Insert(vm.Page{
			PID:      vm.PID(1),
			PAddr:    0x1000,
			VAddr:    0x2000,
			PageSize: 4096,
			Valid:    true,
			DeviceID: 1,
		})

		comp = MakeBuilder().
			WithEngine(engine).
			WithPageTable(pageTable).
			WithNumReqPerCycle(4).
			Build("IdealTLB")
		comp.topPort = topPort
		comp.bottomPort = bottomPort
		comp.controlPort = controlPort
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("should return the page on a hit", func() {
		req := vm.TranslationReqBuilder{}.
			WithSrc(topPort.AsRemote()).
			WithDst(topPort.AsRemote()).
			WithPID(vm.PID(1)).
			WithVAddr(0x2000).
			Build()

		controlPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()

		topPort.EXPECT().PeekIncoming().Return(req).Times(1)
		topPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
		topPort.EXPECT().CanSend().Return(true).Times(1)
		topPort.EXPECT().RetrieveIncoming().Return(req).Times(1)

		var sentRsp *vm.TranslationRsp
		topPort.EXPECT().Send(gomock.Any()).
			DoAndReturn(func(rsp *vm.TranslationRsp) *sim.SendError {
				sentRsp = rsp
				return nil
			}).
			Times(1)

		madeProgress := comp.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(sentRsp).NotTo(BeNil())
		Expect(sentRsp.Page.PAddr).To(Equal(uint64(0x1000)))
		Expect(sentRsp.GetRspTo()).To(Equal(req.ID))
	})

	It("should panic when the page is not found", func() {
		req := vm.TranslationReqBuilder{}.
			WithSrc(topPort.AsRemote()).
			WithDst(topPort.AsRemote()).
			WithPID(vm.PID(1)).
			WithVAddr(0x3000).
			Build()

		controlPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()

		topPort.EXPECT().PeekIncoming().Return(req).Times(1)
		topPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
		topPort.EXPECT().CanSend().Return(true).Times(1)
		topPort.EXPECT().RetrieveIncoming().Return(req).Times(1)

		Ω(func() { comp.Tick() }).Should(Panic())
	})

	It("should handle a flush request", func() {
		flushReq := tlb.FlushReqBuilder{}.
			WithSrc(controlPort.AsRemote()).
			WithDst(controlPort.AsRemote()).
			WithVAddrs([]uint64{0x2000}).
			WithPID(vm.PID(1)).
			Build()

		comp.state = tlbStateFlush

		controlPort.EXPECT().PeekIncoming().Return(flushReq).Times(1)
		controlPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
		controlPort.EXPECT().CanSend().Return(true).Times(1)
		controlPort.EXPECT().RetrieveIncoming().Return(flushReq).Times(1)

		var sentRsp *tlb.FlushRsp
		controlPort.EXPECT().Send(gomock.Any()).
			DoAndReturn(func(rsp *tlb.FlushRsp) *sim.SendError {
				sentRsp = rsp
				return nil
			}).
			Times(1)

		topPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()

		madeProgress := comp.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(sentRsp).NotTo(BeNil())
		Expect(comp.state).To(Equal(tlbStateEnable))
	})

	It("should handle a restart request", func() {
		restartReq := tlb.RestartReqBuilder{}.
			WithSrc(controlPort.AsRemote()).
			WithDst(controlPort.AsRemote()).
			Build()

		comp.state = tlbStateDrain

		controlPort.EXPECT().PeekIncoming().Return(restartReq).Times(1)
		controlPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
		controlPort.EXPECT().Send(gomock.Any()).
			DoAndReturn(func(rsp *tlb.RestartRsp) *sim.SendError {
				Expect(rsp).NotTo(BeNil())
				return nil
			}).
			Times(1)
		controlPort.EXPECT().RetrieveIncoming().Return(restartReq).Times(1)

		topPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
		bottomPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()

		madeProgress := comp.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(comp.state).To(Equal(tlbStateEnable))
	})

	It("should limit translation throughput to numReqPerCycle", func() {
		reqs := make([]*vm.TranslationReq, 8)
		for i := range reqs {
			reqs[i] = vm.TranslationReqBuilder{}.
				WithSrc(topPort.AsRemote()).
				WithDst(topPort.AsRemote()).
				WithPID(vm.PID(1)).
				WithVAddr(0x2000).
				Build()
		}

		controlPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()

		peekPos := 0
		topPort.EXPECT().PeekIncoming().
			DoAndReturn(func() sim.Msg {
				if peekPos < len(reqs) {
					req := reqs[peekPos]
					peekPos++
					return req
				}
				return nil
			}).
			AnyTimes()

		topPort.EXPECT().CanSend().Return(true).AnyTimes()

		retrievePos := 0
		topPort.EXPECT().RetrieveIncoming().
			DoAndReturn(func() sim.Msg {
				if retrievePos < len(reqs) {
					req := reqs[retrievePos]
					retrievePos++
					return req
				}
				return nil
			}).
			AnyTimes()

		sendCount := 0
		topPort.EXPECT().Send(gomock.Any()).
			DoAndReturn(func(rsp *vm.TranslationRsp) *sim.SendError {
				sendCount++
				Expect(rsp.Page.PAddr).To(Equal(uint64(0x1000)))
				return nil
			}).
			AnyTimes()

		comp.Tick()

		Expect(sendCount).To(Equal(4))
	})
})
