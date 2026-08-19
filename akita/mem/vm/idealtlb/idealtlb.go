package idealtlb

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

const (
	tlbStateEnable = 0
	tlbStatePause  = 1
	tlbStateDrain  = 2
	tlbStateFlush  = 3
)

// Comp is an ideal Translation Lookaside Buffer.
// sbin_codex: ideal TLB resolves translations directly from the page table.
type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	topPort     sim.Port
	bottomPort  sim.Port
	controlPort sim.Port

	pageTable      vm.PageTable
	numReqPerCycle int
	latency        int
	state          int
}

// Tick invokes the installed middleware.
func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}
