package tlb

// latcMSHR is LATPC's compressed TLB MSHR (MICRO'25 §5.3, Figure 14,
// Algorithm 1; refs/latpc-plan.md 1.3). One group entry represents up to 32
// outstanding misses of one Regularity-Detector group with
// <Base VAddr, Stride, 32-bit Valid Mask>; the i-th set bit means the VPN at
// Base + Stride*i is in flight. Capacity is counted in group entries, as in
// the paper.
//
// Each occupied mask bit owns a plain *mshrEntry ("subentry") holding that
// VPN's waiting requests, its request to the bottom, and its returned page.
// Because the subentry is the same type the classic MSHR stores, the fill
// path (parseBottom), the response paths, and the cancel path all operate on
// subentries unchanged; only admission (handleTranslationMiss / fetchBottom)
// needs LATC-specific branches. // sbin_claude_latpc

import (
	"log"
	"math/bits"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// latcSubentryCount is the paper's 32-bit valid mask width.
const latcSubentryCount = 32

type latcGroup struct {
	pid       vm.PID
	baseVAddr uint64
	// stridePages is the group's stride in pages. It is 0 while the group
	// holds only its demand request and is adopted from the first prefetch
	// member that joins (Algorithm 1 line 4's "E.Stride = 0" arm).
	stridePages int64
	validMask   uint32
	subentries  [latcSubentryCount]*mshrEntry
}

type latcMSHR struct {
	capacity int
	pageSize uint64
	groups   []*latcGroup

	groupsAllocated     uint64
	coalescedSubentries uint64
}

// newLATCMSHR returns a compressed MSHR with the given group-entry capacity.
func newLATCMSHR(capacity int, pageSize uint64) *latcMSHR {
	if pageSize == 0 {
		panic("LATC MSHR requires a non-zero page size")
	}

	return &latcMSHR{
		capacity: capacity,
		pageSize: pageSize,
	}
}

// subentryIndexOf returns the mask index a request claims within its group.
// A request without usable group information (or an out-of-range index) is
// treated as a demand at index 0 of its own group.
func subentryIndexOf(req *vm.TranslationReq) int {
	if req.GroupStride == 0 ||
		req.GroupIndex < 0 || req.GroupIndex >= latcSubentryCount {
		return 0
	}

	return req.GroupIndex
}

// baseOf computes the group base VAddr the request claims:
// vAddr - Stride*Index pages.
func (m *latcMSHR) baseOf(req *vm.TranslationReq) uint64 {
	index := subentryIndexOf(req)
	if index == 0 {
		return req.VAddr
	}

	return uint64(int64(req.VAddr) -
		req.GroupStride*int64(index)*int64(m.pageSize))
}

// findJoinable returns the group the request's miss can be compressed into:
// same PID and base, a matching (or not yet adopted) stride, and a free
// subentry slot at the request's index. Demands never join - a demand's VPN
// that is already in flight is caught earlier as an MSHR hit via GetEntry.
func (m *latcMSHR) findJoinable(req *vm.TranslationReq) *latcGroup {
	index := subentryIndexOf(req)
	if index == 0 {
		return nil
	}

	base := m.baseOf(req)
	for _, g := range m.groups {
		if g.pid != req.PID || g.baseVAddr != base {
			continue
		}
		if g.stridePages != req.GroupStride && g.stridePages != 0 {
			continue
		}
		if g.subentries[index] != nil {
			continue
		}

		return g
	}

	return nil
}

// CanAccept reports whether a miss for the request can be tracked - by
// joining an existing group or by allocating a new entry. When it cannot,
// the access experiences an MSHR reservation failure.
func (m *latcMSHR) CanAccept(req *vm.TranslationReq) bool {
	return m.findJoinable(req) != nil || len(m.groups) < m.capacity
}

// AddCompressed tracks a new outstanding miss per Algorithm 1: join the
// matching group when one exists, otherwise allocate a new group entry with
// the request at its own index. Callers must check CanAccept first.
func (m *latcMSHR) AddCompressed(req *vm.TranslationReq) *mshrEntry {
	if m.GetEntry(req.PID, req.VAddr) != nil {
		panic("entry already in LATC mshr")
	}

	entry := newMSHREntry()
	entry.pid = req.PID
	entry.vAddr = req.VAddr

	index := subentryIndexOf(req)

	if g := m.findJoinable(req); g != nil {
		if g.stridePages == 0 {
			g.stridePages = req.GroupStride
		}
		g.subentries[index] = entry
		g.validMask |= 1 << uint(index)
		m.coalescedSubentries++

		return entry
	}

	if len(m.groups) >= m.capacity {
		log.Panic("LATC MSHR is full")
	}

	g := &latcGroup{
		pid:         req.PID,
		baseVAddr:   m.baseOf(req),
		stridePages: req.GroupStride,
	}
	g.subentries[index] = entry
	g.validMask |= 1 << uint(index)
	m.groups = append(m.groups, g)
	m.groupsAllocated++

	return entry
}

// Add allocates a demand-style entry for callers without a request context
// (mshr interface parity). The entry forms its own single-VPN group.
func (m *latcMSHR) Add(pid vm.PID, vAddr uint64) *mshrEntry {
	if m.GetEntry(pid, vAddr) != nil {
		panic("entry already in LATC mshr")
	}
	if len(m.groups) >= m.capacity {
		log.Panic("LATC MSHR is full")
	}

	entry := newMSHREntry()
	entry.pid = pid
	entry.vAddr = vAddr

	g := &latcGroup{pid: pid, baseVAddr: vAddr}
	g.subentries[0] = entry
	g.validMask = 1
	m.groups = append(m.groups, g)
	m.groupsAllocated++

	return entry
}

func (m *latcMSHR) AddInTLB(
	pid vm.PID,
	vAddr uint64,
	setID, wayID int,
) *mshrEntry {
	entry := m.Add(pid, vAddr)
	entry.inTLB = true
	entry.setID = setID
	entry.wayID = wayID

	return entry
}

func (m *latcMSHR) InTLBCount() int {
	count := 0
	for _, entry := range m.AllEntries() {
		if entry.inTLB {
			count++
		}
	}

	return count
}

func (m *latcMSHR) find(pid vm.PID, vAddr uint64) (*latcGroup, int) {
	for _, g := range m.groups {
		if g.pid != pid {
			continue
		}
		for i, sub := range g.subentries {
			if sub != nil && sub.vAddr == vAddr {
				return g, i
			}
		}
	}

	return nil, 0
}

func (m *latcMSHR) GetEntry(pid vm.PID, vAddr uint64) *mshrEntry {
	g, i := m.find(pid, vAddr)
	if g == nil {
		return nil
	}

	return g.subentries[i]
}

func (m *latcMSHR) IsEntryPresent(pid vm.PID, vAddr uint64) bool {
	g, _ := m.find(pid, vAddr)
	return g != nil
}

// Remove releases one subentry; the group entry itself is freed once its
// valid mask reaches zero (Algorithm 1's Erase).
func (m *latcMSHR) Remove(pid vm.PID, vAddr uint64) *mshrEntry {
	g, i := m.find(pid, vAddr)
	if g == nil {
		panic("trying to remove an non-exist entry")
	}

	entry := g.subentries[i]
	g.subentries[i] = nil
	g.validMask &^= 1 << uint(i)

	if g.validMask == 0 {
		for k, other := range m.groups {
			if other == g {
				m.groups = append(m.groups[:k], m.groups[k+1:]...)
				break
			}
		}
	}

	return entry
}

func (m *latcMSHR) AllEntries() []*mshrEntry {
	var entries []*mshrEntry
	for _, g := range m.groups {
		for _, sub := range g.subentries {
			if sub != nil {
				entries = append(entries, sub)
			}
		}
	}

	return entries
}

func (m *latcMSHR) IsFull() bool {
	return len(m.groups) >= m.capacity
}

func (m *latcMSHR) Reset() {
	m.groups = nil
}

func (m *latcMSHR) IsEmpty() bool {
	return len(m.groups) == 0
}

// OutstandingMisses counts the in-flight VPNs across all group entries.
func (m *latcMSHR) OutstandingMisses() int {
	total := 0
	for _, g := range m.groups {
		total += bits.OnesCount32(g.validMask)
	}

	return total
}
