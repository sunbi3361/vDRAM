package r9nano

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim/directconnection"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/shaderarray"
)

// DataPathTopology owns L1 mapper selection and L1/TLB/CP wiring. // sbin_codex
type DataPathTopology interface {
	initializeMapper(*Builder)
	configureShaderArray(*Builder, shaderarray.Builder) shaderarray.Builder
	connectL1ToL2(*Builder)
	connectTranslation(*Builder)
	connectCP(*Builder)
}

type (
	baselineDataPathTopology struct{} // sbin_codex
	virtualDataPathTopology  struct{} // sbin_codex
)

// NewBaselineDataPathTopology returns the physical translated L1 topology. // sbin_codex
func NewBaselineDataPathTopology() DataPathTopology { return baselineDataPathTopology{} }

// NewVirtualDataPathTopology returns the virtual L1V/L1S topology. // sbin_codex
func NewVirtualDataPathTopology() DataPathTopology { return virtualDataPathTopology{} }

func (baselineDataPathTopology) initializeMapper(b *Builder) {
	b.l1DataAddressMapper = b.l1AddressMapper // sbin_codex
}

func (virtualDataPathTopology) initializeMapper(b *Builder) {
	b.l1DataAddressMapper = mem.NewInterleavedAddressPortMapper(
		1 << b.log2MemoryBankInterleavingSize) // sbin_codex: virtual data addresses are unrestricted.
}

func (baselineDataPathTopology) configureShaderArray(
	b *Builder,
	saBuilder shaderarray.Builder,
) shaderarray.Builder {
	return saBuilder.
		WithL1AddressMapper(b.l1DataAddressMapper).
		WithRemoteMemoryProviderMapper(b.remoteMemoryProvider).         // sbin_codex
		WithDataPathTopology(shaderarray.NewBaselineDataPathTopology()) // sbin_codex
}

func (virtualDataPathTopology) configureShaderArray(
	b *Builder,
	saBuilder shaderarray.Builder,
) shaderarray.Builder {
	return saBuilder.
		WithL1AddressMapper(b.l1DataAddressMapper).
		WithL1IAddressMapper(b.l1AddressMapper).
		WithRemoteMemoryProviderMapper(b.remoteMemoryProvider).        // sbin_codex
		WithVirtualAddressForLocalMemory().                            // sbin_codex
		WithDataPathTopology(shaderarray.NewVirtualDataPathTopology()) // sbin_codex: L1I remains baseline.
}

func (baselineDataPathTopology) connectL1ToL2(b *Builder) {
	conn := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".L1ToL2")
	// b.plugL1ToL2(conn) // sbin_codex: pre-edit unregistered connection.
	b.simulation.RegisterComponent(conn) // sbin_codex
	b.plugL1ToL2(conn)                   // sbin_codex
}

func (virtualDataPathTopology) connectL1ToL2(b *Builder) {
	conn := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".L1ToL2")
	b.simulation.RegisterComponent(conn)
	for _, l2 := range b.l2Caches {
		b.l1DataAddressMapper.LowModules = append(
			b.l1DataAddressMapper.LowModules, l2.GetPortByName("Top").AsRemote())
	}
	b.plugL1ToL2(conn) // sbin_codex
}

func (baselineDataPathTopology) connectTranslation(b *Builder) {
	conn := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".L1TLBToL2TLB")
	b.simulation.RegisterComponent(conn) // sbin_codex
	conn.PlugIn(b.l2TLBs[0].GetPortByName("Top"))
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VTLBBottom[%d]", i)))
		}
		conn.PlugIn(sa.GetPortByName("L1STLBBottom"))
		conn.PlugIn(sa.GetPortByName("L1ITLBBottom"))
	}
	b.memoryTopology.connectTranslationClients(b, conn) // sbin_codex
}

func (virtualDataPathTopology) connectTranslation(b *Builder) {
	conn := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".TranslationToL2TLB")
	b.simulation.RegisterComponent(conn)
	conn.PlugIn(b.l2TLBs[0].GetPortByName("Top"))
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VTLBBottom[%d]", i))) // sbin_codex
		}
		conn.PlugIn(sa.GetPortByName("L1STLBBottom")) // sbin_codex
		conn.PlugIn(sa.GetPortByName("L1ITLBBottom"))
	}
	b.memoryTopology.connectTranslationClients(b, conn) // sbin_codex
}

func (baselineDataPathTopology) connectCP(b *Builder) {
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			b.addPreCacheTranslator(sa.GetPortByName(fmt.Sprintf("L1VAddrTransCtrl[%d]", i)))
			b.addTLB(sa.GetPortByName(fmt.Sprintf("L1VTLBCtrl[%d]", i)))
		}
		b.addPreCacheTranslator(sa.GetPortByName("L1SAddrTransCtrl"))
		b.addPreCacheTranslator(sa.GetPortByName("L1IAddrTransCtrl"))
		b.addTLB(sa.GetPortByName("L1STLBCtrl"))
		b.addTLB(sa.GetPortByName("L1ITLBCtrl"))
	}
	b.addSharedL2TLBs() // sbin_codex
}

func (virtualDataPathTopology) connectCP(b *Builder) {
	// Pre-edit code (commented per AGENTS.md convention):
	// for _, sa := range b.sas {
	// 	b.addPreCacheTranslator(sa.GetPortByName("L1IAddrTransCtrl"))
	// 	b.addTLB(sa.GetPortByName("L1ITLBCtrl"))
	// }
	// b.addSharedL2TLBs()
	baselineDataPathTopology{}.connectCP(b) // sbin_codex
}

func (b *Builder) plugL1ToL2(conn *directconnection.Comp) { // sbin_codex
	b.rdmaEngine.SetLocalModuleFinder(b.l1AddressMapper)
	b.l1AddressMapper.ModuleForOtherAddresses = b.rdmaEngine.RDMARequestInside.AsRemote()
	conn.PlugIn(b.rdmaEngine.RDMARequestInside)
	conn.PlugIn(b.rdmaEngine.RDMADataInside)
	for _, l2 := range b.l2Caches {
		conn.PlugIn(l2.GetPortByName("Top"))
	}
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VCacheBottom[%d]", i)))
			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VAddrTransRemoteBottom[%d]", i))) // sbin_codex
		}
		conn.PlugIn(sa.GetPortByName("L1SCacheBottom"))
		conn.PlugIn(sa.GetPortByName("L1SAddrTransRemoteBottom")) // sbin_codex
		conn.PlugIn(sa.GetPortByName("L1ICacheBottom"))
	}
}
