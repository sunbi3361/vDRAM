package r9nano

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// MemoryTopology owns the L2 physical-memory boundary and translator controls. // sbin_codex
type MemoryTopology interface {
	buildBoundary(*Builder)
	connectL2AndDRAM(*Builder)
	connectTranslationClients(*Builder, *directconnection.Comp)
	connectCP(*Builder)
	// l2TLBTopChannels reports how many independent top-side request classes
	// the shared L2 TLB must expose. // sbin_claude_vc
	l2TLBTopChannels() int
}

// The shared L2 TLB serves two client classes once the L2 is virtually
// tagged: demand translations from the L1 TLBs, and fill/writeback
// translations from the memory-side address translators. They must not share
// a top port. The port queue is delivered in order, so an L1 TLB that cannot
// take its answer - because its own cache is waiting on an L2 fill - holds up
// the fill translations queued behind it, and those fills are exactly what
// would release that L1 TLB. That is a closed cycle, and it deadlocked
// -gpu=virtual-caching at large working sets. // sbin_claude_vc
const (
	// l2FillTranslationChannel is the L2 TLB top channel reserved for the
	// memory-side address translators.
	l2FillTranslationChannel = 1
	// l2FillTranslationPort is the port key of that channel.
	l2FillTranslationPort = "Top[1]"
	// l2TLBTopChannelsVirtual is the channel count the virtual topology needs.
	l2TLBTopChannelsVirtual = l2FillTranslationChannel + 1
)

func (baselineMemoryTopology) l2TLBTopChannels() int { return 1 } // sbin_claude_vc

func (virtualMemoryTopology) l2TLBTopChannels() int { // sbin_claude_vc
	return l2TLBTopChannelsVirtual
}

type baselineMemoryTopology struct{} // sbin_codex
type virtualMemoryTopology struct{}  // sbin_codex

// NewBaselineMemoryTopology returns direct L2-to-DRAM wiring. // sbin_codex
func NewBaselineMemoryTopology() MemoryTopology { return baselineMemoryTopology{} }

// NewVirtualMemoryTopology returns translated L2-to-DRAM wiring. // sbin_codex
func NewVirtualMemoryTopology() MemoryTopology { return virtualMemoryTopology{} }

func (baselineMemoryTopology) buildBoundary(*Builder) {} // sbin_codex

func (virtualMemoryTopology) buildBoundary(b *Builder) {
	base := addresstranslator.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).WithDeviceID(b.gpuID).WithLog2PageSize(b.log2PageSize).
		WithNumReqPerCycle(16).WithPhysicalAddressPassthrough()
	for i, dram := range b.drams {
		translator := base.WithMemoryProviderMapper(&mem.SinglePortMapper{
			Port: dram.GetPortByName("Top").AsRemote(),
		}).WithTranslationProviderMapper(&mem.SinglePortMapper{
			// Pre-edit code (commented per project convention). The fill
			// translations shared the L1 TLBs' top port:
			// Port: b.l2TLBs[0].GetPortByName("Top").AsRemote(),
			Port: b.l2TLBs[0]. // sbin_claude_vc
						GetPortByName(l2FillTranslationPort).AsRemote(),
		}).Build(fmt.Sprintf("%s.L2AddrTrans[%d]", b.name, i))
		b.simulation.RegisterComponent(translator)
		b.l2AddressTranslators = append(b.l2AddressTranslators, translator)
	}
}

func (baselineMemoryTopology) connectL2AndDRAM(b *Builder) {
	b.l2ToDramConnection = directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".L2ToDRAM")
	b.simulation.RegisterComponent(b.l2ToDramConnection)
	lowModuleFinder := mem.NewInterleavedAddressPortMapper(1 << b.log2MemoryBankInterleavingSize)
	for i, l2 := range b.l2Caches {
		b.l2ToDramConnection.PlugIn(l2.GetPortByName("Bottom"))
		l2.SetAddressToPortMapper(&mem.SinglePortMapper{Port: b.drams[i].GetPortByName("Top").AsRemote()})
	}
	b.connectPhysicalMemoryClients(lowModuleFinder) // sbin_codex
}

func (virtualMemoryTopology) connectL2AndDRAM(b *Builder) {
	b.l2ToDramConnection = directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).Build(b.name + ".L2AddrTransToDRAM")
	b.simulation.RegisterComponent(b.l2ToDramConnection)
	lowModuleFinder := mem.NewInterleavedAddressPortMapper(1 << b.log2MemoryBankInterleavingSize)
	for i, translator := range b.l2AddressTranslators {
		l2ToTranslator := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).
			WithFreq(b.freq).Build(fmt.Sprintf("%s.L2ToL2AddrTrans[%d]", b.name, i))
		b.simulation.RegisterComponent(l2ToTranslator)
		l2ToTranslator.PlugIn(b.l2Caches[i].GetPortByName("Bottom"))
		l2ToTranslator.PlugIn(translator.GetPortByName("Top"))
		b.l2Caches[i].SetAddressToPortMapper(&mem.SinglePortMapper{Port: translator.GetPortByName("Top").AsRemote()})
		b.l2ToDramConnection.PlugIn(translator.GetPortByName("Bottom"))
	}
	b.connectPhysicalMemoryClients(lowModuleFinder) // sbin_codex
}

func (baselineMemoryTopology) connectTranslationClients(*Builder, *directconnection.Comp) {} // sbin_codex

// connectTranslationClients gives the memory-side translators their own
// network to the L2 TLB's fill channel.
//
// Pre-edit code (commented per project convention). They were plugged into
// the L1 TLBs' translation network, which is what closed the deadlock cycle:
//
//	func (virtualMemoryTopology) connectTranslationClients(
//		b *Builder, conn *directconnection.Comp,
//	) {
//		for _, translator := range b.l2AddressTranslators {
//			conn.PlugIn(translator.GetPortByName("Translation"))
//		}
//	}
//
// sbin_claude_vc
func (virtualMemoryTopology) connectTranslationClients(
	b *Builder,
	_ *directconnection.Comp,
) {
	conn := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".L2AddrTransToL2TLB")
	b.simulation.RegisterComponent(conn)

	conn.PlugIn(b.l2TLBs[0].GetPortByName(l2FillTranslationPort))

	for _, translator := range b.l2AddressTranslators {
		conn.PlugIn(translator.GetPortByName("Translation"))
	}
}

func (baselineMemoryTopology) connectCP(*Builder) {} // sbin_codex

func (virtualMemoryTopology) connectCP(b *Builder) {
	for _, translator := range b.l2AddressTranslators {
		b.addPostCacheTranslator(translator.GetPortByName("Control"))
	}
}

func (b *Builder) connectPhysicalMemoryClients(
	lowModuleFinder *mem.InterleavedAddressPortMapper,
) { // sbin_codex
	for _, dram := range b.drams {
		dramTop := dram.GetPortByName("Top")
		b.l2ToDramConnection.PlugIn(dramTop)
		lowModuleFinder.LowModules = append(lowModuleFinder.LowModules, dramTop.AsRemote())
	}
	b.dmaEngine.SetLocalDataSource(lowModuleFinder)
	b.l2ToDramConnection.PlugIn(b.dmaEngine.ToMem)
	b.pmc.MemCtrlFinder = lowModuleFinder
	b.l2ToDramConnection.PlugIn(b.pmc.GetPortByName("LocalMem"))
}
