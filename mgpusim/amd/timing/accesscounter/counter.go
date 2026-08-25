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
	dbg("NOTIFY region=%#x", key.RegionBase)

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
func (c *Comp) resetRegion(key RegionKey) {
	if _, ok := c.counters[key]; ok {
		dbg("RESET region=%#x stalled=%d", key.RegionBase, len(c.stalledWrites[key]))
	}
	delete(c.counters, key)

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
}

// ResetAll clears every counter. The driver calls it at a kernel boundary so
// remote accesses never accumulate across kernels (spec 14.2).
func (c *Comp) ResetAll() {
	c.counters = make(map[RegionKey]*counterState)
	c.pendingNotifications = nil
}

// releaseRange replays the writes stalled inside a region that the driver has
// just made GPU-local.
func (c *Comp) releaseRange(req *vm.UVMFaultReplayReq) {
	end := req.StartVA + req.Size

	for base := req.StartVA; base < end; base += regionByteSize {
		key := RegionKey{PID: req.PID, RegionBase: base}

		c.resetRegion(key)

		writes := c.stalledWrites[key]
		if len(writes) == 0 {
			continue
		}

		delete(c.stalledWrites, key)
		dbg("RELEASE region=%#x refused=%v n=%d", key.RegionBase, req.Refused, len(writes))

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
