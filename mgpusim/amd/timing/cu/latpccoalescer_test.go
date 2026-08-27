package cu

// sbin_claude_latpc: Regularity Detector unit tests, including the paper's
// Figure-12b worked example (refs/latpc-plan.md 3).

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

func rdTriple(h *vm.TranslationGroupHint) (int64, int) {
	return h.StridePages, h.Index
}

var _ = Describe("LATPC Regularity Detector", func() {
	It("should reproduce the paper's Figure-12b classification", func() {
		vpns := []uint64{
			0x1000, 0x1004, 0x1008, 0x100c, 0x1011, 0x1028, 0x1025, 0x1022,
		}

		hints := runRegularityDetector(vpns)

		strides := make([]int64, len(hints))
		indices := make([]int, len(hints))
		for i, h := range hints {
			strides[i], indices[i] = rdTriple(h)
		}

		Expect(strides).To(Equal(
			[]int64{0, 4, 4, 4, 0, 23, 0, -3}))
		Expect(indices).To(Equal(
			[]int{0, 1, 2, 3, 0, 1, 0, 1}))

		// One group per demand; prefetches share their demand's group.
		Expect(hints[1].GroupID).To(Equal(hints[0].GroupID))
		Expect(hints[2].GroupID).To(Equal(hints[0].GroupID))
		Expect(hints[3].GroupID).To(Equal(hints[0].GroupID))
		Expect(hints[5].GroupID).To(Equal(hints[4].GroupID))
		Expect(hints[7].GroupID).To(Equal(hints[6].GroupID))
		Expect(hints[4].GroupID).NotTo(Equal(hints[0].GroupID))
		Expect(hints[6].GroupID).NotTo(Equal(hints[4].GroupID))
	})

	It("should start a new group when the 512-page region breaks", func() {
		// 0x1000 and 0x11ff share region 8; 0x1200 is region 9.
		hints := runRegularityDetector([]uint64{0x1000, 0x11ff, 0x1200})

		s0, i0 := rdTriple(hints[0])
		s1, i1 := rdTriple(hints[1])
		s2, i2 := rdTriple(hints[2])

		Expect([]interface{}{s0, i0}).To(Equal([]interface{}{int64(0), 0}))
		Expect([]interface{}{s1, i1}).To(Equal([]interface{}{int64(0x1ff), 1}))
		Expect([]interface{}{s2, i2}).To(Equal([]interface{}{int64(0), 0}))
		Expect(hints[2].GroupID).NotTo(Equal(hints[0].GroupID))
	})

	It("should cap a group at 32 subentries", func() {
		vpns := make([]uint64, 34)
		for i := range vpns {
			vpns[i] = 0x2000 + uint64(i)
		}

		hints := runRegularityDetector(vpns)

		_, last := rdTriple(hints[31])
		Expect(last).To(Equal(31))

		s32, i32 := rdTriple(hints[32])
		Expect([]interface{}{s32, i32}).To(Equal([]interface{}{int64(0), 0}))
		Expect(hints[32].GroupID).NotTo(Equal(hints[0].GroupID))

		s33, i33 := rdTriple(hints[33])
		Expect([]interface{}{s33, i33}).To(Equal([]interface{}{int64(1), 1}))
	})

	It("should count what the detector produced, per instruction", func() {
		c := &latpcCoalescer{log2PageSize: 12}

		read := func(addr uint64) VectorMemAccessInfo {
			return VectorMemAccessInfo{
				Read: mem.ReadReqBuilder{}.
					WithAddress(addr).
					WithByteSize(64).
					Build(),
			}
		}

		// A fully coalesced instruction: four cache lines, one page. This
		// is the streaming-kernel case, and it must yield exactly one
		// unique VPN and no prefetch - LATC and LATP have nothing to
		// compress here.
		c.annotate([]VectorMemAccessInfo{
			read(0x1000_000), read(0x1000_040),
			read(0x1000_080), read(0x1000_0c0),
		})

		Expect(c.stats).To(Equal(RDStats{
			Instructions:  1,
			MultiVPNInsts: 0,
			UniqueVPNs:    1,
			PrefetchVPNs:  0,
		}))

		// A page-spanning instruction: three unique VPNs at stride 1, so
		// the second and third are prefetches of the first's group.
		c.annotate([]VectorMemAccessInfo{
			read(0x1000_000), read(0x1001_000), read(0x1002_000),
		})

		Expect(c.stats).To(Equal(RDStats{
			Instructions:  2,
			MultiVPNInsts: 1,
			UniqueVPNs:    4,
			PrefetchVPNs:  2,
		}))

		// An instruction the coalescer emptied leaves the counters alone.
		c.annotate(nil)

		Expect(c.stats.Instructions).To(Equal(uint64(2)))
	})

	It("should stamp every transaction of a page with the page's triple",
		func() {
			c := &latpcCoalescer{log2PageSize: 12}

			read := func(addr uint64) VectorMemAccessInfo {
				return VectorMemAccessInfo{
					Read: mem.ReadReqBuilder{}.
						WithAddress(addr).
						WithByteSize(64).
						Build(),
				}
			}

			// Two cache lines in page 0x1000, one in 0x1001, one more back
			// in page 0x1000: four transactions, two unique VPNs.
			transactions := []VectorMemAccessInfo{
				read(0x1000_000),
				read(0x1000_040),
				read(0x1001_000),
				read(0x1000_080),
			}

			c.annotate(transactions)

			h0 := transactions[0].Read.TranslationHint
			h1 := transactions[1].Read.TranslationHint
			h2 := transactions[2].Read.TranslationHint
			h3 := transactions[3].Read.TranslationHint

			Expect(h0).NotTo(BeNil())
			Expect(h1).To(BeIdenticalTo(h0))
			Expect(h3).To(BeIdenticalTo(h0))

			Expect(h0.StridePages).To(Equal(int64(0)))
			Expect(h0.Index).To(Equal(0))
			Expect(h2.GroupID).To(Equal(h0.GroupID))
			Expect(h2.StridePages).To(Equal(int64(1)))
			Expect(h2.Index).To(Equal(1))
		})
})
