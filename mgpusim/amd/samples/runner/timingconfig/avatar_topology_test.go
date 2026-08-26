// sbin_claude_avatar
package timingconfig

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/asu"
	avatarmeta "github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// buildAvatarPlatform builds a single-GPU avatar platform and returns the
// pieces the specs need.
func buildAvatarPlatform(
	compressRatio float64,
) (
	testSimulation *simulation.Simulation,
	gpuDriver *driver.Driver,
	speculationUnit *asu.Comp,
	ctx *driver.Context,
) {
	outputPrefix := filepath.Join(GinkgoT().TempDir(), "avatar-topology")
	testSimulation = simulation.MakeBuilder().
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
		WithGPUType("avatar").
		WithAvatar(AvatarPlatformConfig{
			CompressRatio: compressRatio,
			// Far below any L2-TLB/page-walk round trip, so a validated
			// speculation reliably beats the real translation.
			ValidationLatency: 5,
		}).
		Build()

	gpuDriver = testSimulation.GetComponentByName("Driver").(*driver.Driver)
	speculationUnit = testSimulation.
		GetComponentByName("GPU[1].ASU").(*asu.Comp)
	ctx = gpuDriver.Init()

	return testSimulation, gpuDriver, speculationUnit, ctx
}

var _ = Describe("Avatar speculation topology", func() {
	var (
		testSimulation  *simulation.Simulation
		gpuDriver       *driver.Driver
		registry        *avatarmeta.Registry
		speculationUnit *asu.Comp
		ctx             *driver.Context
		agent           *cpuRemoteTestAgent
	)

	BeforeEach(func() {
		testSimulation, gpuDriver, speculationUnit, ctx =
			buildAvatarPlatform(1.0)
		registry = gpuDriver.AvatarRegistry()

		// One shared agent stands in for one L1 TLB bottom port, so every
		// miss trains the same per-requester MOD table.
		agent = newCPURemoteTestAgent(testSimulation.GetEngine())
		testSimulation.RegisterComponent(agent)
		l1ToL2 := testSimulation.GetComponentByName(
			"GPU[1].L1TLBToL2TLB").(*directconnection.Comp)
		l1ToL2.PlugIn(agent.port)
	})

	// translate pushes one TranslationReq into the ASU top port the way an
	// L1 TLB miss does and runs the engine until the response arrives.
	translate := func(vAddr uint64) vm.Page {
		req := vm.TranslationReqBuilder{}.
			WithSrc(agent.port.AsRemote()).
			WithDst(speculationUnit.TopPort().AsRemote()).
			WithVAddr(vAddr).
			WithPID(ctx.PID()).
			WithDeviceID(1).
			Build()
		agent.received = nil
		agent.pending = req
		agent.TickLater()
		Expect(testSimulation.GetEngine().Run()).To(Succeed())

		rsp := agent.received.(*vm.TranslationRsp)
		Expect(rsp.RespondTo).To(Equal(req.ID))

		return rsp.Page
	}

	It("builds the ASU between the L1 TLBs and the L2 TLB", func() {
		Expect(speculationUnit).NotTo(BeNil())
		Expect(speculationUnit.GetPortByName("Top")).NotTo(BeNil())
		Expect(speculationUnit.GetPortByName("Bottom")).NotTo(BeNil())
		Expect(testSimulation.GetComponentByName("GPU[1].ASUToL2TLB")).
			NotTo(BeNil())
		Expect(registry).NotTo(BeNil())
	})

	It("places pages at 2MB-region granularity (avatar-plan.md 1.4)", func() {
		// A buffer spanning three 2MB regions binds three physical regions.
		base := uint64(gpuDriver.AllocateMemory(
			ctx, 3*avatarmeta.RegionBytes))
		bound, _ := registry.Occupancy(1)
		Expect(bound).To(BeNumerically(">=", 3))

		// Within one region the V2P offset is constant.
		pageA := translate(base)
		pageB := translate(base + 4096)
		Expect(pageB.PAddr - pageA.PAddr).To(Equal(uint64(4096)))

		// Across regions the offsets differ (randomized placement).
		regionStride := avatarmeta.RegionBytes
		pageC := translate(((base / regionStride) + 1) * regionStride)
		offsetAB := pageA.PAddr - pageA.VAddr
		offsetC := pageC.PAddr - pageC.VAddr
		Expect(offsetC).NotTo(Equal(offsetAB))
	})

	It("answers a confident miss early through CAVA and swallows the real "+
		"response (avatar validation tests 1 and 5)", func() {
		base := uint64(gpuDriver.AllocateMemory(ctx, 16*4096))

		// Two real translations train the region's MOD entry to the
		// speculation threshold; neither may speculate yet.
		translate(base)
		translate(base + 4096)
		Expect(speculationUnit.Stats().Speculations).To(Equal(uint64(0)))

		// The third miss speculates, CAVA validates against the embedded
		// metadata, and the request completes before the real translation.
		page := translate(base + 2*4096)
		Expect(page.Valid).To(BeTrue())
		Expect(page.PAddr - page.VAddr).To(
			Equal(translate(base).PAddr - base))

		stats := speculationUnit.Stats()
		Expect(stats.Speculations).To(BeNumerically(">=", uint64(1)))
		Expect(stats.CAVAPass).To(BeNumerically(">=", uint64(1)))
		Expect(stats.EarlyCompletions).To(BeNumerically(">=", uint64(1)))
		// The late page-walk response was swallowed, never completing the
		// request twice.
		Expect(stats.SwallowedRsps).To(Equal(stats.EarlyCompletions))
	})
})

var _ = Describe("Avatar with incompressible memory", func() {
	It("keeps speculative data unguaranteed until the real translation "+
		"(avatar validation test 2)", func() {
		testSimulation, gpuDriver, speculationUnit, ctx :=
			buildAvatarPlatform(1e-12) // effectively nothing compressible

		agent := newCPURemoteTestAgent(testSimulation.GetEngine())
		testSimulation.RegisterComponent(agent)
		l1ToL2 := testSimulation.GetComponentByName(
			"GPU[1].L1TLBToL2TLB").(*directconnection.Comp)
		l1ToL2.PlugIn(agent.port)

		translate := func(vAddr uint64) vm.Page {
			req := vm.TranslationReqBuilder{}.
				WithSrc(agent.port.AsRemote()).
				WithDst(speculationUnit.TopPort().AsRemote()).
				WithVAddr(vAddr).
				WithPID(ctx.PID()).
				WithDeviceID(1).
				Build()
			agent.received = nil
			agent.pending = req
			agent.TickLater()
			Expect(testSimulation.GetEngine().Run()).To(Succeed())

			rsp := agent.received.(*vm.TranslationRsp)
			Expect(rsp.RespondTo).To(Equal(req.ID))

			return rsp.Page
		}

		base := uint64(gpuDriver.AllocateMemory(ctx, 16*4096))
		translate(base)
		translate(base + 4096)
		page := translate(base + 2*4096)

		// The speculation launched, but with no embedded page information
		// the sector stays unguaranteed: the conventional translation
		// completes the request and nothing is swallowed.
		Expect(page.Valid).To(BeTrue())
		stats := speculationUnit.Stats()
		Expect(stats.Speculations).To(BeNumerically(">=", uint64(1)))
		Expect(stats.CAVAIncompressible).To(BeNumerically(">=", uint64(1)))
		Expect(stats.EarlyCompletions).To(Equal(uint64(0)))
		Expect(stats.SwallowedRsps).To(Equal(uint64(0)))
	})
})
