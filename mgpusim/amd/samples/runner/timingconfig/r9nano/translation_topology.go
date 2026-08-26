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
	"github.com/sarchlab/mgpusim/v4/amd/timing/virtualcaching/fbt" // sbin_claude_fbt
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

// FBTSettings carries the Forward-Backward Table's geometry and timing.
// sbin_claude_fbt
type FBTSettings struct {
	NumEntries    int
	NumWays       int
	LookupLatency int
}

type (
	baselineTranslationTopology struct{}
	utopiaTranslationTopology   struct{ settings UtopiaSettings }
	fbtTranslationTopology      struct{ settings FBTSettings } // sbin_claude_fbt
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

// NewFBTTranslationTopology returns the wiring with the Forward-Backward
// Table between the shared L2 TLB and the GMMU. An L2 TLB miss consults the
// FBT before a page walk may start. This is the paper's "VC With OPT"
// configuration: virtual caching filters the bandwidth reaching the shared
// TLB, which exposes the page-walk overhead behind it, and the FBT - which
// has to exist for correctness anyway - recovers that as a second-level TLB.
// sbin_claude_fbt
func NewFBTTranslationTopology(settings FBTSettings) TranslationTopology {
	return fbtTranslationTopology{settings: settings}
}

func (baselineTranslationTopology) buildWalkers(*Builder) {}

func (t fbtTranslationTopology) buildWalkers(b *Builder) { // sbin_claude_fbt
	b.fbt = fbt.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithLog2PageSize(b.log2PageSize).
		WithNumEntries(t.settings.NumEntries).
		WithNumWays(t.settings.NumWays).
		WithLookupLatency(t.settings.LookupLatency).
		Build(b.name + ".FBT")

	b.simulation.RegisterComponent(b.fbt)
}

// l2TLBTranslationProvider points L2 TLB misses at the FBT, not the GMMU.
// sbin_claude_fbt
func (fbtTranslationTopology) l2TLBTranslationProvider(
	b *Builder,
) sim.RemotePort {
	return b.fbt.GetPortByName("Top").AsRemote()
}

// connectWalkers wires L2TLB.Bottom -> FBT.Top and FBT.Bottom -> GMMU.Top, so
// a page walk can only start once the FBT has reported a miss.
// sbin_claude_fbt
func (fbtTranslationTopology) connectWalkers(b *Builder) {
	l2ToFBT := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".L2TLBToFBT")
	b.simulation.RegisterComponent(l2ToFBT)

	l2ToFBT.PlugIn(b.fbt.GetPortByName("Top"))

	for _, l2TLB := range b.l2TLBs {
		l2ToFBT.PlugIn(l2TLB.GetPortByName("Bottom"))
	}

	fbtToGMMU := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".FBTToGMMU")
	b.simulation.RegisterComponent(fbtToGMMU)

	fbtToGMMU.PlugIn(b.fbt.GetPortByName("Bottom"))
	fbtToGMMU.PlugIn(b.gmmu.GetPortByName("Top"))

	b.fbt.SetPageWalker(b.gmmu.GetPortByName("Top").AsRemote())
}

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
