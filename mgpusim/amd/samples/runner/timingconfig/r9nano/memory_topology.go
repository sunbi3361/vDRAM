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
	// uvmRangeVirtual reports whether the L2 data cache is virtually
	// addressed, which selects PID+VA matching for UVM range operations. // sbin_codex
	uvmRangeVirtual() bool
}

type baselineMemoryTopology struct{} // sbin_codex
type virtualMemoryTopology struct{}  // sbin_codex

// NewBaselineMemoryTopology returns direct L2-to-DRAM wiring. // sbin_codex
func NewBaselineMemoryTopology() MemoryTopology { return baselineMemoryTopology{} }

// NewVirtualMemoryTopology returns translated L2-to-DRAM wiring. // sbin_codex
func NewVirtualMemoryTopology() MemoryTopology { return virtualMemoryTopology{} }

func (baselineMemoryTopology) uvmRangeVirtual() bool { return false } // sbin_codex
func (virtualMemoryTopology) uvmRangeVirtual() bool  { return true }  // sbin_codex

func (baselineMemoryTopology) buildBoundary(*Builder) {} // sbin_codex

func (virtualMemoryTopology) buildBoundary(b *Builder) {
	base := addresstranslator.MakeBuilder().WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).WithDeviceID(b.gpuID).WithLog2PageSize(b.log2PageSize).
		WithNumReqPerCycle(16).WithPhysicalAddressPassthrough()
	for i, dram := range b.drams {
		translator := base.WithMemoryProviderMapper(&mem.SinglePortMapper{
			Port: dram.GetPortByName("Top").AsRemote(),
		}).WithTranslationProviderMapper(&mem.SinglePortMapper{
			Port: b.l2TLBs[0].GetPortByName("Top").AsRemote(),
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

func (virtualMemoryTopology) connectTranslationClients(b *Builder, conn *directconnection.Comp) {
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
