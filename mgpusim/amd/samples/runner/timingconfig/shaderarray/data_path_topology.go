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

type (
	baselineDataPathTopology struct{} // sbin_codex
	virtualDataPathTopology  struct{} // sbin_codex
)

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
}

// build omits the L1 vector and scalar address translators and TLBs.
//
// Virtual caching means the L1 and L2 data caches are tagged by virtual
// address, so a data access needs no translation until it misses in the L2 -
// that is the whole point of the design, and it is where the translation
// bandwidth saving comes from. Keeping the baseline L1 translation path here
// spent an L2 TLB translation on every L1 access while the address it
// produced was, for local pages, the same virtual address it was handed.
//
// Pre-edit code (commented per project convention). The topology delegated
// to the baseline, which built the full L1 translation path:
//
//	baselineDataPathTopology{}.build(b)
//
// sbin_claude_vc
func (virtualDataPathTopology) build(b *Builder) {
	b.buildL1VCaches()
	b.buildL1SCache()
}

func (baselineDataPathTopology) addExternalPorts(b *Builder) {
	for i := range b.numCUs {
		b.sa.AddPort(fmt.Sprintf("L1VAddrTransCtrl[%d]", i), b.l1vATs[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VAddrTransRemoteBottom[%d]", i), b.l1vATs[i].GetPortByName("RemoteBottom")) // sbin_codex
		b.sa.AddPort(fmt.Sprintf("L1VTLBCtrl[%d]", i), b.l1vTLBs[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VCacheCtrl[%d]", i), b.l1vCaches[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VCacheBottom[%d]", i), b.l1vCaches[i].GetPortByName("Bottom"))
		b.sa.AddPort(fmt.Sprintf("L1VTLBBottom[%d]", i), b.l1vTLBs[i].GetPortByName("Bottom"))
	}
	b.sa.AddPort("L1SAddrTransCtrl", b.l1sAT.GetPortByName("Control"))
	b.sa.AddPort("L1SAddrTransRemoteBottom", b.l1sAT.GetPortByName("RemoteBottom")) // sbin_codex
	b.sa.AddPort("L1STLBCtrl", b.l1sTLB.GetPortByName("Control"))
	b.sa.AddPort("L1SCacheCtrl", b.l1sCache.GetPortByName("Control"))
	b.sa.AddPort("L1SCacheBottom", b.l1sCache.GetPortByName("Bottom"))
	b.sa.AddPort("L1STLBBottom", b.l1sTLB.GetPortByName("Bottom"))
}

// addExternalPorts exports only what exists on the virtual data path: the
// caches. There are no L1 vector/scalar translator or TLB ports to export.
//
// Pre-edit code (commented per project convention):
//
//	baselineDataPathTopology{}.addExternalPorts(b)
//
// sbin_claude_vc
func (virtualDataPathTopology) addExternalPorts(b *Builder) {
	for i := range b.numCUs {
		b.sa.AddPort(fmt.Sprintf("L1VCacheCtrl[%d]", i),
			b.l1vCaches[i].GetPortByName("Control"))
		b.sa.AddPort(fmt.Sprintf("L1VCacheBottom[%d]", i),
			b.l1vCaches[i].GetPortByName("Bottom"))
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

// connect wires the reorder buffers straight to the virtually tagged caches.
//
// Pre-edit code (commented per project convention). The topology delegated to
// the baseline, which interposed an address translator between each reorder
// buffer and its cache:
//
//	baselineDataPathTopology{}.connect(b)
//
// sbin_claude_vc
func (virtualDataPathTopology) connect(b *Builder) {
	bufferSize := dataPathBufferSize(b)

	for i := range b.numCUs {
		cu, rob, cache := b.cus[i], b.l1vROBs[i], b.l1vCaches[i]
		cu.VectorMemModules = &mem.SinglePortMapper{
			Port: rob.GetPortByName("Top").AsRemote(),
		}
		b.connectWithDirectConnection(
			cu.ToVectorMem, rob.GetPortByName("Top"), bufferSize)
		rob.BottomUnit = cache.GetPortByName("Top").AsRemote()
		b.connectWithDirectConnection(rob.GetPortByName("Bottom"),
			cache.GetPortByName("Top"), bufferSize)
	}

	b.l1sROB.BottomUnit = b.l1sCache.GetPortByName("Top").AsRemote()
	b.connectWithDirectConnection(b.l1sROB.GetPortByName("Bottom"),
		b.l1sCache.GetPortByName("Top"), 32)

	connectScalarCUs(b)
}

func dataPathBufferSize(b *Builder) int {
	if b.memPipelineBufferSize > 0 {
		return b.memPipelineBufferSize
	}
	return 8
}

func connectScalarCUs(b *Builder) {
	conn := directconnection.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(b.freq).
		Build(b.name + ".ScalarMemConn") // sbin_codex: wrapped for lll.
	b.simulation.RegisterComponent(conn)
	conn.PlugIn(b.l1sROB.GetPortByName("Top"))
	for i := range b.numCUs {
		b.cus[i].ScalarMem = b.l1sROB.GetPortByName("Top")
		conn.PlugIn(b.cus[i].ToScalarMem)
	}
}
