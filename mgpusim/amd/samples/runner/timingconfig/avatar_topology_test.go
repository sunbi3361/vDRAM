// sbin_claude_avatar
package timingconfig

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/asu"
	avatarmeta "github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// avatarBurstAgent stands in for the L1 TLB bottom ports and can keep many
// translations in flight at once. Speculation only wins the race when the
// GMMU walkers are saturated - exactly the regime the real benchmarks run
// in - so the CAVA specs drive a burst of misses, not one at a time.
// sbin_claude_avatar v2
type avatarBurstAgent struct {
	*sim.TickingComponent
	port     sim.Port
	pending  []sim.Msg
	received map[string]int // req ID -> number of responses
}

func newAvatarBurstAgent(engine sim.Engine) *avatarBurstAgent {
	agent := &avatarBurstAgent{received: map[string]int{}}
	agent.TickingComponent = sim.NewTickingComponent(
		"CPU.AvatarBurstAgent", engine, sim.GHz, agent)
	agent.port = sim.NewPort(agent, 64, 64, "CPU.AvatarBurstAgent.Port")
	agent.AddPort("Port", agent.port)

	return agent
}

func (a *avatarBurstAgent) Tick() bool {
	madeProgress := false

	for {
		rsp := a.port.RetrieveIncoming()
		if rsp == nil {
			break
		}
		a.received[rsp.(*vm.TranslationRsp).RespondTo]++
		madeProgress = true
	}

	for len(a.pending) > 0 {
		if err := a.port.Send(a.pending[0]); err != nil {
			break
		}
		a.pending = a.pending[1:]
		madeProgress = true
	}

	return madeProgress
}

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

	// Pre-edit v1 spec (behavior superseded, commented per project
	// convention): with the flat 5-cycle validation countdown a single
	// sequential miss reliably completed early and the late walk response
	// was always swallowed. In v2 the sector fetch is a real DRAM read, so
	// on an idle platform the conventional path wins; speculation pays off
	// exactly when the GMMU walkers are saturated - and a validated
	// speculation now cancels its walk instead of swallowing it.
	It("answers confident misses early through CAVA under walker "+
		"saturation and retires the redundant walks "+
		"(avatar validation tests 1 and 5)", func() {
		burst := newAvatarBurstAgent(testSimulation.GetEngine())
		testSimulation.RegisterComponent(burst)
		l1ToL2 := testSimulation.GetComponentByName(
			"GPU[1].L1TLBToL2TLB").(*directconnection.Comp)
		l1ToL2.PlugIn(burst.port)

		const numPages = 64
		base := uint64(gpuDriver.AllocateMemory(ctx, numPages*4096))

		// sendBurst pushes misses through the burst agent - the MOD is
		// per-requester, so training and burst must share one source port.
		sendBurst := func(vAddrs []uint64) []string {
			reqIDs := []string{}
			for _, vAddr := range vAddrs {
				req := vm.TranslationReqBuilder{}.
					WithSrc(burst.port.AsRemote()).
					WithDst(speculationUnit.TopPort().AsRemote()).
					WithVAddr(vAddr).
					WithPID(ctx.PID()).
					WithDeviceID(1).
					Build()
				burst.pending = append(burst.pending, req)
				reqIDs = append(reqIDs, req.ID)
			}
			burst.TickLater()
			Expect(testSimulation.GetEngine().Run()).To(Succeed())

			return reqIDs
		}

		// Two real translations train the region's MOD entry to the
		// speculation threshold; neither may speculate yet.
		sendBurst([]uint64{base})
		sendBurst([]uint64{base + 4096})
		Expect(speculationUnit.Stats().Speculations).To(Equal(uint64(0)))

		// A burst of misses saturates the 16 GMMU walkers: the walk queue
		// grows while the speculative sector fetches ride the idle data
		// hierarchy, so CAVA validates and answers early.
		burstAddrs := []uint64{}
		for i := 2; i < numPages; i++ {
			burstAddrs = append(burstAddrs, base+uint64(i)*4096)
		}
		reqIDs := sendBurst(burstAddrs)

		// Every miss was answered exactly once (refs 5.12: no duplicate
		// completion), even though two paths raced for each of them.
		for _, id := range reqIDs {
			Expect(burst.received[id]).To(Equal(1))
		}

		stats := speculationUnit.Stats()
		Expect(stats.Speculations).To(BeNumerically(">=", uint64(1)))
		// sbin_claude_avatar v3: the ASU issues no sector fetch of its own;
		// CAVA rides the requester's demand access (refs 5.3, 5.6).
		Expect(stats.CAVAPass).To(BeNumerically(">=", uint64(1)))
		Expect(stats.EarlyCompletions).To(BeNumerically(">=", uint64(1)))
		// Each validated speculation retired its conventional walk: the
		// forward was suppressed outright, canceled at the L2 TLB/GMMU, or
		// - when the cancel lost the race - its response was swallowed.
		Expect(stats.ForwardsSuppressed+stats.WalkCancelsSent+
			stats.SwallowedRsps).To(Equal(stats.EarlyCompletions))
	})
})

var _ = Describe("Avatar with incompressible memory", func() {
	It("keeps speculative data unguaranteed until the real translation "+
		"(avatar validation test 2)", func() {
		testSimulation, gpuDriver, speculationUnit, ctx :=
			buildAvatarPlatform(1e-12) // effectively nothing compressible

		burst := newAvatarBurstAgent(testSimulation.GetEngine())
		testSimulation.RegisterComponent(burst)
		l1ToL2 := testSimulation.GetComponentByName(
			"GPU[1].L1TLBToL2TLB").(*directconnection.Comp)
		l1ToL2.PlugIn(burst.port)

		// sendBurst pushes misses through the burst agent - the MOD is
		// per-requester, so training and burst must share one source port.
		sendBurst := func(vAddrs []uint64) []string {
			reqIDs := []string{}
			for _, vAddr := range vAddrs {
				req := vm.TranslationReqBuilder{}.
					WithSrc(burst.port.AsRemote()).
					WithDst(speculationUnit.TopPort().AsRemote()).
					WithVAddr(vAddr).
					WithPID(ctx.PID()).
					WithDeviceID(1).
					Build()
				burst.pending = append(burst.pending, req)
				reqIDs = append(reqIDs, req.ID)
			}
			burst.TickLater()
			Expect(testSimulation.GetEngine().Run()).To(Succeed())

			return reqIDs
		}

		const numPages = 64
		base := uint64(gpuDriver.AllocateMemory(ctx, numPages*4096))
		sendBurst([]uint64{base})
		sendBurst([]uint64{base + 4096})

		// sbin_claude_avatar v2: same walker-saturating burst as the CAVA
		// spec, so the verdict is reached before the real translation.
		burstAddrs := []uint64{}
		for i := 2; i < numPages; i++ {
			burstAddrs = append(burstAddrs, base+uint64(i)*4096)
		}
		reqIDs := sendBurst(burstAddrs)

		// The speculations launched, but with no embedded page information
		// the sectors stay unguaranteed: the conventional translation
		// completes every request exactly once and no walk is retired.
		for _, id := range reqIDs {
			Expect(burst.received[id]).To(Equal(1))
		}
		stats := speculationUnit.Stats()
		Expect(stats.Speculations).To(BeNumerically(">=", uint64(1)))
		Expect(stats.CAVAIncompressible).To(BeNumerically(">=", uint64(1)))
		Expect(stats.EarlyCompletions).To(Equal(uint64(0)))
		Expect(stats.SwallowedRsps).To(Equal(uint64(0)))
		Expect(stats.WalkCancelsSent).To(Equal(uint64(0)))
		Expect(stats.ForwardsSuppressed).To(Equal(uint64(0)))
	})
})
