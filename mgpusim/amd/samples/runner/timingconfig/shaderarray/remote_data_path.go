package shaderarray

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
)

type remoteDataPathConfig struct { // sbin_codex
	mapper             mem.AddressToPortMapper
	virtualLocalMemory bool
}

// WithRemoteMemoryProviderMapper routes remotely accessible data pages to a
// dedicated memory provider. // sbin_codex
func (b Builder) WithRemoteMemoryProviderMapper(mapper mem.AddressToPortMapper) Builder {
	b.remoteDataPath.mapper = mapper
	return b
}

// WithVirtualAddressForLocalMemory preserves virtual addresses and PIDs on the
// local data-cache path. // sbin_codex
func (b Builder) WithVirtualAddressForLocalMemory() Builder {
	b.remoteDataPath.virtualLocalMemory = true
	return b
}

func (b *Builder) configureDataAddressTranslator(
	builder addresstranslator.Builder,
) addresstranslator.Builder {
	builder = builder.WithRemoteMemoryProviderMapper(b.remoteDataPath.mapper)
	if b.remoteDataPath.virtualLocalMemory {
		builder = builder.WithVirtualAddressForLocalMemory()
	}
	return builder
}

func admitLocalPage(page vm.Page) bool { // sbin_codex
	return !page.RemoteAccessible
}
