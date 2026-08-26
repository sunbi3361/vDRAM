package tlb

// In-TLB MSHR tests (SoftWalker, MICRO'25 4.5). Unlike tlb_test.go these use
// the real internal.Set implementation, because the mechanism under test is
// exactly the interplay between MSHR overflow and the replacement rotation.
// sbin_claude_softwalker

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

var _ = Describe("In-TLB MSHR", func() {
	var (
		mockCtrl      *gomock.Controller
		engine        *MockEngine
		tlb           *Comp
		tlbMW         *tlbMiddleware
		topPort       *MockPort
		bottomPort    *MockPort
		controlPort   *MockPort
		addressMapper *MockAddressToPortMapper
	)

	buildTLB := func(numSets, numWays, numMSHR, inTLBMax int) {
		tlb = MakeBuilder().
			WithEngine(engine).
			WithNumSets(numSets).
			WithNumWays(numWays).
			WithNumMSHREntry(numMSHR).
			WithInTLBMSHR(inTLBMax).
			WithTranslationProviderMapper(addressMapper).
			Build("TLB")
		tlb.topPort = topPort
		tlb.topChannels[0].port = topPort
		tlb.bottomPort = bottomPort
		tlb.controlPort = controlPort
		tlb.state = tlbStateEnable

		tlbMW = tlb.Middlewares()[1].(*tlbMiddleware)
	}

	makeReq := func(vAddr uint64) *vm.TranslationReq {
		return vm.TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(vAddr).
			WithDeviceID(1).
			Build()
	}

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		engine = NewMockEngine(mockCtrl)
		topPort = NewMockPort(mockCtrl)
		topPort.EXPECT().AsRemote().
			Return(sim.RemotePort("TopPort")).AnyTimes()
		topPort.EXPECT().Name().Return("TopPort").AnyTimes()
		bottomPort = NewMockPort(mockCtrl)
		bottomPort.EXPECT().AsRemote().
			Return(sim.RemotePort("BottomPort")).AnyTimes()
		bottomPort.EXPECT().Name().Return("BottomPort").AnyTimes()
		controlPort = NewMockPort(mockCtrl)
		controlPort.EXPECT().AsRemote().
			Return(sim.RemotePort("ControlPort")).AnyTimes()
		controlPort.EXPECT().Name().Return("ControlPort").AnyTimes()
		addressMapper = NewMockAddressToPortMapper(mockCtrl)
		addressMapper.EXPECT().Find(gomock.Any()).
			Return(sim.RemotePort("RemotePort")).AnyTimes()
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("should allocate an In-TLB slot when the dedicated MSHR is full",
		func() {
			buildTLB(1, 2, 1, 2)
			bottomPort.EXPECT().Send(gomock.Any()).Return(nil)

			tlb.mshr.Add(1, 0x10000) // exhaust the dedicated MSHR

			madeProgress := tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])

			Expect(madeProgress).To(BeTrue())
			Expect(tlb.mshr.IsEntryPresent(vm.PID(1), uint64(0x2000))).
				To(BeTrue())
			Expect(tlb.mshr.InTLBCount()).To(Equal(1))
			Expect(tlb.InTLBMSHRStats().AllocCount).To(Equal(uint64(1)))

			entry := tlb.mshr.GetEntry(1, 0x2000)
			Expect(entry.inTLB).To(BeTrue())

			// The reserved way must have left the replacement rotation.
			wayID, ok := tlb.sets[0].PeekEvict()
			Expect(ok).To(BeTrue())
			Expect(wayID).NotTo(Equal(entry.wayID))
		})

	It("should merge onto a pending In-TLB entry without a second fetch",
		func() {
			buildTLB(1, 2, 1, 2)
			bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(1)

			tlb.mshr.Add(1, 0x10000)

			Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
				To(BeTrue())
			Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
				To(BeTrue())

			entry := tlb.mshr.GetEntry(1, 0x2000)
			Expect(entry.Requests).To(HaveLen(2))
			Expect(tlb.mshr.InTLBCount()).To(Equal(1))
		})

	It("should refuse the miss when every way of the set is reserved",
		func() {
			buildTLB(2, 2, 1, 4)
			bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(2)

			tlb.mshr.Add(1, 0x10000)

			// 0x2000 and 0x4000 both index set 0 and reserve its two ways.
			Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
				To(BeTrue())
			Expect(tlbMW.lookup(makeReq(0x4000), tlb.topChannels[0])).
				To(BeTrue())

			madeProgress := tlbMW.lookup(makeReq(0x6000), tlb.topChannels[0])

			Expect(madeProgress).To(BeFalse())
			Expect(tlb.InTLBMSHRStats().RefuseSetFull).To(Equal(uint64(1)))
		})

	It("should refuse the miss when the In-TLB capacity is reached", func() {
		buildTLB(2, 2, 1, 1)
		bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(1)

		tlb.mshr.Add(1, 0x10000)

		Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
			To(BeTrue())

		// Set 1 still has free ways, but the global cap is exhausted.
		madeProgress := tlbMW.lookup(makeReq(0x3000), tlb.topChannels[0])

		Expect(madeProgress).To(BeFalse())
		Expect(tlb.InTLBMSHRStats().RefuseCapFull).To(Equal(uint64(1)))
	})

	It("should keep the pre-existing refusal when disabled", func() {
		buildTLB(1, 2, 1, 0)

		tlb.mshr.Add(1, 0x10000)

		madeProgress := tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])

		Expect(madeProgress).To(BeFalse())
		Expect(tlb.InTLBMSHRStats().AllocCount).To(Equal(uint64(0)))
		Expect(tlb.InTLBMSHRStats().RefuseCapFull).To(Equal(uint64(0)))
		Expect(tlb.InTLBMSHRStats().RefuseSetFull).To(Equal(uint64(0)))
	})

	It("should install the fill into the reserved way", func() {
		buildTLB(1, 2, 1, 2)
		bottomPort.EXPECT().Send(gomock.Any()).Return(nil)

		tlb.mshr.Add(1, 0x10000)

		Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
			To(BeTrue())
		entry := tlb.mshr.GetEntry(1, 0x2000)
		reservedWay := entry.wayID

		page := vm.Page{PID: 1, VAddr: 0x2000, PAddr: 0x9000, Valid: true}
		rsp := vm.TranslationRspBuilder{}.
			WithRspTo(entry.reqToBottom.ID).
			WithPage(page).
			Build()
		bottomPort.EXPECT().PeekIncoming().Return(rsp)
		bottomPort.EXPECT().RetrieveIncoming()

		madeProgress := tlbMW.parseBottom()

		Expect(madeProgress).To(BeTrue())
		Expect(tlb.respondingMSHREntry).NotTo(BeNil())
		Expect(tlb.mshr.IsEntryPresent(vm.PID(1), uint64(0x2000))).
			To(BeFalse())
		Expect(tlb.mshr.InTLBCount()).To(Equal(0))

		wayID, storedPage, found := tlb.sets[0].Lookup(1, 0x2000)
		Expect(found).To(BeTrue())
		Expect(wayID).To(Equal(reservedWay))
		Expect(storedPage.PAddr).To(Equal(uint64(0x9000)))
	})

	It("should answer a dedicated fill without installing when reservations "+
		"exhausted the set", func() {
		buildTLB(1, 2, 1, 2)
		bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(3)

		// The dedicated entry misses first, then two In-TLB entries
		// reserve both ways of the only set.
		Expect(tlbMW.lookup(makeReq(0x5000), tlb.topChannels[0])).
			To(BeTrue())
		Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
			To(BeTrue())
		Expect(tlbMW.lookup(makeReq(0x3000), tlb.topChannels[0])).
			To(BeTrue())

		dedicated := tlb.mshr.GetEntry(1, 0x5000)
		page := vm.Page{PID: 1, VAddr: 0x5000, PAddr: 0xA000, Valid: true}
		rsp := vm.TranslationRspBuilder{}.
			WithRspTo(dedicated.reqToBottom.ID).
			WithPage(page).
			Build()
		bottomPort.EXPECT().PeekIncoming().Return(rsp)
		bottomPort.EXPECT().RetrieveIncoming()

		madeProgress := tlbMW.parseBottom()

		Expect(madeProgress).To(BeTrue())
		Expect(tlb.respondingMSHREntry).NotTo(BeNil())

		_, _, found := tlb.sets[0].Lookup(1, 0x5000)
		Expect(found).To(BeFalse())
	})

	It("should release reserved ways on flush", func() {
		buildTLB(1, 2, 1, 2)
		bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(2)

		tlb.mshr.Add(1, 0x10000)
		Expect(tlbMW.lookup(makeReq(0x2000), tlb.topChannels[0])).
			To(BeTrue())
		Expect(tlbMW.lookup(makeReq(0x3000), tlb.topChannels[0])).
			To(BeTrue())

		flush := FlushReqBuilder{}.
			WithSrc(sim.RemotePort("Requester")).
			WithVAddrs([]uint64{0x2000}).
			WithPID(1).
			Build()
		tlb.inflightFlushReq = flush
		controlPort.EXPECT().Send(gomock.Any()).Return(nil)
		controlPort.EXPECT().RetrieveIncoming().AnyTimes()

		madeProgress := tlbMW.processTLBFlush()

		Expect(madeProgress).To(BeTrue())
		Expect(tlb.mshr.InTLBCount()).To(Equal(0))

		// Both ways must be back in the replacement rotation.
		_, ok := tlb.sets[0].Evict()
		Expect(ok).To(BeTrue())
		_, ok = tlb.sets[0].Evict()
		Expect(ok).To(BeTrue())
	})
})
