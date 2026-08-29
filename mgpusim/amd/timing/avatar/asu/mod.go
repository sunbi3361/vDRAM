// sbin_claude_avatar
package asu

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	// sbin_claude_avatar: the meta import is gone with the 2MB-region key.
	// "github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// modKey identifies one memory instruction of one process.
//
// Pre-edit code (commented per project convention): the table used to be
// indexed by the 2MB contiguity region, because vm.TranslationReq carried no
// PC (avatar-plan.md 1.2).
//
//	type modKey struct {
//		pid     vm.PID
//		vRegion uint64
//	}
//
// sbin_claude_avatar: that proxy keyed the MOD on the same 2MB granularity
// the fragmentation allocator places memory at (meta.Log2RegionSize is
// shared by both), so V2POffset was constant across every entry's whole
// reach and the table could not mispredict by construction - the confidence
// counter of refs/avatar.md 5.2 could never fire. The PC now rides the
// access down from the CU, so the key is the paper's: "A new PC creates a
// new MOD entry" (refs/avatar.md 5.2), probed "using the memory instruction
// PC" (5.3). One streaming load walks many regions, so its stored offset is
// now genuinely wrong at every region boundary, which is exactly the
// pressure the confidence counter exists to absorb.
type modKey struct {
	pid vm.PID
	pc  uint64
}

// modEntry is one Mapping Offset Detection record (refs/avatar.md 5.2).
type modEntry struct {
	valid      bool
	key        modKey
	offset     int64 // V2POffset: page-aligned PAddr - VAddr, in bytes
	confidence int
}

const (
	modInitialConfidence = 1
	modMaxConfidence     = 3
)

// modTable is a fully-associative, LRU-replaced MOD table.
type modTable struct {
	entries   []modEntry
	lru       []int // index 0 is the next victim
	threshold int
}

func newModTable(numEntries, threshold int) *modTable {
	t := &modTable{
		entries:   make([]modEntry, numEntries),
		lru:       make([]int, numEntries),
		threshold: threshold,
	}
	for i := range t.lru {
		t.lru[i] = i
	}

	return t
}

// Pre-edit code (commented per project convention):
//
//	func modKeyOf(pid vm.PID, vAddr uint64) modKey {
//		return modKey{pid: pid, vRegion: vAddr >> meta.Log2RegionSize}
//	}
//
// sbin_claude_avatar: keyed by the instruction PC instead. A zero PC means
// the producer had none to offer (instruction/scalar fetch, page-walk
// traffic); those all collapse onto one entry, which is harmless because
// they are a rounding error of the L1 TLB miss stream and the caller counts
// them separately.
func modKeyOf(pid vm.PID, pc uint64) modKey {
	return modKey{pid: pid, pc: pc}
}

func (t *modTable) find(key modKey) int {
	for i := range t.entries {
		if t.entries[i].valid && t.entries[i].key == key {
			return i
		}
	}

	return -1
}

// visit refreshes the LRU order for one entry index.
func (t *modTable) visit(idx int) {
	for i, candidate := range t.lru {
		if candidate != idx {
			continue
		}
		copy(t.lru[i:], t.lru[i+1:])
		t.lru[len(t.lru)-1] = idx

		return
	}
}

// predict returns the stored V2POffset when the entry is confident enough
// to speculate (refs/avatar.md 5.3: confidence threshold 2).
//
// Pre-edit signature (commented per project convention):
// func (t *modTable) predict(pid vm.PID, vAddr uint64) (offset int64, ok bool)
//
// sbin_claude_avatar: probed by PC, not by the address being translated.
func (t *modTable) predict(pid vm.PID, pc uint64) (offset int64, ok bool) {
	idx := t.find(modKeyOf(pid, pc))
	if idx < 0 {
		return 0, false
	}

	t.visit(idx)
	entry := &t.entries[idx]
	if entry.confidence < t.threshold {
		return 0, false
	}

	return entry.offset, true
}

// train updates the MOD with a completed real translation (refs/avatar.md
// 5.2): +1 on a matching offset, -2 on a mismatch; the stored offset is
// replaced only when the confidence reaches zero.
//
// Pre-edit signature (commented per project convention):
// func (t *modTable) train(pid vm.PID, vAddr uint64, offset int64)
//
// sbin_claude_avatar: trained per PC, not per address region.
func (t *modTable) train(pid vm.PID, pc uint64, offset int64) {
	key := modKeyOf(pid, pc)
	idx := t.find(key)

	if idx < 0 {
		victim := t.lru[0]
		t.entries[victim] = modEntry{
			valid:      true,
			key:        key,
			offset:     offset,
			confidence: modInitialConfidence,
		}
		t.visit(victim)

		return
	}

	t.visit(idx)
	entry := &t.entries[idx]

	if entry.offset == offset {
		if entry.confidence < modMaxConfidence {
			entry.confidence++
		}

		return
	}

	entry.confidence -= 2
	if entry.confidence <= 0 {
		entry.offset = offset
		entry.confidence = modInitialConfidence
	}
}
