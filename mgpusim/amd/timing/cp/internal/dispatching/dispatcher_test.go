package dispatching

import (
	"debug/elf"
	"io"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
	"github.com/sarchlab/mgpusim/v4/amd/kernels"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"go.uber.org/mock/gomock"
)

var _ = Describe("Dispatcher", func() {
	var (
		ctrl *gomock.Controller

		cp              *MockNamedHookable
		alg             *MockAlgorithm
		dispatchingPort *MockPort
		respondingPort  *MockPort

		dispatcher *DispatcherImpl
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())

		cp = NewMockNamedHookable(ctrl)
		cp.EXPECT().Name().Return("CP").AnyTimes()
		cp.EXPECT().NumHooks().Return(0).AnyTimes()
		cp.EXPECT().InvokeHook(gomock.Any()).AnyTimes()
		alg = NewMockAlgorithm(ctrl)
		dispatchingPort = NewMockPort(ctrl)
		respondingPort = NewMockPort(ctrl)

		dispatchingPort.EXPECT().AsRemote().AnyTimes()
		respondingPort.EXPECT().AsRemote().AnyTimes()

		dispatcher = MakeBuilder().
			WithCP(cp).
			WithDispatchingPort(dispatchingPort).
			WithRespondingPort(respondingPort).
			Build("dispatcher").(*DispatcherImpl)

		dispatcher.alg = alg

	})

	AfterEach(func() {
		ctrl.Finish()
	})

	It("should start dispatching a new kernel", func() {
		hsaco := &insts.KernelCodeObject{KernelCodeObjectMeta: &insts.KernelCodeObjectMeta{}}
		packet := &kernels.HsaKernelDispatchPacket{}
		packetAddr := uint64(0x40)

		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		req.CodeObject = hsaco
		req.Packet = packet
		req.PacketAddress = packetAddr

		alg.EXPECT().StartNewKernel(kernels.KernelLaunchInfo{
			CodeObject: hsaco,
			Packet:     packet,
			PacketAddr: packetAddr,
		})
		alg.EXPECT().NumWG().Return(0)

		dispatcher.StartDispatching(req)

		Expect(dispatcher.dispatching).To(BeIdenticalTo(req))
	})

	It("should panic if the dispatcher is dispatching another kernel", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		Expect(func() { dispatcher.StartDispatching(req) }).To(Panic())
	})

	It("should dispatch work-groups", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		alg.EXPECT().HasNext().Return(true).AnyTimes()
		firstCall := alg.EXPECT().Next().Return(dispatchLocation{
			valid:     true,
			cu:        nilPort.AsRemote(),
			locations: make([]protocol.WfDispatchLocation, 1),
		})
		alg.EXPECT().Next().Return(dispatchLocation{
			valid: false,
		}).After(firstCall).AnyTimes()
		dispatchingPort.EXPECT().PeekIncoming().Return(nil).AnyTimes()
		dispatchingPort.EXPECT().Send(gomock.Any()).Return(nil)

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(dispatcher.currWG.valid).To(BeFalse())
		Expect(dispatcher.numDispatchedWGs).To(Equal(1))
		Expect(dispatcher.inflightWGs).To(HaveLen(1))
	})

	It("should wait until cycle left becomes 0", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req
		dispatcher.cycleLeft = 3

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(dispatcher.cycleLeft).To(Equal(2))
	})

	It("should pause if no work-group can be executed", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		dispatchingPort.EXPECT().PeekIncoming().Return(nil)
		alg.EXPECT().HasNext().Return(true).AnyTimes()
		alg.EXPECT().Next().Return(dispatchLocation{
			valid: false,
			cu:    nilPort.AsRemote(),
		})

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeFalse())
		Expect(dispatcher.currWG.valid).To(BeFalse())
		Expect(dispatcher.numDispatchedWGs).To(Equal(0))
	})

	It("should pause if send to CU failed", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		dispatchingPort.EXPECT().PeekIncoming().Return(nil)
		alg.EXPECT().HasNext().Return(true).AnyTimes()
		alg.EXPECT().Next().Return(dispatchLocation{
			valid: true,
			cu:    nilPort.AsRemote(),
		})
		dispatchingPort.EXPECT().
			Send(gomock.Any()).
			Return(sim.NewSendError())

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeFalse())
		Expect(dispatcher.currWG.valid).To(BeTrue())
		Expect(dispatcher.numDispatchedWGs).To(Equal(0))
	})

	It("should do nothing if all work-groups dispatched", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 48

		dispatchingPort.EXPECT().PeekIncoming().Return(nil)
		alg.EXPECT().HasNext().Return(false).AnyTimes()

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeFalse())
	})

	It("should receive work-group complete message", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		mapWGReq := protocol.MapWGReqBuilder{}.Build()
		location := dispatchLocation{}
		dispatcher.inflightWGs[mapWGReq.ID] = location
		dispatcher.originalReqs[mapWGReq.ID] = mapWGReq

		wgCompletionMsg := &protocol.WGCompletionMsg{RspTo: []string{mapWGReq.ID}}

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 48

		alg.EXPECT().HasNext().Return(false).AnyTimes()
		alg.EXPECT().NumWG().Return(64).Times(2)
		alg.EXPECT().FreeResources(location)

		firstPeek := dispatchingPort.EXPECT().
			PeekIncoming().
			Return(wgCompletionMsg)
		dispatchingPort.EXPECT().
			PeekIncoming().
			Return(nil).
			After(firstPeek).
			AnyTimes()
		dispatchingPort.EXPECT().
			RetrieveIncoming()

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(dispatcher.inflightWGs).NotTo(HaveKey(mapWGReq.ID))
	})

	It(`should add kernel overhead after completing the last 
	Work-Group`, func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		mapWGReq := protocol.MapWGReqBuilder{}.Build()
		location := dispatchLocation{}
		dispatcher.inflightWGs[mapWGReq.ID] = location
		dispatcher.originalReqs[mapWGReq.ID] = mapWGReq

		wgCompletionMsg := &protocol.WGCompletionMsg{RspTo: []string{mapWGReq.ID}}

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 63

		alg.EXPECT().HasNext().Return(false).AnyTimes()
		alg.EXPECT().NumWG().Return(64).Times(2)
		alg.EXPECT().FreeResources(location)

		firstPeek := dispatchingPort.EXPECT().
			PeekIncoming().
			Return(wgCompletionMsg)
		dispatchingPort.EXPECT().
			PeekIncoming().
			Return(nil).
			After(firstPeek).
			AnyTimes()
		dispatchingPort.EXPECT().
			RetrieveIncoming()

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(dispatcher.inflightWGs).NotTo(HaveKey(mapWGReq.ID))
		Expect(dispatcher.cycleLeft).
			To(Equal(dispatcher.constantKernelOverhead))
	})

	It(`should ignore response if the request is not sent by the 
	dispatcher`, func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		mapWGReq := protocol.MapWGReqBuilder{}.Build()
		// dispatcher.inflightWGs[mapWGReq.ID] = location

		wgCompletionMsg := &protocol.WGCompletionMsg{RspTo: []string{mapWGReq.ID}}

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 48

		alg.EXPECT().HasNext().Return(false).AnyTimes()
		dispatchingPort.EXPECT().
			PeekIncoming().
			Return(wgCompletionMsg).
			AnyTimes()

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeFalse())
	})

	It("should send response when a kernel is completed", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 64

		alg.EXPECT().HasNext().Return(false).AnyTimes()
		dispatchingPort.EXPECT().PeekIncoming().Return(nil)
		respondingPort.EXPECT().
			Send(gomock.Any()).
			Return(nil)

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeTrue())
		Expect(dispatcher.dispatching).To(BeNil())
	})

	It("should wait if response is failed to send", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 64

		alg.EXPECT().HasNext().Return(false).AnyTimes()
		dispatchingPort.EXPECT().PeekIncoming().Return(nil)
		respondingPort.EXPECT().
			Send(gomock.Any()).
			Return(sim.NewSendError())

		madeProgress := dispatcher.Tick()

		Expect(madeProgress).To(BeFalse())
		Expect(dispatcher.dispatching).To(BeIdenticalTo(req))
	})

	It("should print kernel-info to stderr, not stdout", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		hsaco := &insts.KernelCodeObject{
			KernelCodeObjectMeta: &insts.KernelCodeObjectMeta{},
			Symbol:               &elf.Symbol{Name: "test_kernel"},
		}
		packet := &kernels.HsaKernelDispatchPacket{
			GridSizeX:      8,
			GridSizeY:      4,
			GridSizeZ:      1,
			WorkgroupSizeX: 2,
			WorkgroupSizeY: 2,
			WorkgroupSizeZ: 1,
		}

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		req.CodeObject = hsaco
		req.Packet = packet

		alg.EXPECT().StartNewKernel(gomock.Any())
		alg.EXPECT().NumWG().Return(8)

		stdout, stderr := captureStreams(func() {
			dispatcher.StartDispatching(req)
		})

		Expect(stdout).NotTo(ContainSubstring("[kernel-info]"))
		Expect(stderr).To(ContainSubstring(
			"[kernel-info] test_kernel grid=8x4x1 wg=2x2x1 totalWGs=8"))
	})

	It("should print kernel-progress to stderr, not stdout", func() {
		nilPort := NewMockPort(ctrl)
		nilPort.EXPECT().AsRemote().AnyTimes()

		req := protocol.NewLaunchKernelReq(nilPort, respondingPort)
		dispatcher.dispatching = req

		mapWGReq := protocol.MapWGReqBuilder{}.Build()
		location := dispatchLocation{}
		dispatcher.inflightWGs[mapWGReq.ID] = location
		dispatcher.originalReqs[mapWGReq.ID] = mapWGReq

		wgCompletionMsg := &protocol.WGCompletionMsg{RspTo: []string{mapWGReq.ID}}

		dispatcher.numDispatchedWGs = 64
		dispatcher.numCompletedWGs = 15
		dispatcher.maxInflightWGs = 16

		alg.EXPECT().HasNext().Return(false).AnyTimes()
		alg.EXPECT().NumWG().Return(64).Times(2)
		alg.EXPECT().FreeResources(location)

		firstPeek := dispatchingPort.EXPECT().
			PeekIncoming().
			Return(wgCompletionMsg)
		dispatchingPort.EXPECT().
			PeekIncoming().
			Return(nil).
			After(firstPeek).
			AnyTimes()
		dispatchingPort.EXPECT().
			RetrieveIncoming()

		stdout, stderr := captureStreams(func() {
			dispatcher.Tick()
		})

		Expect(stdout).NotTo(ContainSubstring("[kernel-progress]"))
		Expect(stderr).To(ContainSubstring(
			"[kernel-progress] kernel wave 1/4 (25%) [cap=16 WGs]"))
	})
})

func captureStreams(f func()) (stdout, stderr string) {
	oldStdout, oldStderr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout, os.Stderr = wOut, wErr

	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
		rOut.Close()
		rErr.Close()
	}()

	f()

	wOut.Close()
	wErr.Close()
	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)
	return string(outBytes), string(errBytes)
}
