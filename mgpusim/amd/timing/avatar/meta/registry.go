// sbin_claude_avatar
// Package meta holds the driver-owned, authoritative Avatar state: the
// per-frame embedded page metadata (avatar-plan.md 1.3, refs/avatar.md 5.5)
// and the 2MB-region randomized physical placement pool (avatar-plan.md
// 1.4). The GPU-side ASU reads it after its modeled speculative-fetch
// latency, the same way the GMMU consults the functional page table after
// its modeled walk latency.
package meta

import (
	"math"
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// Log2RegionSize is the log2 of the contiguity region size (2MB). The MOD
// predicts one V2POffset per 2MB virtual region, and the fragmentation
// allocator places physical memory at this granularity.
const Log2RegionSize = 21

// RegionBytes is the contiguity region size (2MB).
const RegionBytes uint64 = 1 << Log2RegionSize

// Verdict is the outcome of a CAVA rapid-validation check on a speculated
// physical frame (refs/avatar.md 5.6).
type Verdict int

const (
	// VerdictPass means the frame is compressible and its embedded (PID,
	// VPN) matches the request: the speculation is validated (Case A hit).
	VerdictPass Verdict = iota
	// VerdictMismatch means the frame carries embedded metadata for a
	// different (PID, VPN): the speculation is known wrong (Case A miss).
	VerdictMismatch
	// VerdictIncompressible means the frame holds uncompressed data, so no
	// page information is embedded and rapid validation is impossible
	// (Case B): the conventional translation must decide.
	VerdictIncompressible
	// VerdictNoMetadata means no page was ever installed on the frame; the
	// signature identifies the sector as not encoded, behaving like Case B.
	VerdictNoMetadata
)

// frameMeta is the embedded page information of one physical frame.
type frameMeta struct {
	pid   vm.PID
	vAddr uint64 // page-aligned virtual address
}

// vRegionKey identifies one 2MB virtual region of one process.
type vRegionKey struct {
	pid     vm.PID
	vRegion uint64
}

// deviceRegions is the 2MB physical region pool of one device.
type deviceRegions struct {
	base        uint64
	pageSize    uint64
	freeRegions []uint64 // region base addresses
	// regionLivePages counts mapped pages per bound region base.
	regionLivePages map[uint64]int
	vRegionToRegion map[vRegionKey]uint64
	regionToVRegion map[uint64]vRegionKey
}

// Stats counts the observable registry behavior.
type Stats struct {
	FrameInstalls     uint64
	FrameInvalidates  uint64
	RegionBinds       uint64
	RegionUnbinds     uint64
	FallbackAllocs    uint64 // region pool exhausted, default pool used
	ValidatePass      uint64
	ValidateMismatch  uint64
	ValidateIncomp    uint64
	ValidateNoMeta    uint64
}

// Registry holds the authoritative Avatar metadata and placement state. All
// methods are mutex-protected so the driver (writer) and the per-GPU ASUs
// (readers) can share it under the parallel engine.
type Registry struct {
	mu sync.Mutex

	log2PageSize      uint64
	compressRatio     float64
	compressThreshold uint64
	seed              uint64

	devices map[int]*deviceRegions
	// frames maps frame ID (pAddr >> log2PageSize) to embedded metadata.
	frames map[uint64]frameMeta
	// frameRegion maps an allocated frame's pAddr to its region base, so
	// FreeFrame can tell region-owned frames from default-pool frames.
	frameRegion map[uint64]uint64

	stats Stats
}

// NewRegistry creates an Avatar registry. compressRatio is the fraction of
// frames whose sectors compress well enough to embed page information
// (deterministic per-frame draw, avatar-plan.md 1.3).
func NewRegistry(
	log2PageSize uint64,
	compressRatio float64,
	seed uint64,
) *Registry {
	r := &Registry{
		log2PageSize:  log2PageSize,
		compressRatio: compressRatio,
		seed:          seed,
		devices:       make(map[int]*deviceRegions),
		frames:        make(map[uint64]frameMeta),
		frameRegion:   make(map[uint64]uint64),
	}

	switch {
	case compressRatio >= 1:
		r.compressThreshold = math.MaxUint64
	case compressRatio <= 0:
		r.compressThreshold = 0
	default:
		r.compressThreshold = uint64(compressRatio * float64(math.MaxUint64))
	}

	return r
}

// splitmix64 is a deterministic 64-bit mixer (public-domain SplitMix64).
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb

	return x ^ (x >> 31)
}

// RegisterDevice carves a device's memory range into a 2MB physical region
// pool for randomized placement. A tail smaller than one region is unused.
func (r *Registry) RegisterDevice(
	deviceID int,
	base, size, pageSize uint64,
) {
	r.mu.Lock()
	defer r.mu.Unlock()

	numRegions := size / RegionBytes
	dev := &deviceRegions{
		base:            base,
		pageSize:        pageSize,
		freeRegions:     make([]uint64, 0, numRegions),
		regionLivePages: make(map[uint64]int),
		vRegionToRegion: make(map[vRegionKey]uint64),
		regionToVRegion: make(map[uint64]vRegionKey),
	}
	for i := uint64(0); i < numRegions; i++ {
		dev.freeRegions = append(dev.freeRegions, base+i*RegionBytes)
	}

	r.devices[deviceID] = dev
}

// HasDevice reports whether randomized placement is enabled for a device.
func (r *Registry) HasDevice(deviceID int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, found := r.devices[deviceID]

	return found
}

// AllocateFrame places one page. The 2MB virtual region of vAddr is bound
// on first touch to a pseudo-randomly chosen free physical region; the page
// lands at its position inside that region, so PPN-VPN is constant within a
// region and differs across regions (avatar-plan.md 1.4). ok=false means
// the pool is exhausted (or the device is not registered) and the caller
// must fall back to the default frame pool.
func (r *Registry) AllocateFrame(
	deviceID int,
	pid vm.PID,
	vAddr uint64,
) (pAddr uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dev, found := r.devices[deviceID]
	if !found {
		return 0, false
	}

	key := vRegionKey{pid: pid, vRegion: vAddr >> Log2RegionSize}
	regionBase, bound := dev.vRegionToRegion[key]
	if !bound {
		if len(dev.freeRegions) == 0 {
			r.stats.FallbackAllocs++
			return 0, false
		}

		pick := splitmix64(
			uint64(deviceID)<<48 ^ uint64(pid)<<32 ^ key.vRegion ^ r.seed,
		) % uint64(len(dev.freeRegions))
		regionBase = dev.freeRegions[pick]
		last := len(dev.freeRegions) - 1
		dev.freeRegions[pick] = dev.freeRegions[last]
		dev.freeRegions = dev.freeRegions[:last]

		dev.vRegionToRegion[key] = regionBase
		dev.regionToVRegion[regionBase] = key
		r.stats.RegionBinds++
	}

	offsetInRegion := vAddr & (RegionBytes - 1)
	offsetInRegion &^= dev.pageSize - 1
	pAddr = regionBase + offsetInRegion

	dev.regionLivePages[regionBase]++
	r.frameRegion[pAddr] = regionBase

	return pAddr, true
}

// FreeFrame returns a region-owned frame. When the region's last page is
// freed, the region is unbound and returned to the pool. The return value
// reports whether the frame was region-owned; false means the frame came
// from the default pool and the caller must free it there.
func (r *Registry) FreeFrame(deviceID int, pAddr uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	regionBase, owned := r.frameRegion[pAddr]
	if !owned {
		return false
	}

	delete(r.frameRegion, pAddr)

	dev := r.devices[deviceID]
	dev.regionLivePages[regionBase]--
	if dev.regionLivePages[regionBase] > 0 {
		return true
	}

	delete(dev.regionLivePages, regionBase)
	key := dev.regionToVRegion[regionBase]
	delete(dev.regionToVRegion, regionBase)
	delete(dev.vRegionToRegion, key)
	dev.freeRegions = append(dev.freeRegions, regionBase)
	r.stats.RegionUnbinds++

	return true
}

// Install records the embedded page information of a mapped frame: when the
// page's sectors compress, they carry (VPN, permissions) usable by CAVA
// (refs/avatar.md 5.5, 5.11).
func (r *Registry) Install(pAddr uint64, pid vm.PID, vAddr uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pageMask := (uint64(1) << r.log2PageSize) - 1
	r.frames[pAddr>>r.log2PageSize] = frameMeta{
		pid:   pid,
		vAddr: vAddr &^ pageMask,
	}
	r.stats.FrameInstalls++
}

// Invalidate clears a frame's embedded metadata. The old physical location
// of a migrated/freed page must never validate a future mis-speculation
// (refs/avatar.md 5.11).
func (r *Registry) Invalidate(pAddr uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	frameID := pAddr >> r.log2PageSize
	if _, found := r.frames[frameID]; found {
		delete(r.frames, frameID)
		r.stats.FrameInvalidates++
	}
}

// FrameCompressible reports the deterministic per-frame compressibility
// draw (avatar-plan.md 1.3).
func (r *Registry) FrameCompressible(pAddr uint64) bool {
	return r.frameCompressible(pAddr >> r.log2PageSize)
}

func (r *Registry) frameCompressible(frameID uint64) bool {
	if r.compressRatio >= 1 {
		return true
	}

	return splitmix64(frameID^r.seed) < r.compressThreshold
}

// Validate performs the CAVA rapid-validation check for a speculative
// access to specPAddr on behalf of (pid, vAddr) (refs/avatar.md 5.6).
func (r *Registry) Validate(
	specPAddr uint64,
	pid vm.PID,
	vAddr uint64,
) Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()

	frameID := specPAddr >> r.log2PageSize

	if !r.frameCompressible(frameID) {
		// Uncompressed sectors carry no embedded page information; the
		// speculation stays unguaranteed until the real translation (Case B).
		r.stats.ValidateIncomp++
		return VerdictIncompressible
	}

	fm, found := r.frames[frameID]
	if !found {
		r.stats.ValidateNoMeta++
		return VerdictNoMetadata
	}

	pageMask := (uint64(1) << r.log2PageSize) - 1
	if fm.pid == pid && fm.vAddr == vAddr&^pageMask {
		r.stats.ValidatePass++
		return VerdictPass
	}

	r.stats.ValidateMismatch++

	return VerdictMismatch
}

// Occupancy reports the bound and free region counts of a device.
func (r *Registry) Occupancy(deviceID int) (bound, free int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dev, found := r.devices[deviceID]
	if !found {
		return 0, 0
	}

	return len(dev.regionLivePages), len(dev.freeRegions)
}

// Stats returns a copy of the accumulated counters.
func (r *Registry) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stats
}
