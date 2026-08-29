// sbin_claude_avatar
package asu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// sbin_claude_avatar v4: these tests used to drive the MOD with virtual
// addresses, because the table was keyed by 2MB region. The second argument
// is the instruction PC now (refs/avatar.md 5.2).

func TestModTrainToConfidence(t *testing.T) {
	mod := newModTable(4, 2)
	pid := vm.PID(1)
	pc := uint64(0x2000)

	// A fresh entry (confidence 1) must not speculate yet.
	mod.train(pid, pc, 0x100000)
	if _, ok := mod.predict(pid, pc); ok {
		t.Fatal("confidence 1 must not speculate")
	}

	// A confirming translation raises it to the threshold.
	mod.train(pid, pc, 0x100000)
	offset, ok := mod.predict(pid, pc)
	if !ok || offset != 0x100000 {
		t.Fatalf("expected confident offset 0x100000, got %x ok=%v",
			offset, ok)
	}
}

func TestModOffsetReplacedOnlyAtZeroConfidence(t *testing.T) {
	mod := newModTable(4, 2)
	pid := vm.PID(1)
	pc := uint64(0x2000)

	mod.train(pid, pc, 0x100000)
	mod.train(pid, pc, 0x100000)
	mod.train(pid, pc, 0x100000) // confidence 3 (saturated)

	// One mismatch drops confidence to 1 but keeps the offset.
	mod.train(pid, pc, 0x200000)
	if _, ok := mod.predict(pid, pc); ok {
		t.Fatal("confidence below threshold must not speculate")
	}

	// A second mismatch reaches zero: the offset is replaced and restarts
	// at the initial confidence.
	mod.train(pid, pc, 0x200000)
	mod.train(pid, pc, 0x200000)
	offset, ok := mod.predict(pid, pc)
	if !ok || offset != 0x200000 {
		t.Fatalf("expected replaced offset 0x200000, got %x ok=%v",
			offset, ok)
	}
}

// Pre-edit test (commented per project convention): it asserted that two
// 2MB regions do not share an entry, which was the whole point of the
// region key.
//
//	func TestModRegionsAreIndependent(t *testing.T) { ... }
//
// sbin_claude_avatar v4: what has to hold now is that two static
// instructions do not share an entry, whatever addresses they touch.
func TestModInstructionsAreIndependent(t *testing.T) {
	mod := newModTable(4, 2)
	pid := vm.PID(1)

	mod.train(pid, 0x2000, 0x100000)
	mod.train(pid, 0x2000, 0x100000)

	if _, ok := mod.predict(pid, 0x2008); ok {
		t.Fatal("another instruction must not inherit the offset")
	}
}

// TestModOneInstructionAcrossRegionsLosesConfidence is the behavior the
// region key made unreachable: a single streaming load walks many 2MB
// regions, and under a fragmented placement each region has its own
// V2POffset. The confidence counter must actually respond to that - the MOD
// has to stop speculating rather than predict a stale offset forever.
// sbin_claude_avatar v4
func TestModOneInstructionAcrossRegionsLosesConfidence(t *testing.T) {
	mod := newModTable(32, 2)
	pid := vm.PID(1)
	pc := uint64(0x2000)
	regionB := int64(meta.RegionBytes)

	// The load streams through its first region and becomes confident.
	mod.train(pid, pc, 1*regionB)
	mod.train(pid, pc, 1*regionB)
	if _, ok := mod.predict(pid, pc); !ok {
		t.Fatal("a settled instruction must be confident")
	}

	// It crosses into a region placed somewhere else. One mismatch (-2)
	// already drops it below the threshold, so it stops speculating.
	mod.train(pid, pc, 7*regionB)
	if _, ok := mod.predict(pid, pc); ok {
		t.Fatal("a region boundary must cost the entry its confidence")
	}

	// Once it settles into the new region it speculates again, on the new
	// offset.
	mod.train(pid, pc, 7*regionB)
	mod.train(pid, pc, 7*regionB)
	offset, ok := mod.predict(pid, pc)
	if !ok || offset != 7*regionB {
		t.Fatalf("expected relearned offset %x, got %x ok=%v",
			7*regionB, offset, ok)
	}
}

func TestModLRUReplacement(t *testing.T) {
	mod := newModTable(2, 2)
	pid := vm.PID(1)
	pcA, pcB, pcC := uint64(0x2000), uint64(0x2008), uint64(0x2010)

	mod.train(pid, pcA, 0x100000)
	mod.train(pid, pcA, 0x100000)
	mod.train(pid, pcB, 0x200000)
	mod.train(pid, pcB, 0x200000)

	// Touch pcA so pcB becomes the LRU victim.
	if _, ok := mod.predict(pid, pcA); !ok {
		t.Fatal("pcA must be confident")
	}

	mod.train(pid, pcC, 0x300000)

	if _, ok := mod.predict(pid, pcA); !ok {
		t.Fatal("pcA must survive the replacement")
	}
	mod.train(pid, pcB, 0x200000) // reinstalls as a fresh entry
	if _, ok := mod.predict(pid, pcB); ok {
		t.Fatal("pcB must have been evicted and restart cold")
	}
}
