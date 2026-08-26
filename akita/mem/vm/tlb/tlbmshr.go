package tlb

import (
	"log"

	"github.com/sarchlab/akita/v4/mem/vm"
)

type mshrEntry struct {
	pid         vm.PID
	vAddr       uint64
	Requests    []*vm.TranslationReq
	reqToBottom *vm.TranslationReq
	page        vm.Page

	// staleOnFill marks a lookup that a range invalidation raced with. The
	// waiting requests are still answered, but the returned page is not
	// installed in the TLB so the next access re-walks the page table.
	// sbin_codex
	staleOnFill bool

	// sbin_claude_softwalker: In-TLB MSHR (SoftWalker, MICRO'25 4.5). A miss
	// that arrives with the dedicated MSHR full is tracked in a repurposed
	// TLB way instead of being refused. inTLB marks such an entry; setID and
	// wayID name the reserved way that holds it until the fill returns.
	inTLB bool
	setID int
	wayID int
}

// newMSHREntry returns a new MSHR entry object
func newMSHREntry() *mshrEntry {
	e := new(mshrEntry)
	return e
}

// mshr is an interface that controls MSHR entries
type mshr interface {
	Add(pid vm.PID, addr uint64) *mshrEntry
	Remove(pid vm.PID, addr uint64) *mshrEntry
	AllEntries() []*mshrEntry
	IsFull() bool
	Reset()
	GetEntry(pid vm.PID, vAddr uint64) *mshrEntry
	IsEntryPresent(pid vm.PID, vAddr uint64) bool
	IsEmpty() bool
	// sbin_claude_softwalker: In-TLB MSHR entries live in the same list, so
	// merges, fills, cancels and invalidations see them exactly like
	// dedicated entries. Only admission distinguishes the two kinds.
	AddInTLB(pid vm.PID, vAddr uint64, setID, wayID int) *mshrEntry
	InTLBCount() int
}

type mshrImpl struct {
	capacity int
	entries  []*mshrEntry
	// inTLBCount tracks how many entries are In-TLB overflow entries;
	// IsFull and Add gate on the dedicated count only.
	// sbin_claude_softwalker
	inTLBCount int
}

// newMSHR returns a new mshr object
func newMSHR(capacity int) mshr {
	m := new(mshrImpl)
	m.capacity = capacity

	return m
}

func (m *mshrImpl) Add(pid vm.PID, vAddr uint64) *mshrEntry {
	for _, e := range m.entries {
		if e.pid == pid && e.vAddr == vAddr {
			panic("entry already in mshr")
		}
	}

	// Pre-edit code (commented per project convention):
	// if len(m.entries) >= m.capacity {
	//
	// sbin_claude_softwalker: only dedicated entries count against the
	// dedicated capacity.
	if len(m.entries)-m.inTLBCount >= m.capacity {
		log.Panic("MSHR is full")
	}

	entry := newMSHREntry()
	entry.pid = pid
	entry.vAddr = vAddr
	m.entries = append(m.entries, entry)

	return entry
}

// AddInTLB adds an entry tracked in a repurposed TLB way. The caller owns
// the way reservation; the MSHR only records where the entry lives.
// sbin_claude_softwalker
func (m *mshrImpl) AddInTLB(
	pid vm.PID,
	vAddr uint64,
	setID, wayID int,
) *mshrEntry {
	for _, e := range m.entries {
		if e.pid == pid && e.vAddr == vAddr {
			panic("entry already in mshr")
		}
	}

	entry := newMSHREntry()
	entry.pid = pid
	entry.vAddr = vAddr
	entry.inTLB = true
	entry.setID = setID
	entry.wayID = wayID
	m.entries = append(m.entries, entry)
	m.inTLBCount++

	return entry
}

// InTLBCount reports how many live entries are In-TLB overflow entries.
// sbin_claude_softwalker
func (m *mshrImpl) InTLBCount() int {
	return m.inTLBCount
}

func (m *mshrImpl) Remove(pid vm.PID, vAddr uint64) *mshrEntry {
	for i, e := range m.entries {
		if e.pid == pid && e.vAddr == vAddr {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)

			if e.inTLB { // sbin_claude_softwalker
				m.inTLBCount--
			}

			return e
		}
	}

	panic("trying to remove an non-exist entry")
}

func (m *mshrImpl) AllEntries() []*mshrEntry {
	return m.entries
}

func (m *mshrImpl) IsFull() bool {
	// Pre-edit code (commented per project convention):
	// return len(m.entries) >= m.capacity
	//
	// sbin_claude_softwalker: full means the dedicated entries are exhausted;
	// In-TLB overflow entries do not occupy dedicated capacity.
	return len(m.entries)-m.inTLBCount >= m.capacity
}

func (m *mshrImpl) Reset() {
	m.entries = nil
	m.inTLBCount = 0 // sbin_claude_softwalker
}

func (m *mshrImpl) GetEntry(pid vm.PID, vAddr uint64) *mshrEntry {
	for _, e := range m.entries {
		if e.pid == pid && e.vAddr == vAddr {
			return e
		}
	}

	return nil
}

func (m *mshrImpl) IsEntryPresent(pid vm.PID, vAddr uint64) bool {
	for _, e := range m.entries {
		if e.pid == pid && e.vAddr == vAddr {
			return true
		}
	}

	return false
}

func (m *mshrImpl) IsEmpty() bool {
	return len(m.entries) == 0
}
