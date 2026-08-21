// sbin_codex
// Package virtualcaching provides a GPU configuration identical to the r9nano
// baseline except that the L1 vector, L1 scalar, and shared L2 data caches use
// virtual addresses for lookup and tags. The L1 instruction ROB/cache/
// address-translator/TLB topology and routing remain the baseline.
//
// This is a simplified virtual-address model. DRAM-bound L2 refills and dirty
// writebacks translate through a per-slice address translator, the shared L2TLB,
// and the GMMU before reaching DRAM.
// sbin_codex
//
// Previous package documentation preserved below (commented per project convention):
// // Package virtualcaching provides a GPU configuration identical to the r9nano
// // baseline except that L1 and L2 data cahes are tagged and indexed by virtual address.
// // Only L2D miss go through translation latency (L2TLB->GMMU).
// // For simple implementation, we emulate VirtualCaching (ASPLOS'18) features:
// //  1. L1 and L2 data caches lookup GPU page table and access cache without translation
// //  2. Every L2D misses are forwared to L2 TLB.
// //  3. After finishing translation, L2D is filled up by accessing DRAM.
package virtualcaching

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
)

// Builder embeds the r9nano builder and swaps in virtually-indexed
// virtually-tagged caches. The GPUBuilder-interface surface (WithGPUID,
// WithMemAddrOffset, WithRDMAAddressMapper, WithPageTable, Build) is promoted
// from the embedded r9nano.Builder instead of being redelegated. // sbin_codex
type Builder struct {
	// Pre-edit code (commented per AGENTS.md convention):
	// inner r9nano.Builder
	r9nano.Builder // sbin_codex: embedding replaces the delegation field.
}

var _ gpubuilder.GPUBuilder = MakeBuilder() // sbin_codex: wrapper interface assertion.

// MakeBuilder creates a new Builder with r9nano virtual caching enabled. // sbin_codex
func MakeBuilder() Builder {
	// Pre-edit code (commented per AGENTS.md convention):
	// return Builder{inner: r9nano.MakeBuilder().
	// 	WithDataPathTopology(r9nano.NewVirtualDataPathTopology()).
	// 	WithMemoryTopology(r9nano.NewVirtualMemoryTopology())}
	return Builder{r9nano.MakeBuilder(). // sbin_codex
		WithDataPathTopology(r9nano.NewVirtualDataPathTopology()).
		WithMemoryTopology(r9nano.NewVirtualMemoryTopology())}
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