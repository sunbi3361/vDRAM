package accesscounter

import (
	"sort"

	"github.com/sarchlab/akita/v4/mem/vm"
)

const regionByteSize uint64 = 64 * 1024 // sbin_codex

// RegionKey uniquely identifies a process-owned 64KB virtual region.
// sbin_codex
type RegionKey struct {
	PID        vm.PID
	RegionBase uint64
}

// RegionSnapshot is immutable reporting state for one counter. // sbin_codex
type RegionSnapshot struct {
	Key                 RegionKey
	Count               uint64
	NotificationLatched bool
}

// StatsSnapshot is immutable component reporting state. // sbin_codex
type StatsSnapshot struct {
	Regions              []RegionSnapshot
	PendingNotifications int
}

type counterState struct {
	count               uint64
	notificationLatched bool
}

// Snapshot returns a deterministic copy of all access-counter state.
// sbin_codex
func (c *Comp) Snapshot() StatsSnapshot {
	regions := make([]RegionSnapshot, 0, len(c.counters))
	for key, counter := range c.counters {
		regions = append(regions, RegionSnapshot{
			Key:                 key,
			Count:               counter.count,
			NotificationLatched: counter.notificationLatched,
		})
	}
	sort.Slice(regions, func(i, j int) bool {
		if regions[i].Key.PID != regions[j].Key.PID {
			return regions[i].Key.PID < regions[j].Key.PID
		}
		return regions[i].Key.RegionBase < regions[j].Key.RegionBase
	})
	return StatsSnapshot{
		Regions:              regions,
		PendingNotifications: len(c.pendingNotifications),
	}
}

func regionKey(pid vm.PID, virtualAddress uint64) RegionKey {
	return RegionKey{
		PID:        pid,
		RegionBase: virtualAddress &^ (regionByteSize - 1),
	}
}
