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
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
)

// Builder wraps the r9nano builder and swaps in ideal L1 TLBs. // sbin_codex
type Builder struct {
	inner r9nano.Builder
}

// MakeBuilder creates a new Builder with the ideal-L1-TLB factory injected. // sbin_codex
func MakeBuilder() Builder {
	return Builder{inner: r9nano.MakeBuilder().WithL1TLBFactory(newIdealTLB)}
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

// The four concrete-chain setters MUST return Builder (the wrapper type) so
// the fluent chain keeps the wrapper alive. // sbin_codex
func (b Builder) WithSimulation(s *simulation.Simulation) Builder {
	b.inner = b.inner.WithSimulation(s)
	return b
}
func (b Builder) WithMMU(m *mmu.Comp) Builder {
	b.inner = b.inner.WithMMU(m)
	return b
}
func (b Builder) WithLog2PageSize(p uint64) Builder {
	b.inner = b.inner.WithLog2PageSize(p)
	return b
}
func (b Builder) WithGlobalStorage(g *mem.Storage) Builder {
	b.inner = b.inner.WithGlobalStorage(g)
	return b
}

// The GPUBuilder interface methods delegate to the inner r9nano builder. The
// factory is already baked into b.inner at MakeBuilder time, and r9nano's
// interface-returning methods return the concrete value, so delegation is safe. // sbin_codex
func (b Builder) WithGPUID(id uint64) gpubuilder.GPUBuilder {
	return b.inner.WithGPUID(id)
}
func (b Builder) WithMemAddrOffset(offset uint64) gpubuilder.GPUBuilder {
	return b.inner.WithMemAddrOffset(offset)
}
func (b Builder) WithRDMAAddressMapper(mapper mem.AddressToPortMapper) gpubuilder.GPUBuilder {
	return b.inner.WithRDMAAddressMapper(mapper)
}
func (b Builder) WithPageTable(pageTable vm.PageTable) gpubuilder.GPUBuilder {
	return b.inner.WithPageTable(pageTable)
}
func (b Builder) Build(name string) *sim.Domain {
	return b.inner.Build(name)
}
