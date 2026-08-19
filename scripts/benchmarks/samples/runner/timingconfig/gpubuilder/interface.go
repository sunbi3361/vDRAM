// Package gpubuilder defines the interface for GPU builders used in timing
// simulation.
package gpubuilder

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm" // sbin_codex: per-GPU page-table injection.
	"github.com/sarchlab/akita/v4/sim"
)

// GPUBuilder is the interface for building GPUs of different types.
type GPUBuilder interface {
	WithGPUID(id uint64) GPUBuilder
	WithMemAddrOffset(offset uint64) GPUBuilder
	WithRDMAAddressMapper(mapper mem.AddressToPortMapper) GPUBuilder
	WithPageTable(pageTable vm.PageTable) GPUBuilder // sbin_codex: bind each GPU to its own page table.
	Build(name string) *sim.Domain
}
