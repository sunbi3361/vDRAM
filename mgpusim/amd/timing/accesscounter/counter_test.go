// sbin_codex: Behavioral tests for typed access-counter state and notifications.
package accesscounter

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
)

func assertRegion(
	t *testing.T,
	c *Comp,
	wantKey RegionKey,
	wantCount uint64,
	wantNotified bool,
) {
	t.Helper()
	snapshot := c.Snapshot()
	if len(snapshot.Regions) != 1 {
		t.Fatalf("expected one region, got %+v", snapshot)
	}
	got := snapshot.Regions[0]
	if got.Key != wantKey || got.Count != wantCount || got.NotificationLatched != wantNotified {
		t.Fatalf("unexpected region: got %+v", got)
	}
}

func Test_Comp_separates_counters_by_PID(t *testing.T) {
	// Given
	c := newTestComponent(10, 8)
	deliver(t, c.Top, markedRead(c, vm.PID(1), 0x23456, 1))
	deliver(t, c.Top, markedRead(c, vm.PID(2), 0x23456, 1))

	// When
	c.Tick()
	c.Tick()

	// Then
	_ = outgoing(t, c.Bottom)
	_ = outgoing(t, c.Bottom)
	regions := c.Snapshot().Regions
	if len(regions) != 2 || regions[0].Key.PID == regions[1].Key.PID {
		t.Fatalf("PID counters were combined: %+v", regions)
	}
}

func Test_Comp_separates_counters_by_aligned_region(t *testing.T) {
	// Given
	c := newTestComponent(10, 8)
	deliver(t, c.Top, markedRead(c, vm.PID(1), 0x2ffff, 1))
	deliver(t, c.Top, markedRead(c, vm.PID(1), 0x30000, 1))

	// When
	c.Tick()
	c.Tick()

	// Then
	_ = outgoing(t, c.Bottom)
	_ = outgoing(t, c.Bottom)
	regions := c.Snapshot().Regions
	if len(regions) != 2 || regions[0].Key.RegionBase != 0x20000 || regions[1].Key.RegionBase != 0x30000 {
		t.Fatalf("regions were not separated and aligned: %+v", regions)
	}
}

func Test_Comp_emits_exactly_one_notification_at_threshold(t *testing.T) {
	// Given
	c := newTestComponent(2, 8)
	deliver(t, c.Top, markedRead(c, vm.PID(5), 0x45678, 11))
	deliver(t, c.Top, markedWrite(c, vm.PID(5), 0x4ffff, 22))

	// When
	c.Tick()
	c.Tick()

	// Then
	_ = outgoing(t, c.Bottom)
	_ = outgoing(t, c.Bottom)
	notify, ok := outgoing(t, c.Top).(*vm.AccessCounterNotifyReq)
	if !ok {
		t.Fatalf("expected notification, got %T", notify)
	}
	if notify.PID != vm.PID(5) || notify.RegionBase != 0x40000 || notify.DeviceID != 22 {
		t.Fatalf("unexpected notification payload: %+v", notify)
	}
	deliver(t, c.Top, markedRead(c, vm.PID(5), 0x40001, 33))
	c.Tick()
	_ = outgoing(t, c.Bottom)
	if duplicate := c.Top.RetrieveOutgoing(); duplicate != nil {
		t.Fatalf("latched counter emitted duplicate notification: %T", duplicate)
	}
	assertRegion(t, c, RegionKey{PID: vm.PID(5), RegionBase: 0x40000}, 3, true)
}

func Test_Comp_retries_notification_after_send_backpressure_without_new_access(t *testing.T) {
	// Given
	c := newTestComponent(1, 1)
	blocker := vm.NewAccessCounterNotifyReq(c.Top.AsRemote(), testDriver)
	if err := c.Top.Send(blocker); err != nil {
		t.Fatalf("prefill top failed: %v", err)
	}
	deliver(t, c.Top, markedRead(c, vm.PID(6), 0x50000, 7))

	// When
	c.Tick()
	_ = outgoing(t, c.Bottom)
	_ = outgoing(t, c.Top)
	c.Tick()

	// Then
	notify, ok := outgoing(t, c.Top).(*vm.AccessCounterNotifyReq)
	if !ok || notify.PID != vm.PID(6) || notify.RegionBase != 0x50000 {
		t.Fatalf("pending notification was not retried: %+v", notify)
	}
	if got := c.Snapshot().PendingNotifications; got != 0 {
		t.Fatalf("notification remained pending after send: %d", got)
	}
}

func Test_Comp_reset_clears_and_rearms_only_requested_region(t *testing.T) {
	// Given
	c := newTestComponent(1, 8)
	deliver(t, c.Top, markedRead(c, vm.PID(7), 0x60010, 2))
	c.Tick()
	_ = outgoing(t, c.Bottom)
	_ = outgoing(t, c.Top)
	reset := vm.NewAccessCounterResetReq(testDriver, c.Top.AsRemote())
	reset.PID = vm.PID(7)
	reset.RegionBase = 0x60000
	reset.DeviceID = 99
	deliver(t, c.Top, reset)

	// When
	c.Tick()
	deliver(t, c.Top, markedWrite(c, vm.PID(7), 0x6ff00, 4))
	c.Tick()

	// Then
	_ = outgoing(t, c.Bottom)
	notify, ok := outgoing(t, c.Top).(*vm.AccessCounterNotifyReq)
	if !ok || notify.DeviceID != 4 {
		t.Fatalf("reset did not re-arm region: %+v", notify)
	}
	assertRegion(t, c, RegionKey{PID: vm.PID(7), RegionBase: 0x60000}, 1, true)
}
