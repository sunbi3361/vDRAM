package timingconfig

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	ideall1tlb "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/ideal-l1tlb"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
	virtualcaching "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/virtual-caching" // sbin_codex: virtual-caching selector coverage.
)

// Compile-time assertions that the concrete builder packages satisfy the
// gpubuilder.GPUBuilder interface without product API changes.
var _ gpubuilder.GPUBuilder = r9nano.MakeBuilder()
var _ gpubuilder.GPUBuilder = ideall1tlb.MakeBuilder()

var _ = Describe("GPU builder selector", func() {
	var testSimulation *simulation.Simulation
	var mmuComp *mmu.Comp

	BeforeEach(func() {
		outputPrefix := filepath.Join(GinkgoT().TempDir(), "timingconfig-selector")

		testSimulation = simulation.MakeBuilder().
			WithoutMonitoring().
			WithOutputFileName(outputPrefix).
			Build()

		mmuComp = mmu.MakeBuilder().
			WithEngine(testSimulation.GetEngine()).
			WithLog2PageSize(12).
			Build("MMU")

		testSimulation.RegisterComponent(mmuComp)

		DeferCleanup(func() {
			testSimulation.Terminate()
			artifacts, err := filepath.Glob(outputPrefix + "_*.sqlite3")
			Expect(err).NotTo(HaveOccurred())
			for _, artifact := range artifacts {
				Expect(os.Remove(artifact)).To(Succeed())
			}
		})
	})

	DescribeTable("dispatching createGPUBuilder by gpu type",
		func(gpuType string, expected interface{}) {
			builder := MakeBuilder().
				WithSimulation(testSimulation).
				WithGPUType(gpuType)

			gpuBuilder := builder.createGPUBuilder(mmuComp)

			Expect(gpuBuilder).To(BeAssignableToTypeOf(expected))
		},
		Entry("r9nano returns r9nano.Builder", "r9nano", r9nano.Builder{}),
		Entry("ideal-l1tlb returns ideall1tlb.Builder", "ideal-l1tlb", ideall1tlb.Builder{}),
		// Entry("virtual-caching returns virtualcaching.Builder", "virtual-caching", virtualcaching.Builder{}),
		// sbin_codex: selector target.
		Entry("virtual-caching returns virtualcaching.Builder", "virtual-caching", virtualcaching.Builder{}),
		Entry("unknown selector falls back to r9nano.Builder", "not-a-gpu", r9nano.Builder{}),
	)
})
