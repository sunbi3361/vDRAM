// sbin_claude_utopia
// Package utopia provides a GPU configuration identical to the r9nano
// baseline except that the translation path below the shared L2 TLB uses the
// Utopia hybrid RestSeg/FlexSeg scheme: an L2 TLB miss first performs a
// RestSeg Walk in the UTU (SF filter + TAR tag match with small timed
// metadata caches); only a NotInRestSeg result starts the conventional
// FlexSeg page walk in the GMMU (utopia.md 4.7). The driver reserves the
// RestSeg physical region and owns the authoritative TAR/SF state through
// the shared restseg.Registry.
package utopia

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
)

// Builder embeds the r9nano builder and swaps in the Utopia translation
// topology. The GPUBuilder-interface surface (WithGPUID, WithMemAddrOffset,
// WithRDMAAddressMapper, WithPageTable, Build, ...) is promoted from the
// embedded r9nano.Builder, following the virtual-caching wrapper pattern.
type Builder struct {
	r9nano.Builder
}

// MakeBuilder creates a new Builder. The Utopia translation topology needs
// the shared RestSeg registry, so it is injected later via
// WithUtopiaSettings.
func MakeBuilder() Builder {
	return Builder{r9nano.MakeBuilder()}
}

var _ gpubuilder.GPUBuilder = MakeBuilder() // wrapper interface assertion.

// WithUtopiaSettings injects the shared RestSeg registry and the UTU timing
// knobs, activating the Utopia translation topology.
func (b Builder) WithUtopiaSettings(settings r9nano.UtopiaSettings) Builder {
	b.Builder = b.Builder.WithTranslationTopology(
		r9nano.NewUtopiaTranslationTopology(settings))
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
