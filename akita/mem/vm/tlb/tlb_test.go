package tlb

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb/internal"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"go.uber.org/mock/gomock"
)

var _ = Describe("TLB", func() {
	var (
		mockCtrl      *gomock.Controller
		engine        *MockEngine
		tlb           *Comp
		tlbMW         *tlbMiddleware
		tlbCtrlMW     *ctrlMiddleware
		set           *MockSet
		topPort       *MockPort
		bottomPort    *MockPort
		controlPort   *MockPort
		addressMapper *MockAddressToPortMapper
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		engine = NewMockEngine(mockCtrl)
		set = NewMockSet(mockCtrl)
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
		addressMapper = NewMockAddressToPortMapper(mockCtrl)

		tlb = MakeBuilder().
			WithEngine(engine).
			WithTranslationProviderMapper(addressMapper).
			Build("TLB")
		tlb.topPort = topPort
		tlb.topChannels[0].port = topPort // sbin_claude_vc: inject the mock into channel 0.
		tlb.bottomPort = bottomPort
		tlb.controlPort = controlPort
		tlb.sets = []internal.Set{set}
		tlb.state = tlbStateEnable

		tlbMW = tlb.Middlewares()[1].(*tlbMiddleware)
		tlbCtrlMW = tlb.Middlewares()[0].(*ctrlMiddleware)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("should do nothing if there is no req in TopPort", func() {
		topPort.EXPECT().RetrieveIncoming().Return(nil)

		madeProgress := tlbMW.insertIntoPipeline()

		Expect(madeProgress).To(BeFalse())
	})

	It("should insert req into pipeline when topPort has req", func() {
		req := vm.TranslationReqBuilder{}.
			WithPID(1).
			WithVAddr(uint64(0x100)).
			WithDeviceID(1).
			Build()

		topPort.EXPECT().RetrieveIncoming().Return(req).Times(1)
		topPort.EXPECT().RetrieveIncoming().Return(nil).AnyTimes()
		madeProgress := tlbMW.insertIntoPipeline()

		Expect(madeProgress).To(BeTrue())
	})

	Context("hit", func() {
		var (
			wayID int
			page  vm.Page
			req   *vm.TranslationReq
		)

		BeforeEach(func() {
			wayID = 1
			page = vm.Page{
				PID:   1,
				VAddr: 0x100,
				PAddr: 0x200,
				Valid: true,
			}
			set.EXPECT().Lookup(vm.PID(1), uint64(0x100)).
				Return(wayID, page, true)

			req = vm.TranslationReqBuilder{}.
				WithPID(1).
				WithVAddr(uint64(0x100)).
				WithDeviceID(1).
				Build()
		})

		It("should respond to top", func() {
			topPort.EXPECT().Send(gomock.Any()).Times(1)

			set.EXPECT().Visit(wayID).Times(1)

			madeProgress := tlbMW.lookup(req, tlb.topChannels[0])

			Expect(madeProgress).To(BeTrue())
		})
	})

	Context("miss", func() {
		var (
			wayID int
			page  vm.Page
			req   *vm.TranslationReq
		)

		BeforeEach(func() {
			wayID = 1
			page = vm.Page{
				PID:   1,
				VAddr: 0x100,
				PAddr: 0x200,
				Valid: false,
			}
			set.EXPECT().
				Lookup(vm.PID(1), uint64(0x100)).
				Return(wayID, page, true).
				AnyTimes()

			addressMapper.EXPECT().
				Find(uint64(0x100)).
				Return(sim.RemotePort("RemotePort")).
				AnyTimes()

			req = vm.TranslationReqBuilder{}.
				WithPID(1).
				WithVAddr(0x100).
				WithDeviceID(1).
				Build()
		})

		It("should fetch from bottom and add entry to MSHR", func() {
			bottomPort.EXPECT().Send(gomock.Any()).
				Do(func(req *vm.TranslationReq) {
					Expect(req.VAddr).To(Equal(uint64(0x100)))
					Expect(req.PID).To(Equal(vm.PID(1)))
					Expect(req.DeviceID).To(Equal(uint64(1)))
				}).
				Return(nil)

			madeProgress := tlbMW.lookup(req, tlb.topChannels[0])

			Expect(madeProgress).To(BeTrue())
			Expect(tlb.mshr.IsEntryPresent(vm.PID(1), uint64(0x100))).
				To(Equal(true))
		})

		// sbin_claude_latpc: the LATPC group triple must ride the bottom
		// request so the L2 TLB and the GMMU can batch same-group walks.
		It("should propagate the LATPC group triple to the bottom", func() {
			hintedReq := vm.TranslationReqBuilder{}.
				WithPID(1).
				WithVAddr(0x100).
				WithDeviceID(1).
				WithGroup("g1", 2, 3).
				Build()

			bottomPort.EXPECT().Send(gomock.Any()).
				Do(func(req *vm.TranslationReq) {
					Expect(req.GroupID).To(Equal("g1"))
					Expect(req.GroupStride).To(Equal(int64(2)))
					Expect(req.GroupIndex).To(Equal(3))
				}).
				Return(nil)

			madeProgress := tlbMW.lookup(hintedReq, tlb.topChannels[0])

			Expect(madeProgress).To(BeTrue())
		})
	})

	Context("parse bottom", func() {
		var (
			wayID       int
			req         *vm.TranslationReq
			fetchBottom *vm.TranslationReq
			page        vm.Page
			rsp         *vm.TranslationRsp
		)

		BeforeEach(func() {
			wayID = 1
			req = vm.TranslationReqBuilder{}.
				WithPID(1).
				WithVAddr(0x100).
				WithDeviceID(1).
				Build()
			fetchBottom = vm.TranslationReqBuilder{}.
				WithPID(1).
				WithVAddr(0x100).
				WithDeviceID(1).
				Build()
			page = vm.Page{
				PID:   1,
				VAddr: 0x100,
				PAddr: 0x200,
				Valid: true,
			}
			rsp = vm.TranslationRspBuilder{}.
				WithRspTo(fetchBottom.ID).
				WithPage(page).
				Build()
		})

		It("should do nothing if no return", func() {
			bottomPort.EXPECT().PeekIncoming().Return(nil)

			madeProgress := tlbMW.parseBottom()

			Expect(madeProgress).To(BeFalse())
		})

		It("should stall if the TLB is responding to an MSHR entry", func() {
			mshrEntry := tlb.mshr.Add(1, 0x100)
			mshrEntry.Requests = append(mshrEntry.Requests, req)
			tlb.respondingMSHREntry = mshrEntry

			madeProgress := tlbMW.parseBottom()

			Expect(madeProgress).To(BeFalse())
		})

		It("should parse respond from bottom", func() {
			bottomPort.EXPECT().PeekIncoming().Return(rsp)
			bottomPort.EXPECT().RetrieveIncoming()
			mshrEntry := tlb.mshr.Add(1, 0x100)
			mshrEntry.Requests = append(mshrEntry.Requests, req)
			mshrEntry.reqToBottom = &vm.TranslationReq{}

			set.EXPECT().Evict().Return(wayID, true)
			set.EXPECT().Update(wayID, page)
			set.EXPECT().Visit(wayID)

			madeProgress := tlbMW.parseBottom()

			Expect(madeProgress).To(BeTrue())
			Expect(tlb.respondingMSHREntry).NotTo(BeNil())
			Expect(tlb.mshr.IsEntryPresent(vm.PID(1), uint64(0x100))).
				To(Equal(false))
		})

		It("replies to waiters without storing a rejected page", func() { // sbin_codex
			// Given // sbin_codex
			remotePage := page
			remotePage.RemoteAccessible = true
			remoteRsp := vm.TranslationRspBuilder{}.
				WithRspTo(fetchBottom.ID).
				WithPage(remotePage).
				Build()
			selectiveTLB := MakeBuilder().
				WithEngine(engine).
				WithTranslationProviderMapper(addressMapper).
				WithPageAdmissionPredicate(func(page vm.Page) bool {
					return !page.RemoteAccessible
				}).
				Build("SelectiveTLB")
			selectiveTLB.topPort = topPort
			selectiveTLB.bottomPort = bottomPort
			selectiveTLB.sets = []internal.Set{set}
			selectiveMW := selectiveTLB.Middlewares()[1].(*tlbMiddleware)
			mshrEntry := selectiveTLB.mshr.Add(1, 0x100)
			mshrEntry.Requests = append(mshrEntry.Requests, req)
			mshrEntry.reqToBottom = fetchBottom
			bottomPort.EXPECT().PeekIncoming().Return(remoteRsp)
			bottomPort.EXPECT().RetrieveIncoming()
			topPort.EXPECT().Send(gomock.Any()).DoAndReturn(
				func(rsp *vm.TranslationRsp) *sim.SendError {
					Expect(rsp.Page).To(Equal(remotePage))
					return nil
				})

			// When // sbin_codex
			parsed := selectiveMW.parseBottom()
			responded := selectiveMW.respondMSHREntry()

			// Then // sbin_codex
			Expect(parsed).To(BeTrue())
			Expect(responded).To(BeTrue())
			Expect(selectiveTLB.respondingMSHREntry).To(BeNil())
			Expect(selectiveTLB.mshr.IsEntryPresent(1, 0x100)).To(BeFalse())
		})

		It("should respond", func() {
			mshrEntry := tlb.mshr.Add(1, 0x100)
			mshrEntry.Requests = append(mshrEntry.Requests, req)
			tlb.respondingMSHREntry = mshrEntry

			topPort.EXPECT().Send(gomock.Any()).Return(nil)

			madeProgress := tlbMW.respondMSHREntry()

			Expect(madeProgress).To(BeTrue())
			Expect(mshrEntry.Requests).To(HaveLen(0))
			Expect(tlb.respondingMSHREntry).To(BeNil())
		})
	})

	// sbin_claude_avatar: refs/avatar.md 5.9 steps 5-7 - an Early TLB Fill
	// must leave its validated translation behind in the shared TLB and hand
	// it to the CUs coalesced on the same VPN, not just drop the walk.
	Context("Avatar Early TLB Fill cancel", func() {
		var (
			cancelPort *MockPort
			eafReq     *vm.TranslationReq
			otherReq   *vm.TranslationReq
			page       vm.Page
			mshrEntry  *mshrEntry
		)

		BeforeEach(func() {
			cancelPort = NewMockPort(mockCtrl)
			cancelPort.EXPECT().
				AsRemote().
				Return(sim.RemotePort("CancelPort")).
				AnyTimes()
			cancelPort.EXPECT().Name().Return("CancelPort").AnyTimes()
			tlb.cancelPort = cancelPort
			tlb.walkCancelDst = sim.RemotePort("GMMUCancel")

			eafReq = vm.TranslationReqBuilder{}.
				WithPID(1).WithVAddr(0x100).WithDeviceID(1).Build()
			otherReq = vm.TranslationReqBuilder{}.
				WithPID(1).WithVAddr(0x100).WithDeviceID(2).Build()
			page = vm.Page{PID: 1, VAddr: 0x100, PAddr: 0x200, Valid: true}

			mshrEntry = tlb.mshr.Add(1, 0x100)
			mshrEntry.Requests = append(
				mshrEntry.Requests, eafReq, otherReq)
			mshrEntry.reqToBottom = &vm.TranslationReq{}
		})

		cancelWithPage := func(p vm.Page) *vm.TranslationCancelReq {
			return vm.TranslationCancelReqBuilder{}.
				WithCancelID(eafReq.ID).
				WithVAddr(0x100).
				WithPID(1).
				WithPage(p).
				Build()
		}

		It("installs the validated page and answers the other waiter", func() {
			cancel := cancelWithPage(page)
			cancelPort.EXPECT().PeekIncoming().Return(cancel).Times(1)
			cancelPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
			cancelPort.EXPECT().RetrieveIncoming()

			set.EXPECT().Evict().Return(1, true)
			set.EXPECT().Update(1, page)
			set.EXPECT().Visit(1)

			Expect(tlbMW.processCancels()).To(BeTrue())

			// The page is now in the TLB, the CU that did not trigger the
			// EAF is queued for an answer, and the walk is retired.
			Expect(tlb.respondingMSHREntry).To(Equal(mshrEntry))
			Expect(mshrEntry.Requests).To(Equal(
				[]*vm.TranslationReq{otherReq}))
			Expect(tlb.mshr.IsEntryPresent(1, 0x100)).To(BeFalse())
			Expect(tlb.pendingBottomCancels).To(HaveLen(1))
		})

		It("does not install anything on a plain cancel", func() {
			cancel := cancelWithPage(vm.Page{})
			cancelPort.EXPECT().PeekIncoming().Return(cancel).Times(1)
			cancelPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
			cancelPort.EXPECT().RetrieveIncoming()

			Expect(tlbMW.processCancels()).To(BeTrue())

			// The other waiter still needs its walk, so the entry stays.
			Expect(mshrEntry.Requests).To(Equal(
				[]*vm.TranslationReq{otherReq}))
			Expect(tlb.mshr.IsEntryPresent(1, 0x100)).To(BeTrue())
			Expect(tlb.pendingBottomCancels).To(BeEmpty())
		})

		It("retries while the response register is busy", func() {
			tlb.respondingMSHREntry = tlb.mshr.Add(2, 0x300)

			cancel := cancelWithPage(page)
			cancelPort.EXPECT().PeekIncoming().Return(cancel).AnyTimes()

			Expect(tlbMW.processCancels()).To(BeFalse())

			// Nothing was consumed or mutated: the cancel is still pending.
			Expect(mshrEntry.Requests).To(Equal(
				[]*vm.TranslationReq{eafReq, otherReq}))
			Expect(tlb.mshr.IsEntryPresent(1, 0x100)).To(BeTrue())
		})

		It("fills even when the EAF requester is the only waiter", func() {
			mshrEntry.Requests = []*vm.TranslationReq{eafReq}
			tlb.respondingMSHREntry = tlb.mshr.Add(2, 0x300)

			cancel := cancelWithPage(page)
			cancelPort.EXPECT().PeekIncoming().Return(cancel).Times(1)
			cancelPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
			cancelPort.EXPECT().RetrieveIncoming()

			set.EXPECT().Evict().Return(1, true)
			set.EXPECT().Update(1, page)
			set.EXPECT().Visit(1)

			// No answer is owed, so a busy response register cannot block it.
			Expect(tlbMW.processCancels()).To(BeTrue())
			Expect(tlb.mshr.IsEntryPresent(1, 0x100)).To(BeFalse())
		})
	})

	Context("flush related handling", func() {
		It("should do nothing if no req", func() {
			controlPort.EXPECT().PeekIncoming().Return(nil)
			madeProgress := tlbCtrlMW.performCtrlReq()
			Expect(madeProgress).To(BeFalse())
		})

		It("should handle flush request", func() {
			flushReq := FlushReqBuilder{}.
				WithSrc(sim.RemotePort("")).
				WithDst(controlPort.AsRemote()).
				WithVAddrs([]uint64{0x1000}).
				WithPID(1).
				Build()
			page := vm.Page{
				PID:   1,
				VAddr: 0x1000,
				Valid: true,
			}
			wayID := 1

			set.EXPECT().Lookup(vm.PID(1), uint64(0x1000)).
				Return(wayID, page, true)
			set.EXPECT().Update(wayID, vm.Page{
				PID:   1,
				VAddr: 0x1000,
				Valid: false,
			})
			controlPort.EXPECT().PeekIncoming().Return(flushReq)
			controlPort.EXPECT().RetrieveIncoming().Return(flushReq)
			controlPort.EXPECT().Send(gomock.Any())
			bottomPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
			topPort.EXPECT().RetrieveIncoming().Return(nil).AnyTimes()
			topPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()

			madeProgress := tlbCtrlMW.handleIncomingCommands()
			madeProgress = tlbMW.Tick() || madeProgress

			Expect(madeProgress).To(BeTrue())
		})

		It("should handle restart request", func() {
			restartReq := RestartReqBuilder{}.
				WithSrc(sim.RemotePort("")).
				WithDst(controlPort.AsRemote()).
				Build()
			controlPort.EXPECT().PeekIncoming().
				Return(restartReq)
			controlPort.EXPECT().RetrieveIncoming().
				Return(restartReq)
			controlPort.EXPECT().Send(gomock.Any())
			topPort.EXPECT().PeekIncoming().Return(nil)
			bottomPort.EXPECT().PeekIncoming().Return(nil)

			madeProgress := tlbCtrlMW.handleIncomingCommands()

			Expect(madeProgress).To(BeTrue())
		})
	})

	Context("other control signals", func() {
		It("should handle pause ctrl msg", func() {
			pauseMsg := mem.ControlMsgBuilder{}.
				WithSrc(sim.RemotePort("")).
				WithDst(controlPort.AsRemote()).
				WithCtrlInfo(false, false, false, true, false).
				Build()

			controlPort.EXPECT().PeekIncoming().
				Return(pauseMsg)
			controlPort.EXPECT().RetrieveIncoming().
				Return(pauseMsg)

			madeProgress := tlbCtrlMW.performCtrlReq()

			Expect(madeProgress).To(BeTrue())
			Expect(tlb.state).To(Equal(tlbStatePause))
		})

		It("should handle enable ctrl msg after pause", func() {
			pause := mem.ControlMsgBuilder{}.
				WithSrc(sim.RemotePort("")).
				WithDst(controlPort.AsRemote()).
				WithCtrlInfo(false, false, false, true, false).
				Build()

			controlPort.EXPECT().PeekIncoming().
				Return(pause)
			controlPort.EXPECT().RetrieveIncoming().
				Return(pause)

			madeProgress := tlbCtrlMW.performCtrlReq()

			Expect(madeProgress).To(BeTrue())
			Expect(tlb.state).To(Equal(tlbStatePause))

			enable := mem.ControlMsgBuilder{}.
				WithSrc(sim.RemotePort("")).
				WithDst(controlPort.AsRemote()).
				WithCtrlInfo(true, false, false, false, false).
				Build()

			controlPort.EXPECT().PeekIncoming().
				Return(enable)
			controlPort.EXPECT().RetrieveIncoming().
				Return(enable)

			madeProgress = tlbCtrlMW.performCtrlReq()
			Expect(madeProgress).To(BeTrue())
			Expect(tlb.state).To(Equal(tlbStateEnable))
		})

		It("should handle drain ctrl msg", func() {
			drainMsg := mem.ControlMsgBuilder{}.
				WithSrc(sim.RemotePort("")).
				WithDst(controlPort.AsRemote()).
				WithCtrlInfo(false, true, false, false, false).
				Build()

			controlPort.EXPECT().PeekIncoming().
				Return(drainMsg)
			controlPort.EXPECT().RetrieveIncoming().
				Return(drainMsg)

			madeProgress := tlbCtrlMW.performCtrlReq()

			Expect(madeProgress).To(BeTrue())
			Expect(tlb.state).To(Equal(tlbStateDrain))

			bottomPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
			topPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
			topPort.EXPECT().RetrieveIncoming().Return(nil).AnyTimes()
			madeProgress = tlbMW.handleDrain()
			Expect(madeProgress).To(BeFalse())
			Expect(tlb.state).To(Equal(tlbStatePause))
		})
	})
})

var _ = Describe("TLB Integration", func() {
	var (
		mockCtrl   *gomock.Controller
		engine     sim.Engine
		tlb        *Comp
		lowModule  *MockPort
		agent      *MockPort
		connection sim.Connection
		page       vm.Page
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		engine = sim.NewSerialEngine()
		lowModule = NewMockPort(mockCtrl)
		lowModule.EXPECT().
			AsRemote().
			Return(sim.RemotePort("LowModule")).
			AnyTimes()
		lowModuleCall := lowModule.EXPECT().
			PeekOutgoing().
			Return(nil).
			AnyTimes()

		agent = NewMockPort(mockCtrl)
		agent.EXPECT().PeekOutgoing().Return(nil).AnyTimes()
		agent.EXPECT().
			AsRemote().
			Return(sim.RemotePort("Agent")).
			AnyTimes()

		connection = directconnection.MakeBuilder().
			WithEngine(engine).
			WithFreq(1 * sim.GHz).
			Build("Conn")

		addressMapper := &mem.SinglePortMapper{
			Port: lowModule.AsRemote(),
		}
		tlb = MakeBuilder().
			WithEngine(engine).
			WithTranslationProviderMapper(addressMapper).
			WithLowModule(lowModule.AsRemote()).
			Build("TLB")

		agent.EXPECT().SetConnection(connection)
		lowModule.EXPECT().SetConnection(connection)
		connection.PlugIn(agent)
		connection.PlugIn(lowModule)
		connection.PlugIn(tlb.topPort)
		connection.PlugIn(tlb.bottomPort)
		connection.PlugIn(tlb.controlPort)

		page = vm.Page{
			PID:   1,
			VAddr: 0x1000,
			PAddr: 0x2000,
			Valid: true,
		}
		lowModule.EXPECT().Deliver(gomock.Any()).
			Do(func(req *vm.TranslationReq) {
				rsp := vm.TranslationRspBuilder{}.
					WithSrc(lowModule.AsRemote()).
					WithDst(req.Src).
					WithPage(page).
					WithRspTo(req.ID).
					Build()
				lowModuleCall.Times(0)
				lowModule.EXPECT().PeekOutgoing().Return(rsp)
				lowModule.EXPECT().RetrieveOutgoing().Return(rsp)
				lowModule.EXPECT().PeekOutgoing().Return(nil).AnyTimes()
			}).
			AnyTimes()
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("should do tlb miss", func() {
		req := vm.TranslationReqBuilder{}.
			WithSrc(agent.AsRemote()).
			WithDst(tlb.topPort.AsRemote()).
			WithPID(1).
			WithVAddr(0x1000).
			WithDeviceID(1).
			Build()
		tlb.topPort.Deliver(req)

		agent.EXPECT().Deliver(gomock.Any()).
			Do(func(rsp *vm.TranslationRsp) {
				fmt.Println("Deliver() called with Page:", rsp.Page)
				Expect(rsp.Page).To(Equal(page))
			})

		engine.Run()
	})

	It("should have faster hit than miss", func() {
		time1 := engine.CurrentTime()
		req := vm.TranslationReqBuilder{}.
			WithSrc(agent.AsRemote()).
			WithDst(tlb.topPort.AsRemote()).
			WithPID(1).
			WithVAddr(0x1000).
			WithDeviceID(1).
			Build()
		tlb.topPort.Deliver(req)

		agent.EXPECT().Deliver(gomock.Any()).
			Do(func(rsp *vm.TranslationRsp) {
				Expect(rsp.Page).To(Equal(page))
			})

		engine.Run()

		time2 := engine.CurrentTime()

		tlb.topPort.Deliver(req)

		agent.EXPECT().Deliver(gomock.Any()).
			Do(func(rsp *vm.TranslationRsp) {
				Expect(rsp.Page).To(Equal(page))
			})

		engine.Run()

		time3 := engine.CurrentTime()

		Expect(time3 - time2).To(BeNumerically("<", time2-time1))
	})
})
