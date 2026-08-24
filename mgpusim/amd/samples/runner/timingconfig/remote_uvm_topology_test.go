package timingconfig

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/idealmemcontroller"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver" // sbin_codex
	"github.com/sarchlab/mgpusim/v4/amd/timing/accesscounter"
)

type cpuRemoteTestAgent struct { // sbin_codex
	*sim.TickingComponent
	port     sim.Port
	pending  sim.Msg
	received sim.Msg
}

func newCPURemoteTestAgent(engine sim.Engine) *cpuRemoteTestAgent { // sbin_codex
	agent := &cpuRemoteTestAgent{}
	agent.TickingComponent = sim.NewTickingComponent(
		"CPU.RemoteTestAgent", engine, sim.GHz, agent)
	agent.port = sim.NewPort(agent, 4, 4, "CPU.RemoteTestAgent.Port")
	agent.AddPort("Port", agent.port)
	return agent
}

func (a *cpuRemoteTestAgent) Tick() bool { // sbin_codex
	if response := a.port.RetrieveIncoming(); response != nil {
		a.received = response
		return true
	}
	if a.pending == nil {
		return false
	}
	if sendError := a.port.Send(a.pending); sendError != nil {
		return false
	}
	a.pending = nil
	return true
}

var _ = Describe("CPU-remote UVM timing topology", func() { // sbin_codex
	DescribeTable("builds a routed CPU-memory endpoint for timing GPU modes",
		func(gpuType string) {
			// Given
			outputPrefix := filepath.Join(GinkgoT().TempDir(), "remote-uvm-"+gpuType)
			testSimulation := simulation.MakeBuilder().
				WithoutMonitoring().
				WithOutputFileName(outputPrefix).
				Build()
			DeferCleanup(func() {
				testSimulation.Terminate()
				artifacts, err := filepath.Glob(outputPrefix + "_*.sqlite3")
				Expect(err).NotTo(HaveOccurred())
				for _, artifact := range artifacts {
					Expect(os.Remove(artifact)).To(Succeed())
				}
			})

			// When
			Expect(func() {
				MakeBuilder().
					WithSimulation(testSimulation).
					WithGPUType(gpuType).
					WithUVM(true, false, 1, 2, 1, 64*1024).
					Build()
			}).NotTo(Panic())

			// Then
			cpuMemory := testSimulation.GetComponentByName(
				"CPU.Memory").(*idealmemcontroller.Comp)
			counter := testSimulation.GetComponentByName(
				"CPU.AccessCounter").(*accesscounter.Comp)
			gpuDriver := testSimulation.GetComponentByName("Driver").(*driver.Driver) // sbin_codex
			gpuMemory := testSimulation.GetComponentByName(
				"GPU[1].DRAM[0]").(*idealmemcontroller.Comp)
			Expect(cpuMemory.Storage).To(BeIdenticalTo(gpuMemory.Storage))
			Expect(counter.GetPortByName("Top")).NotTo(BeNil())
			Expect(counter.GetPortByName("Bottom")).NotTo(BeNil())
			Expect(gpuDriver.AccessCounterResetDestination()).To(Equal(
				counter.Top.AsRemote())) // sbin_codex

			// When
			payload := []byte{9, 8, 7, 6}
			Expect(cpuMemory.Storage.Write(0x100, payload)).To(Succeed())
			agent := newCPURemoteTestAgent(testSimulation.GetEngine())
			testSimulation.RegisterComponent(agent)
			l1ToL2 := testSimulation.GetComponentByName(
				"GPU[1].L1ToL2").(*directconnection.Comp)
			l1ToL2.PlugIn(agent.port)
			request := mem.ReadReqBuilder{}.
				WithSrc(agent.port.AsRemote()).
				WithDst(testSimulation.GetComponentByName("GPU[1].RDMA").
					GetPortByName("RDMARequestInside").AsRemote()).
				WithAddress(0x100).
				WithByteSize(uint64(len(payload))).
				WithRemoteDemandInfo(mem.RemoteDemandInfo{
					PID: 7, VAddr: 0x8100, DeviceID: 1,
				}).
				Build()
			agent.pending = request
			agent.TickLater()
			Expect(testSimulation.GetEngine().Run()).To(Succeed())

			// Then
			response := agent.received.(*mem.DataReadyRsp)
			Expect(response.RespondTo).To(Equal(request.ID))
			Expect(response.Data).To(Equal(payload))
			Expect(response.Meta().Dst).To(Equal(agent.port.AsRemote()))
			Expect(response.Meta().Src).To(Equal(
				testSimulation.GetComponentByName("GPU[1].RDMA").
					GetPortByName("RDMARequestInside").AsRemote()))
		},
		Entry("r9nano baseline", "r9nano"),
		Entry("virtual-caching", "virtual-caching"),
	)
})
