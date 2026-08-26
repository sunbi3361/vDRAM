// sbin_claude_avatar
// Package avatar provides a GPU configuration identical to the r9nano
// baseline except that an Avatar Speculation Unit (ASU) is interposed
// between the L1 TLB bottom ports and the shared L2 TLB. Every L1 TLB miss
// still walks the conventional L2-TLB/GMMU path; a confident MOD prediction
// additionally launches a speculative access whose CAVA rapid validation
// can answer the L1 TLB early (Early TLB Fill). See refs/avatar.md and
// avatar-plan.md. The driver owns the authoritative frame metadata and the
// 2MB-region randomized placement through the shared meta.Registry.
package avatar

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
)

// Builder embeds the r9nano builder and swaps in the Avatar speculation
// topology. The GPUBuilder-interface surface (WithGPUID, WithMemAddrOffset,
// WithRDMAAddressMapper, WithPageTable, Build, ...) is promoted from the
// embedded r9nano.Builder, following the utopia wrapper pattern.
type Builder struct {
	r9nano.Builder
}

// MakeBuilder creates a new Builder. The Avatar speculation topology needs
// the shared metadata registry, so it is injected later via
// WithAvatarSettings.
func MakeBuilder() Builder {
	return Builder{r9nano.MakeBuilder()}
}

var _ gpubuilder.GPUBuilder = MakeBuilder() // wrapper interface assertion.

// WithAvatarSettings injects the shared metadata registry and the ASU
// timing knobs, activating the Avatar speculation topology.
func (b Builder) WithAvatarSettings(settings r9nano.AvatarSettings) Builder {
	b.Builder = b.Builder.WithSpeculationTopology(
		r9nano.NewAvatarSpeculationTopology(settings))
	return b
}

// The chain setters below MUST return Builder (the wrapper type) so the
// fluent chain keeps the wrapper alive; they shadow the promoted r9nano
// methods. The rest of the builder surface is promoted via embedding.

// WithSimulation sets the simulation to use.
func (b Builder) WithSimulation(s *simulation.Simulation) Builder {
	b.Builder = b.Builder.WithSimulation(s)
	return b
}

// WithMMU sets the CPU-side MMU.
func (b Builder) WithMMU(m *mmu.Comp) Builder {
	b.Builder = b.Builder.WithMMU(m)
	return b
}

// WithLog2PageSize sets the log2 page size.
func (b Builder) WithLog2PageSize(p uint64) Builder {
	b.Builder = b.Builder.WithLog2PageSize(p)
	return b
}

// WithGlobalStorage sets the global storage.
func (b Builder) WithGlobalStorage(g *mem.Storage) Builder {
	b.Builder = b.Builder.WithGlobalStorage(g)
	return b
}
