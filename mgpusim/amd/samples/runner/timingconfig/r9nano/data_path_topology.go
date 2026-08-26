package r9nano

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim" // sbin_claude_vc
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
	// plugSAToL2 plugs one shader array's bottom ports into the L1-to-L2
	// network. The baseline also plugs the L1 translators' remote-memory
	// egress; the virtual topology has no L1 data translators, so its egress
	// lives at the L2 boundary instead.
	//
	// The whole per-array group is plugged by the topology, rather than just
	// the differing ports, because directconnection arbitrates in plug-in
	// order - appending the baseline's egress ports after the loop instead of
	// inside it changes arbitration and therefore baseline timing.
	// sbin_claude_vc
	plugSAToL2(*Builder, *directconnection.Comp, *sim.Domain)
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
	// Pre-edit code (commented per project convention). The shader array was
	// told how to route remote pages and to keep virtual addresses on the
	// local path, both of which only ever configured the L1 vector/scalar
	// address translators:
	//
	//	WithRemoteMemoryProviderMapper(b.remoteMemoryProvider).
	//	WithVirtualAddressForLocalMemory().
	//
	// sbin_claude_vc: those translators are gone, so the settings would be
	// dead configuration. Remote pages are recognised at the L2 boundary now.
	return saBuilder.
		WithL1AddressMapper(b.l1DataAddressMapper).
		WithL1IAddressMapper(b.l1AddressMapper).
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
	// Pre-edit code (commented per project convention):
	// conn.PlugIn(b.l2TLBs[0].GetPortByName("Top"))
	//
	// sbin_claude_avatar: the speculation topology decides whether the L1
	// TLB misses reach the L2 TLB directly (baseline) or the ASU (avatar).
	conn.PlugIn(b.speculationTopology.l1TranslationProviderPort(b))
	for _, sa := range b.sas {
		for i := range b.numCUPerShaderArray {
			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VTLBBottom[%d]", i)))
		}
		conn.PlugIn(sa.GetPortByName("L1STLBBottom"))
		conn.PlugIn(sa.GetPortByName("L1ITLBBottom"))
	}
	b.memoryTopology.connectTranslationClients(b, conn) // sbin_codex
}

// connectTranslation wires the only remaining L1-side translation client, the
// instruction TLB. Vector and scalar data accesses reach the L2 with their
// virtual address and are translated at the L2 miss instead.
//
// Pre-edit code (commented per project convention). The vector and scalar L1
// TLBs were plugged in here too:
//
//	for i := range b.numCUPerShaderArray {
//		conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VTLBBottom[%d]", i)))
//	}
//	conn.PlugIn(sa.GetPortByName("L1STLBBottom"))
//
// sbin_claude_vc
func (virtualDataPathTopology) connectTranslation(b *Builder) {
	conn := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".TranslationToL2TLB")
	b.simulation.RegisterComponent(conn)
	// sbin_claude_avatar: route through the speculation topology provider.
	conn.PlugIn(b.speculationTopology.l1TranslationProviderPort(b))

	for _, sa := range b.sas {
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

// connectCP registers the control ports that exist on the virtual data path.
// The vector and scalar L1 translators and TLBs are gone, so the command
// processor has nothing to flush or invalidate there; the shared L2 TLB and
// the per-slice L2 translators carry that duty now.
//
// Pre-edit code (commented per project convention):
//
//	baselineDataPathTopology{}.connectCP(b)
//
// sbin_claude_vc
func (virtualDataPathTopology) connectCP(b *Builder) {
	for _, sa := range b.sas {
		b.addPreCacheTranslator(sa.GetPortByName("L1IAddrTransCtrl"))
		b.addTLB(sa.GetPortByName("L1ITLBCtrl"))
	}

	b.addSharedL2TLBs()
}

func (b *Builder) plugL1ToL2(conn *directconnection.Comp) { // sbin_codex
	b.rdmaEngine.SetLocalModuleFinder(b.l1AddressMapper)
	b.l1AddressMapper.ModuleForOtherAddresses = b.rdmaEngine.RDMARequestInside.AsRemote()
	conn.PlugIn(b.rdmaEngine.RDMARequestInside)
	conn.PlugIn(b.rdmaEngine.RDMADataInside)

	// sbin_codex: the UVM access counter sits on the remote egress between the
	// address translators and the RDMA ingress, so both of its data ports live
	// on this network.
	if b.accessCounter != nil {
		conn.PlugIn(b.accessCounter.Top)
		conn.PlugIn(b.accessCounter.Bottom)
	}
	for _, l2 := range b.l2Caches {
		conn.PlugIn(l2.GetPortByName("Top"))
	}
	// Pre-edit code (commented per project convention). The per-array ports
	// were plugged in here directly, the L1 translators' remote egress
	// included:
	//
	//	for _, sa := range b.sas {
	//		for i := range b.numCUPerShaderArray {
	//			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VCacheBottom[%d]", i)))
	//			conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VAddrTransRemoteBottom[%d]", i)))
	//		}
	//		conn.PlugIn(sa.GetPortByName("L1SCacheBottom"))
	//		conn.PlugIn(sa.GetPortByName("L1SAddrTransRemoteBottom"))
	//		conn.PlugIn(sa.GetPortByName("L1ICacheBottom"))
	//	}
	//
	// sbin_claude_vc: which side owns the remote egress is a topology
	// decision now - the virtual data path has no L1 translators to own it.
	for _, sa := range b.sas {
		b.dataPathTopology.plugSAToL2(b, conn, sa)
	}

	b.memoryTopology.connectRemoteEgress(b, conn)
}

// plugSAToL2 keeps the original plug-in order, cache port then egress port,
// so baseline arbitration is unchanged. // sbin_claude_vc
func (baselineDataPathTopology) plugSAToL2(
	b *Builder,
	conn *directconnection.Comp,
	sa *sim.Domain,
) {
	for i := range b.numCUPerShaderArray {
		conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VCacheBottom[%d]", i)))
		conn.PlugIn(sa.GetPortByName(
			fmt.Sprintf("L1VAddrTransRemoteBottom[%d]", i)))
	}

	conn.PlugIn(sa.GetPortByName("L1SCacheBottom"))
	conn.PlugIn(sa.GetPortByName("L1SAddrTransRemoteBottom"))
	conn.PlugIn(sa.GetPortByName("L1ICacheBottom"))
}

// plugSAToL2 plugs only the caches: remotely accessible pages are recognised
// at the L2 boundary on this topology. // sbin_claude_vc
func (virtualDataPathTopology) plugSAToL2(
	b *Builder,
	conn *directconnection.Comp,
	sa *sim.Domain,
) {
	for i := range b.numCUPerShaderArray {
		conn.PlugIn(sa.GetPortByName(fmt.Sprintf("L1VCacheBottom[%d]", i)))
	}

	conn.PlugIn(sa.GetPortByName("L1SCacheBottom"))
	conn.PlugIn(sa.GetPortByName("L1ICacheBottom"))
}
