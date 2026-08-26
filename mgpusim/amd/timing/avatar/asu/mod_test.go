// sbin_claude_avatar
package asu

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

func TestModTrainToConfidence(t *testing.T) {
	mod := newModTable(4, 2)
	pid := vm.PID(1)

	// A fresh entry (confidence 1) must not speculate yet.
	mod.train(pid, 0x1000, 0x100000)
	if _, ok := mod.predict(pid, 0x2000); ok {
		t.Fatal("confidence 1 must not speculate")
	}

	// A confirming translation raises it to the threshold.
	mod.train(pid, 0x3000, 0x100000)
	offset, ok := mod.predict(pid, 0x4000)
	if !ok || offset != 0x100000 {
		t.Fatalf("expected confident offset 0x100000, got %x ok=%v",
			offset, ok)
	}
}

func TestModOffsetReplacedOnlyAtZeroConfidence(t *testing.T) {
	mod := newModTable(4, 2)
	pid := vm.PID(1)

	mod.train(pid, 0x1000, 0x100000)
	mod.train(pid, 0x1000, 0x100000)
	mod.train(pid, 0x1000, 0x100000) // confidence 3 (saturated)

	// One mismatch drops confidence to 1 but keeps the offset.
	mod.train(pid, 0x1000, 0x200000)
	if _, ok := mod.predict(pid, 0x1000); ok {
		t.Fatal("confidence below threshold must not speculate")
	}

	// A second mismatch reaches zero: the offset is replaced and restarts
	// at the initial confidence.
	mod.train(pid, 0x1000, 0x200000)
	mod.train(pid, 0x1000, 0x200000)
	offset, ok := mod.predict(pid, 0x1000)
	if !ok || offset != 0x200000 {
		t.Fatalf("expected replaced offset 0x200000, got %x ok=%v",
			offset, ok)
	}
}

func TestModRegionsAreIndependent(t *testing.T) {
	mod := newModTable(4, 2)
	pid := vm.PID(1)
	regionB := meta.RegionBytes

	mod.train(pid, 0x1000, 0x100000)
	mod.train(pid, 0x1000, 0x100000)

	if _, ok := mod.predict(pid, regionB+0x1000); ok {
		t.Fatal("another 2MB region must not inherit the offset")
	}
}

func TestModLRUReplacement(t *testing.T) {
	mod := newModTable(2, 2)
	pid := vm.PID(1)
	regionB := meta.RegionBytes

	mod.train(pid, 0*regionB, 0x100000)
	mod.train(pid, 0*regionB, 0x100000)
	mod.train(pid, 1*regionB, 0x200000)
	mod.train(pid, 1*regionB, 0x200000)

	// Touch region 0 so region 1 becomes the LRU victim.
	if _, ok := mod.predict(pid, 0); !ok {
		t.Fatal("region 0 must be confident")
	}

	mod.train(pid, 2*regionB, 0x300000)

	if _, ok := mod.predict(pid, 0); !ok {
		t.Fatal("region 0 must survive the replacement")
	}
	mod.train(pid, 1*regionB, 0x200000) // reinstalls as a fresh entry
	if _, ok := mod.predict(pid, 1*regionB); ok {
		t.Fatal("region 1 must have been evicted and restart cold")
	}
}
