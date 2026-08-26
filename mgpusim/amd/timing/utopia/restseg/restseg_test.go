// sbin_claude_utopia
package restseg

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
)

func makeTestConfig() Config {
	// 1MB RestSeg, 4KB pages, 4 ways -> 256 frames, 64 sets.
	// Pre-edit code (commented per project convention):
	// return MakeConfig(1, 0x1000_0000, 1<<20, 4096, 4)
	// sbin_claude_utopia: block size 1 = original per-page indexing.
	return MakeConfig(1, 0x1000_0000, 1<<20, 4096, 4, 1)
}

func TestMakeConfigRoundsDown(t *testing.T) {
	// Pre-edit code (commented per project convention):
	// cfg := MakeConfig(1, 0, (1<<20)+4096*3, 4096, 4)
	cfg := MakeConfig(1, 0, (1<<20)+4096*3, 4096, 4, 1) // sbin_claude_utopia
	if cfg.SegmentBytes != 1<<20 {
		t.Fatalf("expected rounded size %d, got %d", 1<<20, cfg.SegmentBytes)
	}
	if cfg.NumSets != 64 {
		t.Fatalf("expected 64 sets, got %d", cfg.NumSets)
	}
}

func TestFrameAddrSetWayRoundTrip(t *testing.T) {
	cfg := makeTestConfig()
	for set := 0; set < cfg.NumSets; set++ {
		for way := 0; way < cfg.Associativity; way++ {
			pAddr := cfg.FrameAddr(set, way)
			gotSet, gotWay, ok := cfg.SetWayOf(pAddr)
			if !ok || gotSet != set || gotWay != way {
				t.Fatalf("round trip failed for set=%d way=%d", set, way)
			}
		}
	}

	if _, _, ok := cfg.SetWayOf(cfg.BasePAddr - 1); ok {
		t.Fatal("address below base must not be contained")
	}
	if _, _, ok := cfg.SetWayOf(cfg.BasePAddr + cfg.SegmentBytes); ok {
		t.Fatal("address past the segment must not be contained")
	}
}

func TestHashIsDeterministic(t *testing.T) {
	cfg := makeTestConfig()
	for i := 0; i < 1000; i++ {
		vAddr := uint64(i) * cfg.PageSize
		if cfg.SetOf(vAddr) != cfg.SetOf(vAddr) {
			t.Fatal("hash must be deterministic")
		}
		if cfg.SetOf(vAddr) < 0 || cfg.SetOf(vAddr) >= cfg.NumSets {
			t.Fatal("set index out of range")
		}
	}
}

func TestRegistryAllocateLookupRelease(t *testing.T) {
	reg := NewRegistry()
	reg.AddSegment(makeTestConfig())
	pid := vm.PID(1)
	vAddr := uint64(0x2000)

	pAddr, ok := reg.Allocate(1, pid, vAddr)
	if !ok {
		t.Fatal("allocation into an empty RestSeg must succeed")
	}
	if !reg.Contains(pAddr) {
		t.Fatal("allocated frame must be inside the RestSeg")
	}

	got, found := reg.Lookup(1, pid, vAddr)
	if !found || got != pAddr {
		t.Fatalf("lookup returned %x found=%v, want %x", got, found, pAddr)
	}

	if reg.SFCount(1, vAddr) != 1 {
		t.Fatal("SF must count the allocated way")
	}

	// A different PID with the same VPN must not match (PID is in the tag).
	if _, found := reg.Lookup(1, vm.PID(2), vAddr); found {
		t.Fatal("lookup must be PID-qualified")
	}

	if !reg.Release(pid, vAddr) {
		t.Fatal("release of a resident page must succeed")
	}
	if _, found := reg.Lookup(1, pid, vAddr); found {
		t.Fatal("released page must not resolve")
	}
	if reg.SFCount(1, vAddr) != 0 {
		t.Fatal("SF must drop back to zero after release")
	}
}

func TestRegistrySetConflictFallsBack(t *testing.T) {
	cfg := makeTestConfig()
	reg := NewRegistry()
	reg.AddSegment(cfg)
	pid := vm.PID(1)

	// Fill one set completely by scanning for VPNs that hash to set 0.
	target := 0
	filled := 0
	var lastVAddr uint64
	for vpn := uint64(0); filled < cfg.Associativity+1; vpn++ {
		vAddr := vpn * cfg.PageSize
		if cfg.SetOf(vAddr) != target {
			continue
		}
		filled++
		if filled <= cfg.Associativity {
			if _, ok := reg.Allocate(1, pid, vAddr); !ok {
				t.Fatal("allocation must succeed while ways are free")
			}
		} else {
			lastVAddr = vAddr
		}
	}

	// The (assoc+1)-th page hashing to the same set must be rejected so the
	// caller can fall back to FlexSeg (utopia.md 4.8).
	if _, ok := reg.Allocate(1, pid, lastVAddr); ok {
		t.Fatal("allocation into a full set must fail")
	}
	if reg.SFCount(1, lastVAddr) != cfg.Associativity {
		t.Fatal("SF must saturate at the associativity")
	}
}

// sbin_claude_utopia: block-indexing tests.

// TestBlockIndexingGroupsConsecutivePages checks that with BlockPages = B,
// aligned runs of B consecutive pages share one set and the next block moves
// to a different set, while B = 1 keeps the original per-page spreading.
func TestBlockIndexingGroupsConsecutivePages(t *testing.T) {
	// 16-way, 1MB -> 16 sets; block of 16 pages = one full set.
	cfg := MakeConfig(1, 0, 1<<20, 4096, 16, 16)

	base := uint64(0x1000_0000) // block-aligned VPN (0x10000 % 16 == 0)
	first := cfg.SetOf(base)
	for i := uint64(1); i < 16; i++ {
		if got := cfg.SetOf(base + i*4096); got != first {
			t.Fatalf("page %d of the block mapped to set %d, want %d",
				i, got, first)
		}
	}

	next := cfg.SetOf(base + 16*4096)
	if next == first {
		t.Fatal("the next block must not share the set (16 sets, hash step)")
	}
}

// TestBlockIndexingOneIsIdentity locks BlockPages = 1 (and the zero-value
// Config) to the original per-page hash so baseline runs are unaffected.
func TestBlockIndexingOneIsIdentity(t *testing.T) {
	withBlock := MakeConfig(1, 0, 1<<20, 4096, 4, 1)
	zeroValue := withBlock
	zeroValue.BlockPages = 0

	for vpn := uint64(0); vpn < 4096; vpn++ {
		vAddr := vpn * 4096
		want := int(hashVPN(vpn) % uint64(withBlock.NumSets))
		if got := withBlock.SetOf(vAddr); got != want {
			t.Fatalf("BlockPages=1 changed the set of vpn %d: %d != %d",
				vpn, got, want)
		}
		if got := zeroValue.SetOf(vAddr); got != want {
			t.Fatalf("zero-value BlockPages changed the set of vpn %d", vpn)
		}
	}
}

// TestBlockWiderThanAssocRejected: a block that cannot fit into one set's
// ways would guarantee FlexSeg spills, so MakeConfig refuses it.
func TestBlockWiderThanAssocRejected(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MakeConfig must panic when blockPages > associativity")
		}
	}()
	MakeConfig(1, 0, 1<<20, 4096, 4, 8)
}

// TestBlockAllocationFillsOneSet: allocating an aligned 16-page block with
// B = 16 must land every page in the same set on distinct ways.
func TestBlockAllocationFillsOneSet(t *testing.T) {
	cfg := MakeConfig(1, 0, 1<<20, 4096, 16, 16)
	reg := NewRegistry()
	reg.AddSegment(cfg)
	pid := vm.PID(1)

	base := uint64(0x1000_0000)
	set := cfg.SetOf(base)
	seenWays := map[int]bool{}

	for i := uint64(0); i < 16; i++ {
		pAddr, ok := reg.Allocate(1, pid, base+i*4096)
		if !ok {
			t.Fatalf("allocation of block page %d must succeed", i)
		}
		gotSet, gotWay, inSeg := cfg.SetWayOf(pAddr)
		if !inSeg || gotSet != set {
			t.Fatalf("block page %d landed in set %d, want %d", i, gotSet, set)
		}
		if seenWays[gotWay] {
			t.Fatalf("way %d used twice within one block", gotWay)
		}
		seenWays[gotWay] = true
	}

	if reg.SFCount(1, base) != 16 {
		t.Fatal("SF must count all 16 ways of the filled block set")
	}
}

func TestRegistryDeviceIsolation(t *testing.T) {
	reg := NewRegistry()
	reg.AddSegment(makeTestConfig())
	pid := vm.PID(1)
	vAddr := uint64(0x3000)

	if _, ok := reg.Allocate(2, pid, vAddr); ok {
		t.Fatal("device without a RestSeg must not allocate")
	}
	if reg.HasSegments(2) {
		t.Fatal("device 2 must not report segments")
	}
	if !reg.HasSegments(1) {
		t.Fatal("device 1 must report its segment")
	}
}
