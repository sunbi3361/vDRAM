// sbin_claude_avatar
// SpeculationTopology is the strategy hook that decides what sits between
// the L1 TLB bottom ports and the shared L2 TLB, following the
// DataPathTopology/MemoryTopology/TranslationTopology pattern. The baseline
// routes L1 TLB misses straight to the L2 TLB. The Avatar topology
// interposes the ASU (Avatar Speculation Unit): every L1 TLB miss is
// forwarded to the L2 TLB unchanged while a confident MOD prediction
// launches a speculative access with CAVA rapid validation in parallel
// (refs/avatar.md 5.3, avatar-plan.md 1.1).

package r9nano

import (
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/asu"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// SpeculationTopology selects what serves the L1 TLB bottom ports.
type SpeculationTopology interface {
	// buildSpeculationUnit constructs extra components. It runs right
	// after buildL2TLB, so it may retarget the l1TLBAddressMapper.
	buildSpeculationUnit(b *Builder)
	// l1TranslationProviderPort returns the port the L1TLB-to-L2TLB
	// connection plugs in as the provider side.
	l1TranslationProviderPort(b *Builder) sim.Port
	// connectSpeculation wires the interposer bottom to the L2 TLB top.
	// The baseline needs no extra wiring.
	connectSpeculation(b *Builder)
}

// AvatarSettings carries the Avatar timing knobs and the shared
// authoritative metadata registry into the ASU.
type AvatarSettings struct {
	Registry            *meta.Registry
	ValidationLatency   int
	ModNumEntries       int
	ConfidenceThreshold int
}

type (
	baselineSpeculationTopology struct{}
	avatarSpeculationTopology   struct{ settings AvatarSettings }
)

// NewBaselineSpeculationTopology returns the direct L1TLB-to-L2TLB wiring.
func NewBaselineSpeculationTopology() SpeculationTopology {
	return baselineSpeculationTopology{}
}

// NewAvatarSpeculationTopology returns the wiring with the ASU between the
// L1 TLB bottoms and the shared L2 TLB.
func NewAvatarSpeculationTopology(settings AvatarSettings) SpeculationTopology {
	if settings.Registry == nil {
		panic("avatar speculation topology requires a metadata registry")
	}
	return avatarSpeculationTopology{settings: settings}
}

func (baselineSpeculationTopology) buildSpeculationUnit(*Builder) {}

func (baselineSpeculationTopology) l1TranslationProviderPort(
	b *Builder,
) sim.Port {
	return b.l2TLBs[0].GetPortByName("Top")
}

func (baselineSpeculationTopology) connectSpeculation(*Builder) {}

func (t avatarSpeculationTopology) buildSpeculationUnit(b *Builder) {
	s := t.settings

	b.asu = asu.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		WithDeviceID(b.gpuID).
		WithLog2PageSize(b.log2PageSize).
		WithRegistry(s.Registry).
		WithPageTable(b.pageTable).
		WithValidationLatency(s.ValidationLatency).
		WithModNumEntries(s.ModNumEntries).
		WithConfidenceThreshold(s.ConfidenceThreshold).
		// Pre-edit v2 code (commented per project convention): the ASU was
		// handed the L1 caches' L2-bank mapper to route its own fetches.
		//
		// WithMemoryAccess(
		// 	b.l1AddressMapper,
		// 	b.memAddrOffset,
		// 	b.memAddrOffset+b.dramSize).
		//
		// sbin_claude_avatar v3: only the DRAM bounds are needed now.
		WithMemoryRange(
			b.memAddrOffset,
			b.memAddrOffset+b.dramSize).
		Build(b.name + ".ASU")

	b.simulation.RegisterComponent(b.asu)

	// The L1 TLBs now miss into the ASU; the ASU forwards to the L2 TLB.
	b.asu.SetL2TLBPort(b.l2TLBs[0].GetPortByName("Top").AsRemote())
	// sbin_claude_avatar v2: EAF cancels flow ASU -> L2TLB.Cancel, and a
	// fully-canceled MSHR entry forwards the cancel to GMMU.Cancel
	// (avatar-plan.md 5.2).
	b.asu.SetL2TLBCancelPort(b.l2TLBs[0].GetPortByName("Cancel").AsRemote())
	b.l2TLBs[0].SetWalkCancelProvider(
		b.gmmu.GetPortByName("Cancel").AsRemote())
	b.l1TLBAddressMapper.Port = b.asu.TopPort().AsRemote()
}

func (avatarSpeculationTopology) l1TranslationProviderPort(
	b *Builder,
) sim.Port {
	return b.asu.TopPort()
}

func (avatarSpeculationTopology) connectSpeculation(b *Builder) {
	conn := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".ASUToL2TLB")
	b.simulation.RegisterComponent(conn)

	conn.PlugIn(b.asu.BottomPort())
	conn.PlugIn(b.l2TLBs[0].GetPortByName("Top"))
	// sbin_claude_avatar v2: the ASU bottom port also carries the EAF
	// cancels into the L2 TLB's out-of-band Cancel ingress.
	conn.PlugIn(b.l2TLBs[0].GetPortByName("Cancel"))

	// Pre-edit v2 code (commented per project convention): the ASU's own
	// sector fetches were plugged into the L1ToL2 data network.
	//
	// l1ToL2 := b.simulation.GetComponentByName(
	// 	b.name + ".L1ToL2").(*directconnection.Comp)
	// l1ToL2.PlugIn(b.asu.ValidationPort())
	//
	// sbin_claude_avatar v3: CAST's speculative access is the requester's
	// own data access (refs 5.3, 5.6), which already rides this network as
	// the demand request. The ASU adds no traffic of its own.
}
