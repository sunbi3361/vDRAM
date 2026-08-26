// sbin_claude_avatar
package asu

import (
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"
)

// modKey identifies one 2MB virtual region of one process. The paper's MOD
// is PC-indexed; the translation path carries no PC, so the table is indexed
// by the contiguity region the PC proxy stands for (avatar-plan.md 1.2).
type modKey struct {
	pid     vm.PID
	vRegion uint64
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

func modKeyOf(pid vm.PID, vAddr uint64) modKey {
	return modKey{pid: pid, vRegion: vAddr >> meta.Log2RegionSize}
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
func (t *modTable) predict(pid vm.PID, vAddr uint64) (offset int64, ok bool) {
	idx := t.find(modKeyOf(pid, vAddr))
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
func (t *modTable) train(pid vm.PID, vAddr uint64, offset int64) {
	key := modKeyOf(pid, vAddr)
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
