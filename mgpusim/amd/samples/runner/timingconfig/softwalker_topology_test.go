// sbin_claude_softwalker: SoftWalker platform tests. -gpu=softwalker must
// put the GMMU in software-walk mode, keep the page-walk cache (unlike HPT),
// enable the In-TLB MSHR on the shared L2 TLB, and price each walk above the
// baseline radix walk. The baseline platform must stay bit-identical.
package timingconfig

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm/gmmu"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
)

// l2TLBOf fetches the shared L2 TLB of the platform's single GPU.
func l2TLBOf(p *hptPlatform) *tlb.Comp {
	return p.simulation.GetComponentByName("GPU[1].L2TLB").(*tlb.Comp)
}

var _ = Describe("SoftWalker platform", func() {
	It("enables the software walk and the In-TLB MSHR", func() {
		platform := buildHPTPlatform("softwalker")

		Expect(platform.gmmu.SoftwareWalkEnabled()).To(BeTrue())
		// Unlike HPT, the software walk keeps the radix path and its
		// page-walk cache.
		Expect(platform.gmmu.HasPageWalkCache()).To(BeTrue())
		Expect(l2TLBOf(platform).InTLBMSHREnabled()).To(BeTrue())
	})

	It("leaves the baseline platform untouched", func() {
		platform := buildHPTPlatform("r9nano")

		Expect(platform.gmmu.SoftwareWalkEnabled()).To(BeFalse())
		Expect(l2TLBOf(platform).InTLBMSHREnabled()).To(BeFalse())
	})

	It("prices one software walk above the baseline radix walk", func() {
		sw := buildHPTPlatform("softwalker")
		swPage, swDuration := sw.walk(
			uint64(sw.gpuDriver.AllocateMemory(sw.ctx, 4096)))

		radix := buildHPTPlatform("r9nano")
		radixPage, radixDuration := radix.walk(
			uint64(radix.gpuDriver.AllocateMemory(radix.ctx, 4096)))

		// Both paths must resolve the same mapping...
		Expect(swPage.PAddr).To(Equal(radixPage.PAddr))
		// ...and one software walk must cost more than one hardware walk:
		// SoftWalker wins on concurrency, never on individual latency
		// (paper Figure 9).
		Expect(swDuration).To(BeNumerically(">", radixDuration))

		stats := sw.gmmu.SoftwareWalkStats()
		Expect(stats.WalkCount).To(Equal(uint64(1)))
		// sbin_claude: a full PWC-miss walk over the 4-level page table of
		// the target spec, with the default knobs: 2x10 comm + 20 setup +
		// 4x8 per-level cycles.
		Expect(stats.ExtraCyclesTotal).To(Equal(uint64(72)))
	})

	It("keeps the baseline GMMU free of software-walk statistics", func() {
		platform := buildHPTPlatform("r9nano")
		platform.walk(
			uint64(platform.gpuDriver.AllocateMemory(platform.ctx, 4096)))

		Expect(platform.gmmu.SoftwareWalkStats()).
			To(Equal(gmmu.SoftwareWalkStats{}))
	})
})
