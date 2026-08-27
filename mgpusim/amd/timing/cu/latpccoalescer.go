package cu

// latpcCoalescer implements the LATPC Regularity Detector (MICRO'25 §5.2,
// refs/latpc-plan.md 1.1, 2.3). The paper's RD is a streaming FSM in the SM
// that consumes the coalescer's unique VPNs one per cycle - in thread-index
// order - and classifies each as a demand <VPN, 0, 0> or a prefetch
// <VPN, Stride, Index> triple. The coalescer here already has the whole warp
// instruction's addresses in lane order, so the RD is a pure function over
// that sequence producing exactly the triples the hardware FSM would; the
// triple rides each memory request as a TranslationHint. The RD's 1-cycle
// hardware latency is not charged (plan 1.7).
// sbin_claude_latpc

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/timing/wavefront"
)

const (
	// latpcRegionShift is the paper's 9-bit stride width: all VPNs of one
	// group lie in the same 512-page region, so their L4 PTEs share one 4KB
	// page-table page (= one DRAM row). // sbin_claude_latpc
	latpcRegionShift = 9
	// latpcMaxGroupSize is the subentry count of one compressed MSHR entry
	// (32-bit valid mask, 5-bit index). // sbin_claude_latpc
	latpcMaxGroupSize = 32
)

// latpcCoalescer wraps the instruction coalescer and stamps every generated
// memory request with its Regularity Detector triple. State never persists
// across warp instructions: each generateMemTransactions call is one warp
// instruction, matching the paper's per-instruction Reset. // sbin_claude_latpc
type latpcCoalescer struct {
	inner        coalescer
	log2PageSize uint64

	// sbin_claude_latpc: the paper's whole premise (Fig. 8, Fig. 9) is that
	// one warp instruction carries SEVERAL unique VPNs. On a 64-lane GCN3
	// wavefront that is a property of the workload, not a given, so it has
	// to be measurable instead of assumed.
	stats RDStats
}

// RDStats counts what the Regularity Detector produced over a run. One
// demand starts one group, so Demands = UniqueVPNs - PrefetchVPNs;
// UniqueVPNs/Instructions is the paper's Figure-8 quantity, and
// PrefetchVPNs/UniqueVPNs is the fraction of translations LATC and LATP can
// compress at all. // sbin_claude_latpc
type RDStats struct {
	Instructions  uint64
	MultiVPNInsts uint64
	UniqueVPNs    uint64
	PrefetchVPNs  uint64
}

// RDStats reports this CU's Regularity Detector counters. The second result
// is false when the CU does not run the LATPC coalescer. // sbin_claude_latpc
func (cu *ComputeUnit) RDStats() (RDStats, bool) {
	vmu, ok := cu.VectorMemUnit.(*VectorMemoryUnit)
	if !ok {
		return RDStats{}, false
	}

	rd, ok := vmu.coalescer.(*latpcCoalescer)
	if !ok {
		return RDStats{}, false
	}

	return rd.stats, true
}

func (c *latpcCoalescer) generateMemTransactions(
	wf *wavefront.Wavefront,
) []VectorMemAccessInfo {
	transactions := c.inner.generateMemTransactions(wf)
	c.annotate(transactions)

	return transactions
}

// annotate derives the instruction's unique-VPN sequence in first-appearance
// (= thread-index) order, runs the Regularity Detector over it, and stamps
// each transaction with the triple of its VPN. // sbin_claude_latpc
func (c *latpcCoalescer) annotate(transactions []VectorMemAccessInfo) {
	hintByVPN := make(map[uint64]*vm.TranslationGroupHint)
	uniqueVPNs := make([]uint64, 0, len(transactions))

	for i := range transactions {
		vpn := c.vpnOf(&transactions[i])
		if _, seen := hintByVPN[vpn]; !seen {
			hintByVPN[vpn] = nil
			uniqueVPNs = append(uniqueVPNs, vpn)
		}
	}

	hints := runRegularityDetector(uniqueVPNs)
	for k, vpn := range uniqueVPNs {
		hintByVPN[vpn] = hints[k]
	}

	c.recordStats(uniqueVPNs, hints) // sbin_claude_latpc

	for i := range transactions {
		c.stamp(&transactions[i], hintByVPN[c.vpnOf(&transactions[i])])
	}
}

// recordStats accumulates the Regularity Detector's output for one warp
// instruction. Only this CU's goroutine ever touches these counters, so they
// need no synchronization even under -parallel. // sbin_claude_latpc
func (c *latpcCoalescer) recordStats(
	uniqueVPNs []uint64,
	hints []*vm.TranslationGroupHint,
) {
	if len(uniqueVPNs) == 0 {
		return
	}

	c.stats.Instructions++
	c.stats.UniqueVPNs += uint64(len(uniqueVPNs))

	if len(uniqueVPNs) > 1 {
		c.stats.MultiVPNInsts++
	}

	for _, hint := range hints {
		if hint.StridePages != 0 {
			c.stats.PrefetchVPNs++
		}
	}
}

func (c *latpcCoalescer) vpnOf(t *VectorMemAccessInfo) uint64 {
	if t.Read != nil {
		return t.Read.Address >> c.log2PageSize
	}

	return t.Write.Address >> c.log2PageSize
}

func (c *latpcCoalescer) stamp(
	t *VectorMemAccessInfo,
	hint *vm.TranslationGroupHint,
) {
	if t.Read != nil {
		t.Read.TranslationHint = hint
		return
	}

	t.Write.TranslationHint = hint
}

// runRegularityDetector is the paper's Figure-12 state machine over one warp
// instruction's unique VPNs. A VPN starts a new group (a demand) when it is
// the first input, leaves the group base's 512-page region, breaks the
// running stride, or would overflow the 32-subentry index; otherwise it
// extends the group as a prefetch. The 9-bit hardware subtractor's
// wraparound ambiguity does not arise here: the region check bounds every
// accepted stride to the same 512-page window the hardware resolves against.
// sbin_claude_latpc
func runRegularityDetector(uniqueVPNs []uint64) []*vm.TranslationGroupHint {
	hints := make([]*vm.TranslationGroupHint, len(uniqueVPNs))

	var (
		valid   bool
		baseVPN uint64
		prevVPN uint64
		stride  int64
		index   int
		groupID string
	)

	newGroup := func(vpn uint64) *vm.TranslationGroupHint {
		baseVPN = vpn
		stride = 0
		index = 0
		groupID = sim.GetIDGenerator().Generate()

		return &vm.TranslationGroupHint{GroupID: groupID}
	}

	for k, vpn := range uniqueVPNs {
		switch {
		case !valid:
			hints[k] = newGroup(vpn)
			valid = true
		case vpn>>latpcRegionShift != baseVPN>>latpcRegionShift ||
			index+1 >= latpcMaxGroupSize:
			hints[k] = newGroup(vpn)
		case stride == 0:
			// The previous VPN was a demand: any in-region stride starts a
			// run (the paper's Prev.Stride = 0 exception).
			stride = int64(vpn) - int64(prevVPN)
			index = 1
			hints[k] = &vm.TranslationGroupHint{
				GroupID:     groupID,
				StridePages: stride,
				Index:       index,
			}
		case int64(vpn)-int64(prevVPN) == stride:
			index++
			hints[k] = &vm.TranslationGroupHint{
				GroupID:     groupID,
				StridePages: stride,
				Index:       index,
			}
		default:
			hints[k] = newGroup(vpn)
		}

		prevVPN = vpn
	}

	return hints
}
