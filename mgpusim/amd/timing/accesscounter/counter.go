package accesscounter

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
)

func (c *Comp) countAccepted(remoteDemand mem.RemoteDemandInfo) {
	key := regionKey(remoteDemand.PID, remoteDemand.VAddr)
	counter, found := c.counters[key]
	if !found {
		counter = &counterState{}
		c.counters[key] = counter
	}
	counter.count++
	if counter.notificationLatched || counter.count < c.threshold {
		return
	}
	counter.notificationLatched = true
	notification := vm.NewAccessCounterNotifyReq(
		c.Top.AsRemote(), c.driverDestination)
	notification.PID = key.PID
	notification.RegionBase = key.RegionBase
	notification.DeviceID = remoteDemand.DeviceID
	c.pendingNotifications = append(c.pendingNotifications, notification)
}

func (c *Comp) retryNotification() bool {
	if len(c.pendingNotifications) == 0 {
		return false
	}
	notification := c.pendingNotifications[0]
	if sendError := c.Top.Send(notification); sendError != nil {
		return true
	}
	c.pendingNotifications = c.pendingNotifications[1:]
	return true
}

func (c *Comp) reset(request *vm.AccessCounterResetReq) {
	key := regionKey(request.PID, request.RegionBase)
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
