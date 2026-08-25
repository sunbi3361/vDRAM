package accesscounter

import (
	"sort"

	"github.com/sarchlab/akita/v4/mem/vm"
)

const regionByteSize uint64 = 64 * 1024 // sbin_codex

// RegionKey uniquely identifies a process-owned 64KB virtual region.
type RegionKey struct {
	PID        vm.PID
	RegionBase uint64
}

// RegionSnapshot is immutable reporting state for one counter.
type RegionSnapshot struct {
	Key                 RegionKey
	Count               uint64
	NotificationLatched bool
}

// Stats are the counters the reporter reads back. // sbin_codex
type Stats struct {
	RemoteAccesses uint64
	Notifications  uint64
	StalledWrites  uint64
	ReleasedWrites uint64
	RefusedWrites  uint64
}

// StatsSnapshot is immutable component reporting state.
type StatsSnapshot struct {
	Stats
	Regions              []RegionSnapshot
	PendingNotifications int
	StalledWriteRegions  int
}

type counterState struct {
	count               uint64
	notificationLatched bool
}

// Snapshot returns a deterministic copy of all access-counter state.
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
		Stats:                c.stats,
		Regions:              regions,
		PendingNotifications: len(c.pendingNotifications),
		StalledWriteRegions:  len(c.stalledWrites),
	}
}

func regionKey(pid vm.PID, virtualAddress uint64) RegionKey {
	return RegionKey{
		PID:        pid,
		RegionBase: virtualAddress &^ (regionByteSize - 1),
	}
}
