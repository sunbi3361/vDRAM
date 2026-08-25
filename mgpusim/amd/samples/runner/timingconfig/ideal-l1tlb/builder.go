// Package ideall1tlb provides a GPU configuration identical to the r9nano
// baseline except that all L1 TLBs are replaced with ideal TLBs that resolve
// every translation directly from the GPU page table with zero latency. // sbin_codex
package ideall1tlb

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/idealtlb"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
)

// Builder embeds the r9nano builder and overrides the L1-TLB factory at
// construction time. The GPUBuilder-interface surface (WithGPUID,
// WithMemAddrOffset, WithRDMAAddressMapper, WithPageTable, Build) is promoted
// from the embedded r9nano.Builder instead of being redelegated. // sbin_codex
type Builder struct {
	// Pre-edit code (commented per AGENTS.md convention):
	// inner r9nano.Builder
	r9nano.Builder // sbin_codex: embedding replaces the delegation field.
}

// MakeBuilder creates a new Builder with the ideal-L1-TLB factory injected. // sbin_codex
func MakeBuilder() Builder {
	// Pre-edit code (commented per AGENTS.md convention):
	// return Builder{inner: r9nano.MakeBuilder().
	// 	WithL1TLBFactory(newIdealTLB)}
	return Builder{r9nano.MakeBuilder(). // sbin_codex
						WithL1TLBFactory(newIdealTLB)}
}

// newIdealTLB builds an ideal TLB component for the given name. // sbin_codex
func newIdealTLB(
	name string,
	engine sim.Engine,
	freq sim.Freq,
	pageTable vm.PageTable,
	mapper mem.AddressToPortMapper,
	numReqPerCycle int,
) sim.Component {
	return idealtlb.MakeBuilder().
		WithEngine(engine).
		WithFreq(freq).
		WithNumReqPerCycle(numReqPerCycle).
		WithPageTable(pageTable).
		Build(name)
}

// The four chain setters MUST return Builder (the wrapper type) so the fluent
// chain keeps the wrapper alive. They shadow the promoted r9nano methods; the
// rest of the builder surface is promoted via embedding. // sbin_codex
func (b Builder) WithSimulation(s *simulation.Simulation) Builder {
	// Pre-edit code (commented per AGENTS.md convention):
	// b.inner = b.inner.WithSimulation(s)
	b.Builder = b.Builder.WithSimulation(s) // sbin_codex
	return b
}

func (b Builder) WithMMU(m *mmu.Comp) Builder {
	// Pre-edit code (commented per AGENTS.md convention):
	// b.inner = b.inner.WithMMU(m)
	b.Builder = b.Builder.WithMMU(m) // sbin_codex
	return b
}

func (b Builder) WithLog2PageSize(p uint64) Builder {
	// Pre-edit code (commented per AGENTS.md convention):
	// b.inner = b.inner.WithLog2PageSize(p)
	b.Builder = b.Builder.WithLog2PageSize(p) // sbin_codex
	return b
}

func (b Builder) WithGlobalStorage(g *mem.Storage) Builder {
	// Pre-edit code (commented per AGENTS.md convention):
	// b.inner = b.inner.WithGlobalStorage(g)
	b.Builder = b.Builder.WithGlobalStorage(g) // sbin_codex
	return b
}

// Pre-edit delegation methods (commented per AGENTS.md convention). These
// GPUBuilder-interface methods were previously redelegated to b.inner; with
// embedding they are promoted from the embedded r9nano.Builder:
//
// func (b Builder) WithGPUID(id uint64) gpubuilder.GPUBuilder {
// 	return b.inner.WithGPUID(id)
// }
// func (b Builder) WithMemAddrOffset(offset uint64) gpubuilder.GPUBuilder {
// 	return b.inner.WithMemAddrOffset(offset)
// }
// func (b Builder) WithRDMAAddressMapper(mapper mem.AddressToPortMapper) gpubuilder.GPUBuilder {
// 	return b.inner.WithRDMAAddressMapper(mapper)
// }
// func (b Builder) WithPageTable(pageTable vm.PageTable) gpubuilder.GPUBuilder {
// 	return b.inner.WithPageTable(pageTable)
// }
// func (b Builder) Build(name string) *sim.Domain {
// 	return b.inner.Build(name)
// }
