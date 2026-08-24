package shaderarray

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// DataPathTopology owns L1V/L1S construction, exported ports, and wiring. // sbin_codex
type DataPathTopology interface {
	build(*Builder)
	addExternalPorts(*Builder)
	connect(*Builder)
}

// sbin_codex: baseline UVM access-gate ID base (todo 9 of
// mgpusim-uvm-manager). The GMMU owns gate ID 1; the baseline L1V/L1S
// pre-cache translators own the following IDs.
const baselineAccessGateIDBase uint64 = 2

type baselineDataPathTopology struct{} // sbin_codex
type virtualDataPathTopology struct{}  // sbin_codex

// NewBaselineDataPathTopology returns the default translated L1 data path. // sbin_codex
func NewBaselineDataPathTopology() DataPathTopology {
	return baselineDataPathTopology{}
}

// NewVirtualDataPathTopology returns the virtually addressed L1 data path. // sbin_codex
func NewVirtualDataPathTopology() DataPathTopology {
	return virtualDataPathTopology{}
}

func (baselineDataPathTopology) build(b *Builder) {
	b.buildL1VAddressTranslators()
	b.buildL1VCaches()
	b.buildL1VTLBs()
	b.buildL1SAddressTranslator()
	b.buildL1SCache()
	b.buildL1STLB()
	b.configureBaselineAccessGates() // sbin_codex: baseline UVM access-gate wiring (todo 9).
}

func (virtualDataPathTopology) build(b *Builder) {
	b.buildL1VCaches()
	b.buildL1SCache()
}

func (baselineDataPathTopology) addExternalPorts(b *Builder) {
	for i := range b.numCUs {
		b.sa.AddPort(fmt.Sprintf("L1VAddrTransCtrl[%d]", i), b.l1vATs[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VTLBCtrl[%d]", i), b.l1vTLBs[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VCacheCtrl[%d]", i), b.l1vCaches[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VCacheBottom[%d]", i), b.l1vCaches[i].GetPortByName("Bottom"))
		b.sa.AddPort(fmt.Sprintf("L1VTLBBottom[%d]", i), b.l1vTLBs[i].GetPortByName("Bottom"))
	}
	b.sa.AddPort("L1SAddrTransCtrl", b.l1sAT.GetPortByName("Control"))
	b.sa.AddPort("L1STLBCtrl", b.l1sTLB.GetPortByName("Control"))
	b.sa.AddPort("L1SCacheCtrl", b.l1sCache.GetPortByName("Control"))
	b.sa.AddPort("L1SCacheBottom", b.l1sCache.GetPortByName("Bottom"))
	b.sa.AddPort("L1STLBBottom", b.l1sTLB.GetPortByName("Bottom"))
}

func (virtualDataPathTopology) addExternalPorts(b *Builder) {
	for i := range b.numCUs {
		b.sa.AddPort(fmt.Sprintf("L1VCacheCtrl[%d]", i), b.l1vCaches[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VCacheBottom[%d]", i), b.l1vCaches[i].GetPortByName("Bottom"))
	}
	b.sa.AddPort("L1SCacheCtrl", b.l1sCache.GetPortByName("Control"))
	b.sa.AddPort("L1SCacheBottom", b.l1sCache.GetPortByName("Bottom"))
}

func (baselineDataPathTopology) connect(b *Builder) {
	bufferSize := dataPathBufferSize(b)
	for i := range b.numCUs {
		cu, rob, at := b.cus[i], b.l1vROBs[i], b.l1vATs[i]
		cache, translation := b.l1vCaches[i], b.l1vTLBs[i]
		b.l1vMemMappers[i].Port = cache.GetPortByName("Top").AsRemote()
		b.l1vTransMappers[i].Port = translation.GetPortByName("Top").AsRemote()
		cu.VectorMemModules = &mem.SinglePortMapper{Port: rob.GetPortByName("Top").AsRemote()}
		b.connectWithDirectConnection(cu.ToVectorMem, rob.GetPortByName("Top"), bufferSize)
		rob.BottomUnit = at.GetPortByName("Top").AsRemote()
		b.connectWithDirectConnection(rob.GetPortByName("Bottom"), at.GetPortByName("Top"), bufferSize)
		b.connectWithDirectConnection(at.GetPortByName("Translation"), translation.GetPortByName("Top"), bufferSize)
		b.connectWithDirectConnection(cache.GetPortByName("Top"), at.GetPortByName("Bottom"), bufferSize)
	}

	rob, at := b.l1sROB, b.l1sAT
	b.l1sMemMapper.Port = b.l1sCache.GetPortByName("Top").AsRemote()
	b.l1sTransMapper.Port = b.l1sTLB.GetPortByName("Top").AsRemote()
	rob.BottomUnit = at.GetPortByName("Top").AsRemote()
	b.connectWithDirectConnection(rob.GetPortByName("Bottom"), at.GetPortByName("Top"), 32)
	b.connectWithDirectConnection(at.GetPortByName("Translation"), b.l1sTLB.GetPortByName("Top"), 32)
	b.connectWithDirectConnection(b.l1sCache.GetPortByName("Top"), at.GetPortByName("Bottom"), 32)
	connectScalarCUs(b)
}

func (virtualDataPathTopology) connect(b *Builder) {
	bufferSize := dataPathBufferSize(b)
	for i := range b.numCUs {
		cu, rob, cache := b.cus[i], b.l1vROBs[i], b.l1vCaches[i]
		cu.VectorMemModules = &mem.SinglePortMapper{Port: rob.GetPortByName("Top").AsRemote()}
		b.connectWithDirectConnection(cu.ToVectorMem, rob.GetPortByName("Top"), bufferSize)
		rob.BottomUnit = cache.GetPortByName("Top").AsRemote()
		b.connectWithDirectConnection(rob.GetPortByName("Bottom"), cache.GetPortByName("Top"), bufferSize)
	}
	b.l1sROB.BottomUnit = b.l1sCache.GetPortByName("Top").AsRemote()
	b.connectWithDirectConnection(b.l1sROB.GetPortByName("Bottom"), b.l1sCache.GetPortByName("Top"), 32)
	connectScalarCUs(b)
}

func dataPathBufferSize(b *Builder) int {
	if b.memPipelineBufferSize > 0 {
		return b.memPipelineBufferSize
	}
	return 8
}

// configureBaselineAccessGates assigns a UVM access-gate ID to every baseline
// L1V/L1S pre-cache address translator. The gate is inert unless the CP sends
// BlockRange commands, so disabled (-uvm off) behavior is unchanged. // sbin_codex
func (b *Builder) configureBaselineAccessGates() {
	for i := range b.numCUs {
		b.l1vATs[i].SetUVMGateID(baselineAccessGateIDBase + uint64(i))
	}
	b.l1sAT.SetUVMGateID(baselineAccessGateIDBase + uint64(b.numCUs))
}

func connectScalarCUs(b *Builder) {
	conn := directconnection.MakeBuilder().WithEngine(b.simulation.GetEngine()).WithFreq(b.freq).Build(b.name + ".ScalarMemConn")
	b.simulation.RegisterComponent(conn)
	conn.PlugIn(b.l1sROB.GetPortByName("Top"))
	for i := range b.numCUs {
		b.cus[i].ScalarMem = b.l1sROB.GetPortByName("Top")
		conn.PlugIn(b.cus[i].ToScalarMem)
	}
}
