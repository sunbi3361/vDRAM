// sbin_claude_hpt
package timingconfig

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/gmmu"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
)

// hptPlatform is one built platform plus the handles a walk test needs.
type hptPlatform struct {
	simulation *simulation.Simulation
	gpuDriver  *driver.Driver
	gmmu       *gmmu.Comp
	ctx        *driver.Context
}

// buildHPTPlatform builds a single-GPU platform of the given type and
// registers cleanup of its simulation artifacts.
func buildHPTPlatform(gpuType string) *hptPlatform {
	outputPrefix := filepath.Join(GinkgoT().TempDir(), "hpt-"+gpuType)
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

	MakeBuilder().
		WithSimulation(testSimulation).
		WithGPUType(gpuType).
		Build()

	gpuDriver := testSimulation.GetComponentByName("Driver").(*driver.Driver)

	return &hptPlatform{
		simulation: testSimulation,
		gpuDriver:  gpuDriver,
		gmmu: testSimulation.GetComponentByName(
			"GPU[1].GMMU").(*gmmu.Comp),
		ctx: gpuDriver.Init(),
	}
}

// walk pushes one TranslationReq into the GMMU top port the way an L2 TLB
// miss does, runs the engine to quiescence, and reports the page plus the
// time the walk finished at.
func (p *hptPlatform) walk(vAddr uint64) (vm.Page, sim.VTimeInSec) {
	agent := newCPURemoteTestAgent(p.simulation.GetEngine())
	p.simulation.RegisterComponent(agent)

	l2ToGMMU := p.simulation.GetComponentByName(
		"GPU[1].L2TLBToGMMU").(*directconnection.Comp)
	l2ToGMMU.PlugIn(agent.port)

	req := vm.TranslationReqBuilder{}.
		WithSrc(agent.port.AsRemote()).
		WithDst(p.gmmu.GetPortByName("Top").AsRemote()).
		WithVAddr(vAddr).
		WithPID(p.ctx.PID()).
		WithDeviceID(1).
		Build()
	agent.pending = req
	agent.TickLater()

	start := p.simulation.GetEngine().CurrentTime()
	Expect(p.simulation.GetEngine().Run()).To(Succeed())

	rsp := agent.received.(*vm.TranslationRsp)
	Expect(rsp.RespondTo).To(Equal(req.ID))

	return rsp.Page, p.simulation.GetEngine().CurrentTime() - start
}

var _ = Describe("HPT walk mode", func() {
	It("puts the GMMU in hashed mode and builds no page-walk cache", func() {
		platform := buildHPTPlatform("hpt")

		Expect(platform.gmmu.HashedPageTableEnabled()).To(BeTrue())
		Expect(platform.gmmu.HasPageWalkCache()).To(BeFalse())
	})

	It("leaves the baseline GMMU walking the radix table", func() {
		platform := buildHPTPlatform("r9nano")

		Expect(platform.gmmu.HashedPageTableEnabled()).To(BeFalse())
		Expect(platform.gmmu.HasPageWalkCache()).To(BeTrue())
	})

	It("resolves a translation in one modeled memory access", func() {
		platform := buildHPTPlatform("hpt")
		vAddr := uint64(platform.gpuDriver.AllocateMemory(platform.ctx, 4096))

		page, _ := platform.walk(vAddr)

		Expect(page.Valid).To(BeTrue())
		Expect(page.VAddr).To(Equal(vAddr))

		stats := platform.gmmu.HPTStats()
		Expect(stats.Walks).To(Equal(uint64(1)))
		Expect(stats.MemoryAccesses).To(Equal(uint64(1)))
	})

	It("finishes a walk faster than the radix baseline", func() {
		hpt := buildHPTPlatform("hpt")
		hptPage, hptDuration := hpt.walk(
			uint64(hpt.gpuDriver.AllocateMemory(hpt.ctx, 4096)))

		radix := buildHPTPlatform("r9nano")
		radixPage, radixDuration := radix.walk(
			uint64(radix.gpuDriver.AllocateMemory(radix.ctx, 4096)))

		// Both paths must resolve the same mapping...
		Expect(hptPage.PAddr).To(Equal(radixPage.PAddr))
		// ...and the hashed walk must be the cheaper one, which is the whole
		// point of the scheme and proves the branch is on the critical path.
		Expect(hptDuration).To(BeNumerically("<", radixDuration))
	})

	It("keeps the radix GMMU free of hashed-walk statistics", func() {
		platform := buildHPTPlatform("r9nano")
		platform.walk(
			uint64(platform.gpuDriver.AllocateMemory(platform.ctx, 4096)))

		Expect(platform.gmmu.HPTStats()).To(Equal(gmmu.HPTStats{}))
	})
})
