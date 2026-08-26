package accesscounter

// sbin_codex: 64KB remote-access accounting and the write-stall release path.

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

// countAccepted records one completed remote demand and notifies the driver
// the first time the region reaches the threshold.
func (c *Comp) countAccepted(remoteDemand mem.RemoteDemandInfo) {
	key := regionKey(remoteDemand.PID, remoteDemand.VAddr)

	counter, found := c.counters[key]
	if !found {
		counter = &counterState{}
		c.counters[key] = counter
	}

	counter.count++
	c.stats.RemoteAccesses++

	if counter.notificationLatched || counter.count < c.threshold {
		return
	}

	c.notifyRegion(key, remoteDemand.DeviceID)
}

// notifyRegion emits at most one outstanding notification per region and per
// residency episode (spec 16).
func (c *Comp) notifyRegion(key RegionKey, deviceID uint64) {
	counter, found := c.counters[key]
	if !found {
		counter = &counterState{}
		c.counters[key] = counter
	}

	if counter.notificationLatched {
		return
	}

	counter.notificationLatched = true

	notification := vm.NewAccessCounterNotifyReq(
		c.Ctrl.AsRemote(), c.ctrlDestination)
	notification.PID = key.PID
	notification.RegionBase = key.RegionBase
	notification.DeviceID = deviceID

	c.pendingNotifications = append(c.pendingNotifications, notification)
	c.stats.Notifications++
}

func (c *Comp) retryNotification() bool {
	if len(c.pendingNotifications) == 0 || c.ctrlDestination == "" {
		return false
	}

	notification := c.pendingNotifications[0]
	notification.Dst = c.ctrlDestination

	if sendError := c.Ctrl.Send(notification); sendError != nil {
		return false
	}

	c.pendingNotifications = c.pendingNotifications[1:]

	return true
}

// resetRegion clears the counter of one region and re-arms its notification.
//
// A region that still holds stalled writes is raised again on the spot. Those
// writes are released by a replay and nothing but a notification asks the
// driver for one, so a reset that merely dropped the pending notification
// would leave them — and the compute units waiting on them — with nobody left
// to answer. The driver resets a region after evicting it for exactly this
// reason: the eviction consumed a notification it could not answer, and the
// fresh one takes the ordinary admission path. // sbin_codex
func (c *Comp) resetRegion(key RegionKey) {
	delete(c.counters, key)

	held := len(c.stalledWrites[key]) > 0
	retained := c.pendingNotifications[:0]

	for _, notification := range c.pendingNotifications {
		pendingKey := RegionKey{
			PID: notification.PID, RegionBase: notification.RegionBase,
		}
		if pendingKey != key {
			retained = append(retained, notification)
		}
	}

	c.pendingNotifications = retained

	if held {
		c.notifyRegion(key, c.deviceID)
	}
}

// ResetAll clears every counter. The driver calls it at a kernel boundary so
// remote accesses never accumulate across kernels (spec 14.2).
//
// Regions that still hold stalled writes keep their notification, for the same
// reason resetRegion re-raises them. // sbin_codex
func (c *Comp) ResetAll() {
	c.counters = make(map[RegionKey]*counterState)

	// Pre-edit code (commented per AGENTS.md convention):
	// c.pendingNotifications = nil
	retained := c.pendingNotifications[:0]

	for _, notification := range c.pendingNotifications {
		key := RegionKey{
			PID: notification.PID, RegionBase: notification.RegionBase,
		}
		if len(c.stalledWrites[key]) > 0 {
			c.counters[key] = &counterState{notificationLatched: true}
			retained = append(retained, notification)
		}
	}

	c.pendingNotifications = retained
}

// releaseRange replays the writes stalled inside a region that the driver has
// just made GPU-local.
func (c *Comp) releaseRange(req *vm.UVMFaultReplayReq) {
	end := req.StartVA + req.Size

	for base := req.StartVA; base < end; base += regionByteSize {
		key := RegionKey{PID: req.PID, RegionBase: base}

		// The writes leave first: resetRegion re-raises a region that still
		// holds any, and this command is the answer to the notification that
		// was raised for these. // sbin_codex
		writes := c.stalledWrites[key]
		delete(c.stalledWrites, key)

		c.resetRegion(key)

		if len(writes) == 0 {
			continue
		}

		for _, write := range writes {
			if req.Refused {
				// The driver cannot make the region GPU-local, so the write
				// completes over PCIe instead of waiting forever.
				c.pendingRemoteWrites = append(c.pendingRemoteWrites, write)
				c.stats.RefusedWrites++

				continue
			}

			c.pendingRetries = append(c.pendingRetries,
				vm.NewUVMRemoteRetryRsp(
					c.Top.AsRemote(), write.Src, write.ID))
			c.stats.ReleasedWrites++
		}
	}
}

// sendPendingRemoteWrite performs a write the driver refused to migrate.
func (c *Comp) sendPendingRemoteWrite() bool {
	if len(c.pendingRemoteWrites) == 0 {
		return false
	}

	if c.atOutstandingLimit() { // sbin_claude
		return false
	}

	original := c.pendingRemoteWrites[0]
	forwarded := original.Clone().(*mem.WriteReq)
	forwarded.Meta().Src = c.Bottom.AsRemote()
	forwarded.Meta().Dst = c.bottomDestination

	if sendError := c.Bottom.Send(forwarded); sendError != nil {
		return false
	}

	c.pendingRemoteWrites = c.pendingRemoteWrites[1:]
	c.trackTransaction(
		forwarded.Meta().ID, original, original.RemoteDemandInfo)

	if original.RemoteDemandInfo != nil {
		c.countAccepted(*original.RemoteDemandInfo)
	}

	return true
}

func (c *Comp) sendPendingRetry() bool {
	if len(c.pendingRetries) == 0 {
		return false
	}

	retry := c.pendingRetries[0]
	if sendError := c.Top.Send(retry); sendError != nil {
		return false
	}

	c.pendingRetries = c.pendingRetries[1:]

	return true
}
