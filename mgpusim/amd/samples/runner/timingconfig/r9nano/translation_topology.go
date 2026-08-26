// sbin_claude_utopia
// TranslationTopology is the strategy hook that decides what sits between the
// shared L2 TLB and the GMMU, following the DataPathTopology/MemoryTopology
// pattern. The baseline routes L2 TLB misses straight to the GMMU page
// walker. The Utopia topology interposes the UTU (RestSeg walker): an L2 TLB
// miss first performs the RestSeg walk and only a NotInRestSeg result starts
// the conventional FlexSeg walk in the GMMU (utopia.md 4.7).
package r9nano

import (
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/rsw"
)

// TranslationTopology selects the walker chain below the shared L2 TLB.
type TranslationTopology interface {
	// buildWalkers constructs extra walker components. It runs after
	// buildGMMU and before buildL2TLB.
	buildWalkers(b *Builder)
	// l2TLBTranslationProvider returns the port that serves L2 TLB misses.
	l2TLBTranslationProvider(b *Builder) sim.RemotePort
	// connectWalkers wires the L2 TLB bottom to the walker chain. It replaces
	// the baseline connectL2TLBToGMMU step.
	connectWalkers(b *Builder)
}

// UtopiaSettings carries the Utopia GPU-side timing knobs and the shared
// authoritative RestSeg state into the UTU.
type UtopiaSettings struct {
	Registry      *restseg.Registry
	TARCacheBytes uint64
	SFCacheBytes  uint64
	HitLatency    int
	MissLatency   int
}

type (
	baselineTranslationTopology struct{}
	utopiaTranslationTopology   struct{ settings UtopiaSettings }
)

// NewBaselineTranslationTopology returns the baseline L2TLB-to-GMMU wiring.
func NewBaselineTranslationTopology() TranslationTopology {
	return baselineTranslationTopology{}
}

// NewUtopiaTranslationTopology returns the wiring with the UTU (RestSeg
// walker) between the L2 TLB and the GMMU.
func NewUtopiaTranslationTopology(settings UtopiaSettings) TranslationTopology {
	if settings.Registry == nil {
		panic("utopia translation topology requires a RestSeg registry")
	}
	return utopiaTranslationTopology{settings: settings}
}

func (baselineTranslationTopology) buildWalkers(*Builder) {}

func (baselineTranslationTopology) l2TLBTranslationProvider(
	b *Builder,
) sim.RemotePort {
	return b.gmmu.GetPortByName("Top").AsRemote()
}

func (baselineTranslationTopology) connectWalkers(b *Builder) {
	b.connectL2TLBToGMMU()
}

func (t utopiaTranslationTopology) buildWalkers(b *Builder) {
	s := t.settings

	b.utu = rsw.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithDeviceID(b.gpuID).
		WithRegistry(s.Registry).
		WithTARCacheBytes(s.TARCacheBytes).
		WithSFCacheBytes(s.SFCacheBytes).
		WithHitLatency(s.HitLatency).
		WithMissLatency(s.MissLatency).
		Build(b.name + ".UTU")

	b.simulation.RegisterComponent(b.utu)
}

func (utopiaTranslationTopology) l2TLBTranslationProvider(
	b *Builder,
) sim.RemotePort {
	return b.utu.GetPortByName("Top").AsRemote()
}

// connectWalkers wires L2TLB.Bottom -> UTU.Top and UTU.Bottom -> GMMU.Top so
// the FlexSeg walk can only start after the RestSeg walk reported
// NotInRestSeg (utopia.md 4.7).
func (utopiaTranslationTopology) connectWalkers(b *Builder) {
	l2ToUTU := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".L2TLBToUTU")
	b.simulation.RegisterComponent(l2ToUTU)

	l2ToUTU.PlugIn(b.utu.GetPortByName("Top"))
	for _, l2TLB := range b.l2TLBs {
		l2ToUTU.PlugIn(l2TLB.GetPortByName("Bottom"))
	}

	utuToGMMU := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".UTUToGMMU")
	b.simulation.RegisterComponent(utuToGMMU)

	utuToGMMU.PlugIn(b.utu.GetPortByName("Bottom"))
	utuToGMMU.PlugIn(b.gmmu.GetPortByName("Top"))

	b.utu.SetFlexSegWalker(b.gmmu.GetPortByName("Top").AsRemote())
}
