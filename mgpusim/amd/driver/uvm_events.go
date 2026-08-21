package driver

import (
	"github.com/sarchlab/akita/v4/sim"
)

// faultHandlingCompleteEvent fires after the fixed fault-handling latency and
// triggers the fault-ready (TBN/capacity/eviction/migration) stage.
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

// migrationCompleteEvent fires after the CPU<->GPU interconnect transfer and
// finalizes a normal-mode migration.
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

// idealMigrationCompleteEvent completes a migration at the current simulated
// time in ideal mode, preserving the same state transitions.
type idealMigrationCompleteEvent struct {
	*sim.EventBase
	migrationID string
}

func newIdealMigrationCompleteEvent(
	at sim.VTimeInSec,
	handler sim.Handler,
	migrationID string,
) *idealMigrationCompleteEvent {
	return &idealMigrationCompleteEvent{
		EventBase:   sim.NewEventBase(at, handler),
		migrationID: migrationID,
	}
}
