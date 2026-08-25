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
					WithUVM(UVMPlatformConfig{ // sbin_codex
						Enabled:                true,
						FaultLatencyUS:         1,
						AccessCounterEnabled:   true,
						AccessCounterThreshold: 2,
						TBNExpandRatio:         0.51,
						TBNMaxFetchSize:        64 * 1024,
					}).
					Build()
			}).NotTo(Panic())

			// Then: host memory is a plain PCIe endpoint and the UVM access
			// counter lives on the GPU, after translation. // sbin_codex
			cpuMemory := testSimulation.GetComponentByName(
				"CPU.Memory").(*idealmemcontroller.Comp)
			counter := testSimulation.GetComponentByName(
				"GPU[1].UVMAccessCounter").(*accesscounter.Comp)
			gpuMemory := testSimulation.GetComponentByName(
				"GPU[1].DRAM[0]").(*idealmemcontroller.Comp)
			Expect(cpuMemory.Storage).To(BeIdenticalTo(gpuMemory.Storage))
			Expect(counter.GetPortByName("Top")).NotTo(BeNil())
			Expect(counter.GetPortByName("Bottom")).NotTo(BeNil())
			Expect(counter.GetPortByName("Ctrl")).NotTo(BeNil())

			// When: a remote read enters the counter the way a translated
			// CPU-remote access does.
			payload := []byte{9, 8, 7, 6}
			Expect(cpuMemory.Storage.Write(0x100, payload)).To(Succeed())
			agent := newCPURemoteTestAgent(testSimulation.GetEngine())
			testSimulation.RegisterComponent(agent)
			l1ToL2 := testSimulation.GetComponentByName(
				"GPU[1].L1ToL2").(*directconnection.Comp)
			l1ToL2.PlugIn(agent.port)
			request := mem.ReadReqBuilder{}.
				WithSrc(agent.port.AsRemote()).
				WithDst(counter.Top.AsRemote()).
				WithAddress(0x100).
				WithByteSize(uint64(len(payload))).
				WithRemoteDemandInfo(mem.RemoteDemandInfo{
					PID: 7, VAddr: 0x8100, DeviceID: 1,
				}).
				Build()
			agent.pending = request
			agent.TickLater()
			Expect(testSimulation.GetEngine().Run()).To(Succeed())

			// Then: the data comes back through the counter and the remote
			// access was accounted at 64KB granularity.
			response := agent.received.(*mem.DataReadyRsp)
			Expect(response.RespondTo).To(Equal(request.ID))
			Expect(response.Data).To(Equal(payload))
			Expect(response.Meta().Dst).To(Equal(agent.port.AsRemote()))
			Expect(response.Meta().Src).To(Equal(counter.Top.AsRemote()))

			snapshot := counter.Snapshot()
			Expect(snapshot.RemoteAccesses).To(Equal(uint64(1)))
			Expect(snapshot.Regions).To(HaveLen(1))
			Expect(snapshot.Regions[0].Key.RegionBase).To(Equal(uint64(0)))
		},
		Entry("r9nano baseline", "r9nano"),
		Entry("virtual-caching", "virtual-caching"),
	)
})
