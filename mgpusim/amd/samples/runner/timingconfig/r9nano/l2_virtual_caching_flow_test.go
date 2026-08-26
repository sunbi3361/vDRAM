package r9nano

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

type l2FlowAgent struct {
	*sim.TickingComponent
	port     sim.Port
	pending  []sim.Msg
	received []sim.Msg
}

type l2FlowMessageCapture struct {
	messages     []sim.Msg
	writes       []*mem.WriteReq
	translations []*vm.TranslationReq
}

func (capture *l2FlowMessageCapture) Func(ctx sim.HookCtx) {
	message := ctx.Item.(sim.Msg)
	capture.messages = append(capture.messages, message)
	if write, ok := message.(*mem.WriteReq); ok {
		capture.writes = append(capture.writes, write)
	}
	if translation, ok := message.(*vm.TranslationReq); ok {
		capture.translations = append(capture.translations, translation)
	}
}

func newL2FlowAgent(engine sim.Engine) *l2FlowAgent {
	agent := &l2FlowAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"TestAgent", engine, 1*sim.GHz, agent)
	agent.port = sim.NewPort(agent, 16, 16, "TestAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (agent *l2FlowAgent) Tick() bool {
	progress := false
	for {
		msg := agent.port.RetrieveIncoming()
		if msg == nil {
			break
		}
		agent.received = append(agent.received, msg)
		progress = true
	}
	if len(agent.pending) > 0 && agent.port.CanSend() {
		Expect(agent.port.Send(agent.pending[0])).To(BeNil()) // sbin_codex: Akita Send returns nil on success.
		agent.pending = agent.pending[1:]
		progress = true
	}
	return progress
}

func (agent *l2FlowAgent) enqueue(msg sim.Msg) {
	agent.pending = append(agent.pending, msg)
	agent.TickLater()
}

var _ = Describe("R9 Nano virtual L2 cache flow", func() {
	It("refills a read and writes back a dirty eviction through its matching translator", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("l2-flow")
		const pageSize = 4096
		const virtualA = 0x1000
		const physicalA = 0x8000
		const virtualB = 0x12000
		const physicalB = 0x19000
		originalA := make([]byte, 64)
		originalB := make([]byte, 64)
		for i := range originalA {
			originalA[i] = byte(i + 1)
			originalB[i] = byte(0xa0 + i)
		}
		globalStorage := mem.NewStorage(2 * mem.GB)
		globalStorage.Write(physicalA, originalA)
		gpuPageTable.Insert(vm.Page{
			PID: 1, VAddr: virtualA, PAddr: physicalA, // sbin_codex: virtual traffic uses GPU PID 1.
			PageSize: pageSize, Valid: true,
		})
		for i := 0; i < 17; i++ {
			virtual := uint64(0x2000 + i*0x1000)
			physical := uint64(0x9000 + i*0x1000)
			gpuPageTable.Insert(vm.Page{
				PID: 1, VAddr: virtual, PAddr: physical, // sbin_codex: virtual traffic uses GPU PID 1.
				PageSize: pageSize, Valid: true,
			})
			globalStorage.Write(physical, originalB)
		}
		globalStorage.Write(physicalB, originalB)

		MakeBuilder().
			WithSimulation(testSimulation).
			WithNumShaderArray(1).
			WithNumCUPerShaderArray(1).
			WithNumMemoryBank(2).
			WithL2CacheSize(2 * mem.KB).
			WithDramSize(2 * mem.GB).
			WithGlobalStorage(globalStorage).
			WithMMU(cpuMMU).
			WithDataPathTopology(NewVirtualDataPathTopology()).
			WithMemoryTopology(NewVirtualMemoryTopology()). // sbin_codex
			WithGPUID(1).
			WithPageTable(gpuPageTable).
			WithRDMAAddressMapper(&mem.BankedAddressPortMapper{
				BankSize:   2 * mem.GB,
				LowModules: []sim.RemotePort{"CPU"},
			}).
			Build("GPU")

		agent := newL2FlowAgent(testSimulation.GetEngine())
		l1ToL2 := testSimulation.GetComponentByName("GPU.L1ToL2").(*directconnection.Comp)
		l1ToL2.PlugIn(agent.port)
		l2Top := testSimulation.GetComponentByName(
			"GPU.L2Cache[0]").GetPortByName("Top").AsRemote()
		translatorBottom := testSimulation.GetComponentByName(
			"GPU.L2AddrTrans[0]").GetPortByName("Bottom")
		translatorTop := testSimulation.GetComponentByName(
			"GPU.L2AddrTrans[0]").GetPortByName("Top")
		translatorTranslation := testSimulation.GetComponentByName(
			"GPU.L2AddrTrans[0]").GetPortByName("Translation")
		translatedMessages := &l2FlowMessageCapture{}
		translatorTopMessages := &l2FlowMessageCapture{}
		translationMessages := &l2FlowMessageCapture{}
		translatorBottom.AcceptHook(translatedMessages)
		translatorTop.AcceptHook(translatorTopMessages)
		translatorTranslation.AcceptHook(translationMessages)

		// When
		readA := mem.ReadReqBuilder{}.
			WithSrc(agent.port.AsRemote()).WithDst(l2Top).
			WithAddress(virtualA + 4).WithByteSize(4).WithPID(1).Build()
		agent.enqueue(readA)
		testSimulation.GetEngine().Run()

		// Then
		Expect(agent.received).To(HaveLen(1))
		readRsp, ok := agent.received[0].(*mem.DataReadyRsp)
		Expect(ok).To(BeTrue())
		Expect(readRsp.RespondTo).To(Equal(readA.ID))
		Expect(readRsp.Data).To(Equal(originalA[4:8]))

		// When
		writeA := mem.WriteReqBuilder{}.
			WithSrc(agent.port.AsRemote()).WithDst(l2Top).
			WithAddress(virtualA + 4).WithData([]byte{9, 8, 7, 6}).WithPID(1).Build()
		agent.enqueue(writeA)
		testSimulation.GetEngine().Run()

		// Then
		Expect(agent.received).To(HaveLen(2))
		writeRsp, ok := agent.received[1].(*mem.WriteDoneRsp)
		Expect(ok).To(BeTrue())
		Expect(writeRsp.RespondTo).To(Equal(writeA.ID))

		for i := 0; i < 16; i++ {
			read := mem.ReadReqBuilder{}.
				WithSrc(agent.port.AsRemote()).WithDst(l2Top).
				WithAddress(uint64(0x2000+i*0x1000) + 4).
				WithByteSize(4).WithPID(1).Build()
			agent.enqueue(read)
			testSimulation.GetEngine().Run()
		}

		// When
		readB := mem.ReadReqBuilder{}.
			WithSrc(agent.port.AsRemote()).WithDst(l2Top).
			WithAddress(virtualB + 4).WithByteSize(4).WithPID(1).Build()
		agent.enqueue(readB)
		testSimulation.GetEngine().Run()

		// Then
		Expect(agent.received).To(HaveLen(19))
		refillRsp, ok := agent.received[18].(*mem.DataReadyRsp)
		Expect(ok).To(BeTrue())
		Expect(refillRsp.RespondTo).To(Equal(readB.ID))
		Expect(refillRsp.Data).To(Equal(originalB[4:8]))
		Expect(translatorTopMessages.writes).To(HaveLen(2))
		Expect(translatedMessages.writes).To(HaveLen(2))
		Expect(translatedMessages.writes[0].GetAddress()).To(Equal(uint64(physicalA)))
		Expect(translatedMessages.writes[0].Meta().Dst).To(Equal(
			sim.RemotePort("GPU.DRAM[0].TopPort")))
		Expect(translationMessages.translations).NotTo(BeEmpty())
		for _, translation := range translationMessages.translations {
			// Pre-edit code (commented per project convention). Fill
			// translations used to target the L1 TLBs' port:
			// Expect(translation.Dst).To(Equal(sim.RemotePort("GPU.L2TLB.TopPort")))
			//
			// sbin_claude_vc: they now target the dedicated fill channel.
			Expect(translation.Dst).To(Equal(
				sim.RemotePort("GPU.L2TLB.TopPort[1]")))
		}
		writtenBack, err := globalStorage.Read(physicalA+4, 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(writtenBack).To(Equal([]byte{9, 8, 7, 6}))
	})
})
