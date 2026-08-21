package r9nano

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/sarchlab/mgpusim/v4/amd/timing/cp"
)

type l2ShootdownDeadline struct {
	agent          *l2FlowAgent
	reached        bool
	completionSeen bool
}

func (d *l2ShootdownDeadline) Handle(sim.Event) error {
	d.reached = true
	for _, message := range d.agent.received {
		if _, ok := message.(*protocol.ShootDownCompleteRsp); ok {
			d.completionSeen = true
		}
	}
	return nil
}

var _ = Describe("R9 Nano dirty L2 shootdown", func() { // sbin_codex: regression for L2-boundary translator ordering.
	It("persists a dirty translated line and completes before the simulation deadline", func() {
		// Given
		testSimulation, gpuPageTable, cpuMMU := newR9NanoTestSimulation("l2-shootdown")
		const pageSize = 4096
		const virtualAddress = 0x1000
		const physicalAddress = 0x8000
		original := make([]byte, 64)
		for i := range original {
			original[i] = byte(i + 1)
		}
		updated := []byte{9, 8, 7, 6}
		globalStorage := mem.NewStorage(2 * mem.GB)
		globalStorage.Write(physicalAddress, original)
		gpuPageTable.Insert(vm.Page{
			PID: 1, VAddr: virtualAddress, PAddr: physicalAddress, // sbin_codex: PID 1 virtual page maps to backing storage.
			PageSize: pageSize, Valid: true,
		})

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

		engine := testSimulation.GetEngine()
		memoryAgent := newL2FlowAgent(engine)
		l1ToL2 := testSimulation.GetComponentByName("GPU.L1ToL2").(*directconnection.Comp)
		l1ToL2.PlugIn(memoryAgent.port)
		l2Top := testSimulation.GetComponentByName(
			"GPU.L2Cache[0]").GetPortByName("Top").AsRemote()

		controlAgent := &l2FlowAgent{}
		controlAgent.TickingComponent = sim.NewTickingComponent(
			"ShootdownAgent", engine, 1*sim.GHz, controlAgent)
		controlAgent.port = sim.NewPort(
			controlAgent, 16, 16, "ShootdownAgent.Port")
		controlAgent.AddPort("Port", controlAgent.port)
		commandProcessor := testSimulation.GetComponentByName(
			"GPU.CommandProcessor").(*cp.CommandProcessor)
		commandProcessor.Driver = controlAgent.port
		driverConnection := directconnection.MakeBuilder().
			WithEngine(engine).
			WithFreq(1 * sim.GHz).
			Build("DriverToGPU")
		testSimulation.RegisterComponent(driverConnection)
		driverConnection.PlugIn(controlAgent.port)
		driverConnection.PlugIn(commandProcessor.ToDriver)

		translatorTopMessages := &l2FlowMessageCapture{}
		translatedMessages := &l2FlowMessageCapture{}
		translator := testSimulation.GetComponentByName("GPU.L2AddrTrans[0]")
		translator.GetPortByName("Top").AcceptHook(translatorTopMessages)
		translator.GetPortByName("Bottom").AcceptHook(translatedMessages)

		read := mem.ReadReqBuilder{}.
			WithSrc(memoryAgent.port.AsRemote()).WithDst(l2Top).
			WithAddress(virtualAddress + 4).WithByteSize(4).WithPID(1).Build()
		memoryAgent.enqueue(read)
		Expect(engine.Run()).To(Succeed())
		write := mem.WriteReqBuilder{}.
			WithSrc(memoryAgent.port.AsRemote()).WithDst(l2Top).
			WithAddress(virtualAddress + 4).WithData(updated).WithPID(1).Build()
		memoryAgent.enqueue(write)
		Expect(engine.Run()).To(Succeed())

		// Then
		Expect(memoryAgent.received).To(HaveLen(2))
		writeRsp, ok := memoryAgent.received[1].(*mem.WriteDoneRsp)
		Expect(ok).To(BeTrue())
		Expect(writeRsp.RespondTo).To(Equal(write.ID))
		persistedBeforeShootdown, err := globalStorage.Read(physicalAddress+4, 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(persistedBeforeShootdown).To(Equal(original[4:8]))
		Expect(translatorTopMessages.writes).To(BeEmpty())
		Expect(translatedMessages.writes).To(BeEmpty())

		// When
		shootdown := protocol.NewShootdownCommand(
			controlAgent.port,
			commandProcessor.ToDriver,
			[]uint64{virtualAddress},
			1,
		)
		controlAgent.enqueue(shootdown)
		deadline := &l2ShootdownDeadline{agent: controlAgent}
		engine.Schedule(sim.NewEventBase(
			engine.CurrentTime()+sim.VTimeInSec(10e-6), deadline)) // sbin_codex: simulation-time bound, never wall-clock sleep.
		Expect(engine.Run()).To(Succeed())

		// Then
		Expect(deadline.reached).To(BeTrue())
		Expect(deadline.completionSeen).To(BeTrue(),
			"dirty L2 shootdown did not complete before the simulation deadline; translator top writes=%d translated writes=%d",
			len(translatorTopMessages.writes), len(translatedMessages.writes))
		// Expect(translatorTopMessages.writes).To(HaveLen(1))
		Expect(translatorTopMessages.writes).To(HaveLen(2)) // sbin_codex: port hooks observe before and after delivery.
		// Expect(translatedMessages.writes).To(HaveLen(1))
		Expect(translatedMessages.writes).To(HaveLen(2)) // sbin_codex: one translated flush write produces two hook observations.
		Expect(translatedMessages.writes[0].GetAddress()).To(Equal(uint64(physicalAddress)))
		persistedAfterShootdown, err := globalStorage.Read(physicalAddress+4, 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(persistedAfterShootdown).To(Equal(updated))
	})
})
