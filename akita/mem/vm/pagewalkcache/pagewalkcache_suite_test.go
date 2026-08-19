package pagewalkcache

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type noopConn struct {
	sim.HookableBase
}

func (c *noopConn) Name() string { return "noop" }

func (c *noopConn) PlugIn(port sim.Port) { port.SetConnection(c) }

func (c *noopConn) Unplug(sim.Port) {}

func (c *noopConn) NotifyAvailable(sim.Port) {}

func (c *noopConn) NotifySend() {}

type testHarness struct {
	cache   *Comp
	topPort sim.Port
}

func newTestHarness(latency int) testHarness {
	cache := MakeBuilder().
		WithEngine(sim.NewSerialEngine()).
		WithNumLevels(5).
		WithNumBlocks(2).
		WithNumReqPerCycle(4).
		WithLatency(latency).
		Build("PageWalkCache")

	topPort := cache.GetPortByName("Top")
	(&noopConn{}).PlugIn(topPort)
	return testHarness{cache: cache, topPort: topPort}
}

func lookupReq(port sim.Port, pid vm.PID, addr uint64) *LookupReq {
	return LookupReqBuilder{}.
		WithSrc("GMMU").
		WithDst(port.AsRemote()).
		WithPID(pid).
		WithVAddr(addr).
		Build()
}
