package driver

import (
	"github.com/sarchlab/akita/v4/sim"
)

// faultHandlingCompleteEvent fires after the fixed software fault-handling
// latency and starts the service stage of one 64KB fault transaction. The
// delay is one scheduled event, never a cycle-by-cycle wait (spec 10.3).
type faultHandlingCompleteEvent struct {
	*sim.EventBase
	faultID string
}

func newFaultHandlingCompleteEvent(
	at sim.VTimeInSec,
	handler sim.Handler,
	faultID string,
) *faultHandlingCompleteEvent {
	return &faultHandlingCompleteEvent{
		EventBase: sim.NewEventBase(at, handler),
		faultID:   faultID,
	}
}

// migrationCompleteEvent finalizes a migration once its data plane finished.
// In ideal mode it is scheduled at the current time, so the same functional
// transitions run with zero transfer latency.
type migrationCompleteEvent struct {
	*sim.EventBase
	migrationID string
}

func newMigrationCompleteEvent(
	at sim.VTimeInSec,
	handler sim.Handler,
	migrationID string,
) *migrationCompleteEvent {
	return &migrationCompleteEvent{
		EventBase:   sim.NewEventBase(at, handler),
		migrationID: migrationID,
	}
}

// remoteMapCompleteEvent fires after the fixed software fault-handling latency
// and publishes the REMOTE mappings of one 64KB region. It is the lazy
// counterpart of the install the allocator performs eagerly when
// LazyRemotePTE is off, and it never moves a page. // sbin_claude_uvm
type remoteMapCompleteEvent struct {
	*sim.EventBase
	region RegionKey
}

func newRemoteMapCompleteEvent(
	at sim.VTimeInSec,
	handler sim.Handler,
	region RegionKey,
) *remoteMapCompleteEvent {
	return &remoteMapCompleteEvent{
		EventBase: sim.NewEventBase(at, handler),
		region:    region,
	}
}
