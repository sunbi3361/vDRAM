// sbin_claude_utopia
package restseg

import (
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// A TAREntry is the authoritative Tag-Array record for one occupied RestSeg
// way: TAR[set][way] = {valid, PID, VPN tag} (utopia.md 4.3). The PID is part
// of the tag so multi-process workloads cannot alias.
type TAREntry struct {
	Valid bool
	PID   vm.PID
	Tag   uint64 // full VPN
}

// segmentState is one RestSeg with its authoritative TAR and SF.
type segmentState struct {
	cfg Config
	tar [][]TAREntry // [set][way]
	sf  []int        // SF[set] = number of valid ways (utopia.md 4.4)
}

// Registry holds the driver-owned, authoritative RestSeg state: segment
// layouts, TAR and SF contents (utopia.md 4.8: the allocator needs a global
// view of RestSeg occupancy). The GPU-side RestSeg walker reads it after its
// modeled TAR/SF access latency, the same way the GMMU consults the
// functional page table after its modeled walk latency. All methods are
// mutex-protected; iteration is over an insertion-ordered slice so results
// stay deterministic under the parallel engine.
type Registry struct {
	mu       sync.Mutex
	segments []*segmentState
}

// NewRegistry creates an empty RestSeg registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// AddSegment registers one reserved RestSeg region.
func (r *Registry) AddSegment(cfg Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tar := make([][]TAREntry, cfg.NumSets)
	for set := range tar {
		tar[set] = make([]TAREntry, cfg.Associativity)
	}

	r.segments = append(r.segments, &segmentState{
		cfg: cfg,
		tar: tar,
		sf:  make([]int, cfg.NumSets),
	})
}

// SegmentConfigs returns the layouts of every RestSeg owned by a device, in
// registration order.
func (r *Registry) SegmentConfigs(deviceID int) []Config {
	r.mu.Lock()
	defer r.mu.Unlock()

	configs := make([]Config, 0)
	for _, seg := range r.segments {
		if seg.cfg.DeviceID == deviceID {
			configs = append(configs, seg.cfg)
		}
	}

	return configs
}

// HasSegments reports whether a device owns at least one RestSeg.
func (r *Registry) HasSegments(deviceID int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, seg := range r.segments {
		if seg.cfg.DeviceID == deviceID {
			return true
		}
	}

	return false
}

// Allocate tries to place a page into a RestSeg of the device: hash the VPN,
// take the first free way in the hashed set (utopia.md 4.8). It returns the
// RestSeg frame address, or ok=false when every relevant set is full (the
// caller then falls back to FlexSeg).
func (r *Registry) Allocate(
	deviceID int,
	pid vm.PID,
	vAddr uint64,
) (pAddr uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, seg := range r.segments {
		if seg.cfg.DeviceID != deviceID {
			continue
		}

		set := seg.cfg.SetOf(vAddr)
		for way := 0; way < seg.cfg.Associativity; way++ {
			if seg.tar[set][way].Valid {
				continue
			}

			seg.tar[set][way] = TAREntry{
				Valid: true,
				PID:   pid,
				Tag:   seg.cfg.TagOf(vAddr),
			}
			seg.sf[set]++

			return seg.cfg.FrameAddr(set, way), true
		}
	}

	return 0, false
}

// Lookup resolves a virtual address against every RestSeg of the device; it
// is the functional truth behind the timed RestSeg walk. ok=false means
// NotInRestSeg.
func (r *Registry) Lookup(
	deviceID int,
	pid vm.PID,
	vAddr uint64,
) (pAddr uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, seg := range r.segments {
		if seg.cfg.DeviceID != deviceID {
			continue
		}

		set := seg.cfg.SetOf(vAddr)
		tag := seg.cfg.TagOf(vAddr)
		for way := 0; way < seg.cfg.Associativity; way++ {
			entry := seg.tar[set][way]
			if entry.Valid && entry.PID == pid && entry.Tag == tag {
				return seg.cfg.FrameAddr(set, way), true
			}
		}
	}

	return 0, false
}

// SFCount returns the Set Filter value (number of valid ways) of the set the
// address hashes to, summed over the device's RestSegs. Zero means the page
// cannot be RestSeg-resident and the TAR access can be skipped (utopia.md
// 4.4).
func (r *Registry) SFCount(deviceID int, vAddr uint64) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := 0
	for _, seg := range r.segments {
		if seg.cfg.DeviceID != deviceID {
			continue
		}
		count += seg.sf[seg.cfg.SetOf(vAddr)]
	}

	return count
}

// Contains reports whether a physical address is a RestSeg frame of any
// registered segment.
func (r *Registry) Contains(pAddr uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, seg := range r.segments {
		if seg.cfg.Contains(pAddr) {
			return true
		}
	}

	return false
}

// IsResident reports whether the (pid, page) pair currently owns a RestSeg
// frame on any device.
func (r *Registry) IsResident(pid vm.PID, vAddr uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, found := r.lookupAnyDevice(pid, vAddr)

	return found
}

// Release frees the RestSeg way owned by (pid, page). It returns false when
// the page is not RestSeg-resident.
func (r *Registry) Release(pid vm.PID, vAddr uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, seg := range r.segments {
		set := seg.cfg.SetOf(vAddr)
		tag := seg.cfg.TagOf(vAddr)
		for way := 0; way < seg.cfg.Associativity; way++ {
			entry := &seg.tar[set][way]
			if entry.Valid && entry.PID == pid && entry.Tag == tag {
				*entry = TAREntry{}
				seg.sf[set]--
				return true
			}
		}
	}

	return false
}

// OccupiedFrames returns the number of valid TAR entries over every segment
// of a device (statistics).
func (r *Registry) OccupiedFrames(deviceID int) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := 0
	for _, seg := range r.segments {
		if seg.cfg.DeviceID != deviceID {
			continue
		}
		for _, sf := range seg.sf {
			count += sf
		}
	}

	return count
}

// lookupAnyDevice scans every segment regardless of device. Callers hold the
// lock.
func (r *Registry) lookupAnyDevice(
	pid vm.PID,
	vAddr uint64,
) (pAddr uint64, ok bool) {
	for _, seg := range r.segments {
		set := seg.cfg.SetOf(vAddr)
		tag := seg.cfg.TagOf(vAddr)
		for way := 0; way < seg.cfg.Associativity; way++ {
			entry := seg.tar[set][way]
			if entry.Valid && entry.PID == pid && entry.Tag == tag {
				return seg.cfg.FrameAddr(set, way), true
			}
		}
	}

	return 0, false
}
