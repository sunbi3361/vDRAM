// sbin_claude_utopia
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
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/rsw"
)

var _ = Describe("Utopia translation topology", func() {
	var (
		testSimulation *simulation.Simulation
		gpuDriver      *driver.Driver
		registry       *restseg.Registry
		utu            *rsw.Comp
		ctx            *driver.Context
	)

	BeforeEach(func() {
		outputPrefix := filepath.Join(GinkgoT().TempDir(), "utopia-topology")
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
			WithGPUType("utopia").
			WithUtopia(UtopiaPlatformConfig{
				RestSegBytes:  1 << 20, // 1MB -> 256 frames, 64 sets @ 4 ways
				Associativity: 4,
			}).
			Build()

		gpuDriver = testSimulation.GetComponentByName("Driver").(*driver.Driver)
		registry = gpuDriver.UtopiaRegistry()
		utu = testSimulation.GetComponentByName("GPU[1].UTU").(*rsw.Comp)
		ctx = gpuDriver.Init()
	})

	// translate pushes one TranslationReq into the UTU top port the way an
	// L2 TLB miss does and runs the engine until the response arrives.
	translate := func(vAddr uint64) vm.Page {
		agent := newCPURemoteTestAgent(testSimulation.GetEngine())
		testSimulation.RegisterComponent(agent)

		l2ToUTU := testSimulation.GetComponentByName(
			"GPU[1].L2TLBToUTU").(*directconnection.Comp)
		l2ToUTU.PlugIn(agent.port)

		req := vm.TranslationReqBuilder{}.
			WithSrc(agent.port.AsRemote()).
			WithDst(utu.GetPortByName("Top").AsRemote()).
			WithVAddr(vAddr).
			WithPID(ctx.PID()).
			WithDeviceID(1).
			Build()
		agent.pending = req
		agent.TickLater()
		Expect(testSimulation.GetEngine().Run()).To(Succeed())

		rsp := agent.received.(*vm.TranslationRsp)
		Expect(rsp.RespondTo).To(Equal(req.ID))

		return rsp.Page
	}

	It("builds the UTU between the L2 TLB and the GMMU", func() {
		Expect(utu).NotTo(BeNil())
		Expect(utu.GetPortByName("Top")).NotTo(BeNil())
		Expect(utu.GetPortByName("Bottom")).NotTo(BeNil())
		Expect(testSimulation.GetComponentByName("GPU[1].L2TLBToUTU")).
			NotTo(BeNil())
		Expect(testSimulation.GetComponentByName("GPU[1].UTUToGMMU")).
			NotTo(BeNil())
	})

	It("reserves the RestSeg and places allocations into hashed sets", func() {
		Expect(registry).NotTo(BeNil())
		Expect(registry.HasSegments(1)).To(BeTrue())

		vAddr := uint64(gpuDriver.AllocateMemory(ctx, 4096))

		pAddr, resident := registry.Lookup(1, ctx.PID(), vAddr)
		Expect(resident).To(BeTrue())

		cfg := registry.SegmentConfigs(1)[0]
		set, _, ok := cfg.SetWayOf(pAddr)
		Expect(ok).To(BeTrue())
		Expect(set).To(Equal(cfg.SetOf(vAddr)))
	})

	It("resolves a RestSeg page through the RSW without a page walk "+
		"(utopia.md validation test 1)", func() {
		// Given: a page resident in the RestSeg. It is intentionally absent
		// from the GPU page table, so a FlexSeg walk would panic; a correct
		// response proves the RSW resolved it.
		vAddr := uint64(gpuDriver.AllocateMemory(ctx, 4096))
		expectedPAddr, resident := registry.Lookup(1, ctx.PID(), vAddr)
		Expect(resident).To(BeTrue())

		page := translate(vAddr)

		Expect(page.Valid).To(BeTrue())
		Expect(page.PAddr).To(Equal(expectedPAddr))
		Expect(utu.Stats().RSWHits).To(Equal(uint64(1)))
		Expect(utu.Stats().FlexSegWalks).To(Equal(uint64(0)))
	})

	It("skips the TAR when the Set Filter reports an empty set "+
		"(utopia.md validation test 2)", func() {
		// Given: single-page buffers so RestSeg residents can be freed one by
		// one, plus at least one FlexSeg-mapped page (overflowed set).
		cfg := registry.SegmentConfigs(1)[0]
		numPages := cfg.NumFrames() + cfg.NumSets

		vAddrs := make([]uint64, 0, numPages)
		for i := 0; i < numPages; i++ {
			vAddrs = append(vAddrs,
				uint64(gpuDriver.AllocateMemory(ctx, cfg.PageSize)))
		}

		flexVAddr := uint64(0)
		for _, v := range vAddrs {
			if _, resident := registry.Lookup(1, ctx.PID(), v); !resident {
				flexVAddr = v
				break
			}
		}
		Expect(flexVAddr).NotTo(Equal(uint64(0)))
		set := cfg.SetOf(flexVAddr)

		// When: every RestSeg resident of that set is freed, the Set Filter
		// drops to zero while the FlexSeg mapping stays valid.
		for _, v := range vAddrs {
			if cfg.SetOf(v) != set {
				continue
			}
			if _, resident := registry.Lookup(1, ctx.PID(), v); resident {
				Expect(gpuDriver.FreeMemory(ctx, driver.Ptr(v))).To(Succeed())
			}
		}
		Expect(registry.SFCount(1, flexVAddr)).To(Equal(0))

		page := translate(flexVAddr)

		// Then: the walk was SF-filtered straight to the FlexSeg walker; the
		// TAR was never consulted (utopia.md 4.4).
		Expect(page.Valid).To(BeTrue())
		Expect(utu.Stats().SFFiltered).To(Equal(uint64(1)))
		Expect(utu.Stats().FlexSegWalks).To(Equal(uint64(1)))
		Expect(utu.Stats().RSWMisses).To(Equal(uint64(0)))
		Expect(utu.Stats().TARCacheHits + utu.Stats().TARCacheMisses).
			To(Equal(uint64(0)))
	})

	It("falls back to the FlexSeg walk only after NotInRestSeg "+
		"(utopia.md validation test 3)", func() {
		// Given: more pages than the RestSeg sets can hold, so some pages
		// are FlexSeg-mapped.
		cfg := registry.SegmentConfigs(1)[0]
		numPages := cfg.NumFrames() + cfg.NumSets
		base := uint64(gpuDriver.AllocateMemory(
			ctx, uint64(numPages)*cfg.PageSize))

		flexVAddr := uint64(0)
		for i := 0; i < numPages; i++ {
			vAddr := base + uint64(i)*cfg.PageSize
			if _, resident := registry.Lookup(1, ctx.PID(), vAddr); !resident {
				flexVAddr = vAddr
				break
			}
		}
		Expect(flexVAddr).NotTo(Equal(uint64(0)))

		page := translate(flexVAddr)

		// Then: the translation resolved through the GMMU (FlexSeg walk) and
		// the frame is outside the RestSeg.
		Expect(page.Valid).To(BeTrue())
		Expect(cfg.Contains(page.PAddr)).To(BeFalse())
		Expect(utu.Stats().FlexSegWalks).To(BeNumerically(">=", uint64(1)))
		Expect(utu.Stats().RSWMisses).To(BeNumerically(">=", uint64(1)))
	})
})
