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
