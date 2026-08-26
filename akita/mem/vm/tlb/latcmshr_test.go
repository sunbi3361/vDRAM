package tlb

// sbin_claude_latpc: LATC compressed-MSHR tests, including the paper's
// Figure-14c walkthrough (refs/latpc-plan.md 3).

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/sim"
	"go.uber.org/mock/gomock"
)

func latcReq(vAddr uint64, group string, stride int64, index int,
) *vm.TranslationReq {
	return vm.TranslationReqBuilder{}.
		WithPID(1).
		WithVAddr(vAddr).
		WithDeviceID(1).
		WithGroup(group, stride, index).
		Build()
}

var _ = Describe("LATC compressed MSHR", func() {
	var m *latcMSHR

	BeforeEach(func() {
		m = newLATCMSHR(8, 4096)
	})

	It("should compress the Figure-14c group into one entry", func() {
		// VPNs 0x8, 0xa, 0xc, 0xe with a stride of 2 pages: a demand and
		// three prefetches sharing one group entry with mask 0b1111.
		m.AddCompressed(latcReq(0x8000, "g", 0, 0))
		m.AddCompressed(latcReq(0xa000, "g", 2, 1))
		m.AddCompressed(latcReq(0xc000, "g", 2, 2))
		m.AddCompressed(latcReq(0xe000, "g", 2, 3))

		Expect(m.groups).To(HaveLen(1))
		Expect(m.groups[0].validMask).To(Equal(uint32(0b1111)))
		Expect(m.groups[0].baseVAddr).To(Equal(uint64(0x8000)))
		Expect(m.groups[0].stridePages).To(Equal(int64(2)))
		Expect(m.groupsAllocated).To(Equal(uint64(1)))
		Expect(m.coalescedSubentries).To(Equal(uint64(3)))
		Expect(m.OutstandingMisses()).To(Equal(4))

		Expect(m.GetEntry(1, 0xc000)).NotTo(BeNil())
		Expect(m.IsEntryPresent(1, 0xe000)).To(BeTrue())
		Expect(m.IsEntryPresent(1, 0x9000)).To(BeFalse())
	})

	It("should adopt the stride from the first prefetch member", func() {
		m.AddCompressed(latcReq(0x8000, "g", 0, 0))
		Expect(m.groups[0].stridePages).To(Equal(int64(0)))

		m.AddCompressed(latcReq(0xa000, "g", 2, 1))
		Expect(m.groups).To(HaveLen(1))
		Expect(m.groups[0].stridePages).To(Equal(int64(2)))

		// A member of a different stride can no longer join.
		m.AddCompressed(latcReq(0xb000, "g", 3, 1))
		Expect(m.groups).To(HaveLen(2))
	})

	It("should free the group entry once the mask reaches zero", func() {
		m.AddCompressed(latcReq(0x8000, "g", 0, 0))
		m.AddCompressed(latcReq(0xa000, "g", 2, 1))

		removed := m.Remove(1, 0x8000)
		Expect(removed.vAddr).To(Equal(uint64(0x8000)))
		Expect(m.groups).To(HaveLen(1))
		Expect(m.groups[0].validMask).To(Equal(uint32(0b10)))
		Expect(m.IsEmpty()).To(BeFalse())

		m.Remove(1, 0xa000)
		Expect(m.IsEmpty()).To(BeTrue())
	})

	It("should accept a joinable miss even at entry capacity", func() {
		small := newLATCMSHR(1, 4096)
		small.AddCompressed(latcReq(0x8000, "g", 0, 0))

		// A new group is a reservation failure; a group member is not.
		Expect(small.CanAccept(latcReq(0x100000, "h", 0, 0))).To(BeFalse())
		Expect(small.CanAccept(latcReq(0xa000, "g", 2, 1))).To(BeTrue())
	})

	It("should allocate a prefetch whose group does not exist yet", func() {
		// The demand hit in the TLB; the prefetch's group entry is created
		// at the base the triple names.
		m.AddCompressed(latcReq(0xa000, "g", 2, 1))

		Expect(m.groups[0].baseVAddr).To(Equal(uint64(0x8000)))
		Expect(m.groups[0].stridePages).To(Equal(int64(2)))
		Expect(m.groups[0].validMask).To(Equal(uint32(0b10)))

		// A later same-group member joins it.
		m.AddCompressed(latcReq(0xc000, "g", 2, 2))
		Expect(m.groups).To(HaveLen(1))
	})
})

var _ = Describe("TLB with LATC compressed MSHR", func() {
	var (
		mockCtrl      *gomock.Controller
		engine        *MockEngine
		latcTLB       *Comp
		latcMW        *tlbMiddleware
		set           *MockSet
		topPort       *MockPort
		bottomPort    *MockPort
		addressMapper *MockAddressToPortMapper
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		engine = NewMockEngine(mockCtrl)
		set = NewMockSet(mockCtrl)
		topPort = NewMockPort(mockCtrl)
		topPort.EXPECT().AsRemote().
			Return(sim.RemotePort("TopPort")).AnyTimes()
		topPort.EXPECT().Name().Return("TopPort").AnyTimes()
		bottomPort = NewMockPort(mockCtrl)
		bottomPort.EXPECT().AsRemote().
			Return(sim.RemotePort("BottomPort")).AnyTimes()
		bottomPort.EXPECT().Name().Return("BottomPort").AnyTimes()
		addressMapper = NewMockAddressToPortMapper(mockCtrl)
		addressMapper.EXPECT().Find(gomock.Any()).
			Return(sim.RemotePort("RemotePort")).AnyTimes()

		latcTLB = MakeBuilder().
			WithEngine(engine).
			WithTranslationProviderMapper(addressMapper).
			WithCompressedMSHR(true).
			Build("LATCTLB")
		latcTLB.topPort = topPort
		latcTLB.topChannels[0].port = topPort
		latcTLB.bottomPort = bottomPort
		latcTLB.sets = []internal.Set{set}
		latcTLB.state = tlbStateEnable

		latcMW = latcTLB.Middlewares()[1].(*tlbMiddleware)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("should report the compressed MSHR through the accessor", func() {
		Expect(latcTLB.CompressedMSHREnabled()).To(BeTrue())
	})

	It("should compress same-group misses and coalesce covered VPNs", func() {
		demand := latcReq(0x8000, "g", 0, 0)
		prefetch := latcReq(0xa000, "g", 2, 1)

		set.EXPECT().Lookup(vm.PID(1), uint64(0x8000)).
			Return(0, vm.Page{}, false)
		set.EXPECT().Lookup(vm.PID(1), uint64(0xa000)).
			Return(0, vm.Page{}, false)
		bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(2)

		Expect(latcMW.lookup(demand, latcTLB.topChannels[0])).To(BeTrue())
		Expect(latcMW.lookup(prefetch, latcTLB.topChannels[0])).To(BeTrue())

		groups, coalesced := latcTLB.LATCStats()
		Expect(groups).To(Equal(uint64(1)))
		Expect(coalesced).To(Equal(uint64(1)))

		// A repeat of a covered VPN is an MSHR hit: no bottom send.
		repeat := latcReq(0xa000, "h", 0, 0)
		Expect(latcMW.lookup(repeat, latcTLB.topChannels[0])).To(BeTrue())

		entry := latcTLB.mshr.GetEntry(1, 0xa000)
		Expect(entry.Requests).To(HaveLen(2))
	})

	It("should count reservation failures only when a group entry is needed",
		func() {
			smallTLB := MakeBuilder().
				WithEngine(engine).
				WithTranslationProviderMapper(addressMapper).
				WithCompressedMSHR(true).
				WithNumMSHREntry(1).
				Build("SmallLATCTLB")
			smallTLB.topPort = topPort
			smallTLB.topChannels[0].port = topPort
			smallTLB.bottomPort = bottomPort
			smallTLB.sets = []internal.Set{set}
			smallTLB.state = tlbStateEnable
			smallMW := smallTLB.Middlewares()[1].(*tlbMiddleware)

			set.EXPECT().Lookup(gomock.Any(), gomock.Any()).
				Return(0, vm.Page{}, false).AnyTimes()
			bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(2)

			demand := latcReq(0x8000, "g", 0, 0)
			Expect(smallMW.lookup(demand, smallTLB.topChannels[0])).
				To(BeTrue())

			otherDemand := latcReq(0x100000, "h", 0, 0)
			Expect(smallMW.lookup(otherDemand, smallTLB.topChannels[0])).
				To(BeFalse())
			Expect(smallTLB.ReservationFailureCount()).To(Equal(uint64(1)))

			member := latcReq(0xa000, "g", 2, 1)
			Expect(smallMW.lookup(member, smallTLB.topChannels[0])).
				To(BeTrue())
		})

	It("should fill one subentry per bottom response and free it", func() {
		set.EXPECT().Lookup(gomock.Any(), gomock.Any()).
			Return(0, vm.Page{}, false).AnyTimes()
		bottomPort.EXPECT().Send(gomock.Any()).Return(nil).Times(2)

		demand := latcReq(0x8000, "g", 0, 0)
		prefetch := latcReq(0xa000, "g", 2, 1)
		Expect(latcMW.lookup(demand, latcTLB.topChannels[0])).To(BeTrue())
		Expect(latcMW.lookup(prefetch, latcTLB.topChannels[0])).To(BeTrue())

		page := vm.Page{PID: 1, VAddr: 0xa000, PAddr: 0x200000, Valid: true}
		rsp := vm.TranslationRspBuilder{}.
			WithRspTo(latcTLB.mshr.GetEntry(1, 0xa000).reqToBottom.ID).
			WithPage(page).
			Build()

		bottomPort.EXPECT().PeekIncoming().Return(rsp)
		bottomPort.EXPECT().RetrieveIncoming()
		set.EXPECT().Evict().Return(1, true)
		set.EXPECT().Update(1, page)
		set.EXPECT().Visit(1)

		Expect(latcMW.parseBottom()).To(BeTrue())

		Expect(latcTLB.mshr.IsEntryPresent(vm.PID(1), uint64(0xa000))).
			To(BeFalse())
		Expect(latcTLB.mshr.IsEntryPresent(vm.PID(1), uint64(0x8000))).
			To(BeTrue())
		Expect(latcTLB.respondingMSHREntry.page).To(Equal(page))
		Expect(latcTLB.respondingMSHREntry.Requests).To(HaveLen(1))
	})
})
