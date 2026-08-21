package cp

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"go.uber.org/mock/gomock"
)

var _ = Describe("L2 address translator shootdown control", func() { // sbin_codex: lock the post-cache quiesce and restart ordering.
	var (
		mockCtrl            *gomock.Controller
		commandProcessor    *CommandProcessor
		middleware          *ctrlMiddleware
		toAddressTranslator *MockPort
		toCaches            *MockPort
		toCUs               *MockPort
		toTLBs              *MockPort
		l1Translator        *MockPort
		l2Translator        *MockPort
		tlbPort             *MockPort
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		toAddressTranslator = NewMockPort(mockCtrl)
		toCaches = NewMockPort(mockCtrl)
		toCUs = NewMockPort(mockCtrl)
		toTLBs = NewMockPort(mockCtrl)
		l1Translator = NewMockPort(mockCtrl)
		l2Translator = NewMockPort(mockCtrl)
		tlbPort = NewMockPort(mockCtrl)

		toAddressTranslator.EXPECT().AsRemote().
			Return(sim.RemotePort("CP.ToAddressTranslators")).AnyTimes()
		toCaches.EXPECT().AsRemote().
			Return(sim.RemotePort("CP.ToCaches")).AnyTimes()
		toCUs.EXPECT().AsRemote().
			Return(sim.RemotePort("CP.ToCUs")).AnyTimes()
		toTLBs.EXPECT().AsRemote().
			Return(sim.RemotePort("CP.ToTLBs")).AnyTimes()
		l1Translator.EXPECT().AsRemote().
			Return(sim.RemotePort("L1Translator")).AnyTimes()
		l2Translator.EXPECT().AsRemote().
			Return(sim.RemotePort("L2Translator")).AnyTimes()
		tlbPort.EXPECT().AsRemote().
			Return(sim.RemotePort("TLB")).AnyTimes()

		commandProcessor = &CommandProcessor{
			PreCacheTranslators: TranslatorControlGroup{
				Ports: []sim.Port{l1Translator},
			},
			PostCacheTranslators: TranslatorControlGroup{
				Ports: []sim.Port{l2Translator},
			}, // sbin_codex
			TLBs:                 []sim.Port{tlbPort},
			CUs:                  []sim.RemotePort{"CU"},
			ToAddressTranslators: toAddressTranslator,
			ToCaches:             toCaches,
			ToCUs:                toCUs,
			ToTLBs:               toTLBs,
			currShootdownRequest: &protocol.ShootDownCommand{
				VAddr: []uint64{0x1000},
				PID:   1,
			},
			shootDownInProcess: true,
		}
		middleware = &ctrlMiddleware{commandProcessor}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("quiesces L2 translators after cache acknowledgements and before TLB flush", func() {
		// Given
		commandProcessor.numCacheACK = 1
		cacheRsp := cache.FlushRspBuilder{}.Build()
		translatorRsp := mem.ControlMsgBuilder{}.
			WithSrc(l2Translator.AsRemote()).
			WithDst(toAddressTranslator.AsRemote()).
			ToNotifyDone().
			Build()
		var discardReq *mem.ControlMsg
		var tlbFlushReq *tlb.FlushReq
		gomock.InOrder( // sbin_codex: the shared response port must gate TLB flush on the L2 discard acknowledgement.
			toCaches.EXPECT().RetrieveIncoming(),
			toAddressTranslator.EXPECT().
				Send(gomock.AssignableToTypeOf(&mem.ControlMsg{})).
				DoAndReturn(func(msg sim.Msg) *sim.SendError {
					discardReq = msg.(*mem.ControlMsg)
					return nil
				}),
			toAddressTranslator.EXPECT().PeekIncoming().Return(translatorRsp),
			toTLBs.EXPECT().
				Send(gomock.AssignableToTypeOf(&tlb.FlushReq{})).
				DoAndReturn(func(msg sim.Msg) *sim.SendError {
					tlbFlushReq = msg.(*tlb.FlushReq)
					return nil
				}),
			toAddressTranslator.EXPECT().RetrieveIncoming(),
		)

		// When
		cacheProgress := middleware.processCacheFlushRsp(cacheRsp)
		translatorProgress := middleware.processRspFromATs()

		// Then
		Expect(cacheProgress).To(BeTrue())
		Expect(translatorProgress).To(BeTrue())
		Expect(discardReq.DiscardTransations).To(BeTrue())
		Expect(discardReq.Dst).To(Equal(l2Translator.AsRemote()))
		Expect(tlbFlushReq.PID).To(Equal(commandProcessor.currShootdownRequest.PID))
		Expect(tlbFlushReq.VAddr).To(Equal(commandProcessor.currShootdownRequest.VAddr))
		Expect(commandProcessor.numTLBAck).To(Equal(uint64(1)))
	})

	It("preserves direct TLB flush when there are no L2 translators", func() {
		// Given
		commandProcessor.PostCacheTranslators = TranslatorControlGroup{} // sbin_codex
		commandProcessor.numCacheACK = 1
		cacheRsp := cache.FlushRspBuilder{}.Build()
		var tlbFlushReq *tlb.FlushReq
		gomock.InOrder( // sbin_codex: baseline configurations retain the existing cache-to-TLB path.
			toCaches.EXPECT().RetrieveIncoming(),
			toTLBs.EXPECT().
				Send(gomock.AssignableToTypeOf(&tlb.FlushReq{})).
				DoAndReturn(func(msg sim.Msg) *sim.SendError {
					tlbFlushReq = msg.(*tlb.FlushReq)
					return nil
				}),
		)

		// When
		madeProgress := middleware.processCacheFlushRsp(cacheRsp)

		// Then
		Expect(madeProgress).To(BeTrue())
		Expect(tlbFlushReq).NotTo(BeNil())
		Expect(commandProcessor.numTLBAck).To(Equal(uint64(1)))
	})

	It("restarts L1 and L2 translators after TLBs and before CUs", func() {
		// Given
		commandProcessor.numTLBAck = 1
		tlbRsp := tlb.RestartRspBuilder{}.Build()
		l1Rsp := mem.ControlMsgBuilder{}.
			WithSrc(l1Translator.AsRemote()).
			WithDst(toAddressTranslator.AsRemote()).
			ToNotifyDone().
			Build()
		l2Rsp := mem.ControlMsgBuilder{}.
			WithSrc(l2Translator.AsRemote()).
			WithDst(toAddressTranslator.AsRemote()).
			ToNotifyDone().
			Build()
		var l1RestartReq *mem.ControlMsg
		var l2RestartReq *mem.ControlMsg
		gomock.InOrder( // sbin_codex: both translator lists acknowledge restart before the CU restart is emitted.
			toAddressTranslator.EXPECT().
				Send(gomock.AssignableToTypeOf(&mem.ControlMsg{})).
				DoAndReturn(func(msg sim.Msg) *sim.SendError {
					l1RestartReq = msg.(*mem.ControlMsg)
					return nil
				}),
			toAddressTranslator.EXPECT().
				Send(gomock.AssignableToTypeOf(&mem.ControlMsg{})).
				DoAndReturn(func(msg sim.Msg) *sim.SendError {
					l2RestartReq = msg.(*mem.ControlMsg)
					return nil
				}),
			toTLBs.EXPECT().RetrieveIncoming(),
			toAddressTranslator.EXPECT().PeekIncoming().Return(l1Rsp),
			toAddressTranslator.EXPECT().RetrieveIncoming(),
			toAddressTranslator.EXPECT().PeekIncoming().Return(l2Rsp),
			toCUs.EXPECT().
				Send(gomock.AssignableToTypeOf(&protocol.CUPipelineRestartReq{})),
			toAddressTranslator.EXPECT().RetrieveIncoming(),
		)

		// When
		tlbProgress := middleware.processTLBRestartRsp(tlbRsp)
		l1Progress := middleware.processRspFromATs()
		l2Progress := middleware.processRspFromATs()

		// Then
		Expect(tlbProgress).To(BeTrue())
		Expect(l1Progress).To(BeTrue())
		Expect(l2Progress).To(BeTrue())
		Expect(l1RestartReq.Restart).To(BeTrue())
		Expect(l1RestartReq.Dst).To(Equal(l1Translator.AsRemote()))
		Expect(l2RestartReq.Restart).To(BeTrue())
		Expect(l2RestartReq.Dst).To(Equal(l2Translator.AsRemote()))
		Expect(commandProcessor.numCUAck).To(Equal(uint64(1)))
	})
})
