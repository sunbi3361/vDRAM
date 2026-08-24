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
	WithDriverPort(port sim.Port) GPUBuilder         // sbin_codex: route CP control responses to the driver.
	WithPageTable(pageTable vm.PageTable) GPUBuilder // sbin_codex: bind each GPU to its own page table.
	// sbin_codex: UVM demand-fault service provider (driver UVM port).
	WithUVMServiceProvider(provider sim.RemotePort) GPUBuilder
	// WithAccessCounterThreshold(thresh uint64) GPUBuilder // sbin_codex
	Build(name string) *sim.Domain
}
