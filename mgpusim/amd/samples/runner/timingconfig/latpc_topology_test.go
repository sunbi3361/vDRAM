// sbin_claude_latpc: -gpu=latpc must reach all three mechanisms - the LATC
// compressed MSHR in every L1V TLB and LATP batching in the GMMU are checked
// here; the Regularity Detector is a CU-internal coalescer wrapper covered
// by the cu package tests. The baseline must stay untouched.
package timingconfig

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
)

var _ = Describe("LATPC topology", func() {
	It("enables LATC and LATP everywhere on -gpu=latpc", func() {
		platform := buildHPTPlatform("latpc")

		Expect(platform.gmmu.LATPBatchingEnabled()).To(BeTrue())
		Expect(platform.gmmu.HashedPageTableEnabled()).To(BeFalse())

		// A missing name makes GetComponentByName fall back to component
		// index 0, so every hit is verified by name.
		checked := 0
		for sa := 0; ; sa++ {
			name := fmt.Sprintf("GPU[1].SA[%d].L1VTLB[0]", sa)
			c := platform.simulation.GetComponentByName(name)
			if c == nil || c.Name() != name {
				break
			}

			Expect(c.(*tlb.Comp).CompressedMSHREnabled()).To(BeTrue())
			checked++
		}
		Expect(checked).To(BeNumerically(">", 0))
	})

	It("leaves the baseline classic", func() {
		platform := buildHPTPlatform("r9nano")

		Expect(platform.gmmu.LATPBatchingEnabled()).To(BeFalse())

		l1vTLB := platform.simulation.GetComponentByName(
			"GPU[1].SA[0].L1VTLB[0]").(*tlb.Comp)
		Expect(l1vTLB.CompressedMSHREnabled()).To(BeFalse())
	})

	It("still walks correctly with LATPC enabled", func() {
		platform := buildHPTPlatform("latpc")
		vAddr := uint64(platform.gpuDriver.AllocateMemory(platform.ctx, 4096))

		page, _ := platform.walk(vAddr)

		Expect(page.Valid).To(BeTrue())
		Expect(page.VAddr).To(Equal(vAddr))
	})
})
