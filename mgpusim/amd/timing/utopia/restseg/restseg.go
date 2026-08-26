// sbin_claude_utopia
// Package restseg defines the shared RestSeg layout used by both the driver
// (allocation policy, authoritative TAR/SF state) and the GPU-side RestSeg
// walker (timed lookup). Keeping the hash function and the set/way-to-frame
// arithmetic in one leaf package guarantees that allocation and lookup always
// agree on the layout (utopia.md 4.5).
package restseg

// Config describes one RestSeg region carved out of a device's physical
// memory. A RestSeg is organized like a set-associative cache over physical
// page frames: a VPN hashes to one set and may occupy one of the
// Associativity ways in that set (utopia.md 4.2).
type Config struct {
	// DeviceID is the driver device ID (GPU 1 = 1) that owns this RestSeg.
	DeviceID int
	// BasePAddr is the physical address of the first RestSeg frame.
	BasePAddr uint64
	// SegmentBytes is the total RestSeg size. Always NumSets *
	// Associativity * PageSize.
	SegmentBytes uint64
	// PageSize is the page size of the frames in this RestSeg.
	PageSize uint64
	// Associativity is the number of ways per set.
	Associativity int
	// NumSets is the number of sets: NumFrames / Associativity.
	NumSets int
}

// MakeConfig builds a RestSeg layout. The requested size is rounded down to a
// whole number of sets (sets * assoc * pageSize). It panics when the rounded
// segment cannot hold a single set.
func MakeConfig(
	deviceID int,
	basePAddr uint64,
	requestedBytes uint64,
	pageSize uint64,
	associativity int,
) Config {
	if pageSize == 0 || associativity <= 0 {
		panic("restseg: page size and associativity must be positive")
	}

	frames := requestedBytes / pageSize
	numSets := int(frames) / associativity
	if numSets == 0 {
		panic("restseg: RestSeg too small for one set")
	}

	return Config{
		DeviceID:      deviceID,
		BasePAddr:     basePAddr,
		SegmentBytes:  uint64(numSets) * uint64(associativity) * pageSize,
		PageSize:      pageSize,
		Associativity: associativity,
		NumSets:       numSets,
	}
}

// NumFrames returns the total number of physical page frames in the RestSeg.
func (c Config) NumFrames() int {
	return c.NumSets * c.Associativity
}

// vpnOf returns the virtual page number of an address.
func (c Config) vpnOf(vAddr uint64) uint64 {
	return vAddr / c.PageSize
}

// hashVPN mixes the VPN bits by XOR-folding so that adjacent pages spread
// over the sets. The exact function is arbitrary but MUST be identical for
// allocation (driver) and lookup (RestSeg walker); both go through SetOf.
func hashVPN(vpn uint64) uint64 {
	h := vpn
	h ^= h >> 12
	h ^= h >> 24
	h ^= h >> 48
	return h
}

// SetOf returns the RestSeg set index a virtual address hashes to.
func (c Config) SetOf(vAddr uint64) int {
	return int(hashVPN(c.vpnOf(vAddr)) % uint64(c.NumSets))
}

// TagOf returns the virtual-page identity stored in the TAR for an address.
// The full VPN is used as the tag (utopia.md 4.3).
func (c Config) TagOf(vAddr uint64) uint64 {
	return c.vpnOf(vAddr)
}

// FrameAddr derives the physical frame address of a set/way position:
// PFN = RestSegBasePFN + set*Associativity + way (utopia.md 4.5).
func (c Config) FrameAddr(set, way int) uint64 {
	return c.BasePAddr +
		(uint64(set)*uint64(c.Associativity)+uint64(way))*c.PageSize
}

// Contains reports whether a physical address falls inside this RestSeg.
func (c Config) Contains(pAddr uint64) bool {
	return pAddr >= c.BasePAddr && pAddr < c.BasePAddr+c.SegmentBytes
}

// SetWayOf is the inverse of FrameAddr. The bool is false when the physical
// address is not a RestSeg frame of this segment.
func (c Config) SetWayOf(pAddr uint64) (set, way int, ok bool) {
	if !c.Contains(pAddr) {
		return 0, 0, false
	}

	frame := (pAddr - c.BasePAddr) / c.PageSize
	return int(frame) / c.Associativity, int(frame) % c.Associativity, true
}
