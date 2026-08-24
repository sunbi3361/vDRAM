package driver

import (
	"fmt"
	"sort"
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
)

// UVMManager owns the functional UVM demand-paging state machine: residency,
// faults, coalescing, TBN selection, capacity enforcement, eviction,
// migration, access counters, and statistics. It is owned by the Driver and
// driven by scheduled Akita events so that the fixed fault latency never
// blocks the simulation engine.
//
// sbin_codex: the manager now carries the validated UVMConfig and the
// resolved GPU capacity so later todos (TBN, capacity enforcement, eviction)
// can consult them without re-deriving configuration.
// type UVMManager struct {
//
//		sync.Mutex
//	}
type UVMManager struct {
	sync.Mutex

	config   UVMConfig // sbin_codex: validated UVM configuration.
	capacity uint64    // sbin_codex: resolved UVM GPU capacity in bytes.

	// sbin_codex: registered managed allocations; a registration is appended
	// only after its boundaries and masks are fully built (atomic visibility).
	registrations []*ManagedAllocationRegistration

	// sbin_codex (todo 4): GPU capacity reservation tracker (R+I+N <= C).
	reservation *AdmissionReservation

	// sbin_codex (todo 10): publication generation counter. The manager
	// increments it before every mapping publication; the virtual access
	// gates stamp it into GPU_LOCAL annotations so stale retries can be
	// detected.
	generation uint64

	// sbin_codex (todo 5): the shared ownership table keyed by
	// (PID, GPU, regionBase). Copies, faults, migrations, prefetches, and
	// evictions all claim slots here; a slot is idle or owned by exactly one
	// transaction, and waiters never hold a slot.
	ownership map[copyRegionKey]*OwnershipEntry

	// sbin_codex (todo 5): the global monotonically increasing copy ticket
	// counter. Each copy request obtains exactly one ticket.
	nextCopyTicket uint64

	// sbin_codex (todo 5): the ticket-ordered queue of copies waiting to
	// claim their whole key set. A copy is enqueued once and re-evaluated
	// after any release.
	copyWaiters []*copyTransaction
}

// NewUVMManager constructs a UVM manager for an enabled UVM configuration.
// availableGPUMemory is the total GPU DRAM the allocator can back; the
// resolved capacity is the explicit -uvm-gpu-memory-capacity when set,
// otherwise the full available GPU memory. sbin_codex
func NewUVMManager(cfg UVMConfig, availableGPUMemory uint64) *UVMManager {
	return &UVMManager{
		config:      cfg,
		capacity:    cfg.ResolvedCapacity(availableGPUMemory),
		reservation: NewAdmissionReservation(cfg.ResolvedCapacity(availableGPUMemory)),
		ownership:   make(map[copyRegionKey]*OwnershipEntry), // sbin_codex (todo 5)
	}
}

// Reservation returns the manager's GPU capacity reservation tracker.
// sbin_codex (todo 4)
func (m *UVMManager) Reservation() *AdmissionReservation {
	m.Lock()
	defer m.Unlock()

	return m.reservation
}

// Generation returns the current publication generation. // sbin_codex
func (m *UVMManager) Generation() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.generation
}

// IncrementGeneration advances the publication generation and returns the new
// value. The manager calls it before every mapping publication so the gates
// can detect stale annotations. // sbin_codex
func (m *UVMManager) IncrementGeneration() uint64 {
	m.Lock()
	defer m.Unlock()

	m.generation++
	return m.generation
}

// RegisterManagedAllocation validates an allocator result and atomically
// records the managed allocation's boundaries and per-page masks. The record
// is complete before it becomes visible; on validation failure no state is
// mutated and the caller must roll the allocation back.
func (m *UVMManager) RegisterManagedAllocation(
	pid vm.PID,
	res internal.ManagedAllocationResult,
) error {
	m.Lock()
	defer m.Unlock()

	reg, err := newManagedAllocationRegistration(pid, res)
	if err != nil {
		return err
	}

	m.registrations = append(m.registrations, reg)
	return nil
}

// RegistrationCount returns the number of registered managed allocations.
func (m *UVMManager) RegistrationCount() int {
	m.Lock()
	defer m.Unlock()

	return len(m.registrations)
}

// newManagedAllocationRegistration validates an allocator result and builds
// the complete boundary + mask record. Any validation failure returns an
// error without producing a record, so a failed registration never becomes
// visible. sbin_codex
func newManagedAllocationRegistration(
	pid vm.PID,
	res internal.ManagedAllocationResult,
) (*ManagedAllocationRegistration, error) {
	if res.Size == 0 {
		return nil, fmt.Errorf("uvm: managed allocation size is 0")
	}
	if res.PageSize != basePageSize {
		return nil, fmt.Errorf(
			"uvm: managed allocation page size %d != base page %d",
			res.PageSize, basePageSize)
	}
	if want := (res.Size-1)/res.PageSize + 1; res.PageCount != want {
		return nil, fmt.Errorf(
			"uvm: managed allocation page count %d != %d",
			res.PageCount, want)
	}
	if res.PageCount != uint64(len(res.CPUBackingPages)) {
		return nil, fmt.Errorf(
			"uvm: managed allocation has %d CPU backing pages for %d pages",
			len(res.CPUBackingPages), res.PageCount)
	}
	if res.Base == 0 {
		return nil, fmt.Errorf("uvm: managed allocation base address is 0")
	}

	numWords := (res.PageCount + 63) / 64
	reg := &ManagedAllocationRegistration{
		PID:          pid,
		Base:         res.Base,
		Size:         res.Size,
		PageCount:    res.PageCount,
		PageSize:     res.PageSize,
		ResidentMask: make([]uint64, numWords),
		InFlightMask: make([]uint64, numWords),
		DirtyMask:    make([]uint64, numWords),
		ValidMask:    make([]uint64, numWords),
		// sbin_codex (todo 4): copy the CPU backing frames so the VA-block
		// model can publish per-page CPU physical addresses.
		CPUBackingPages: append([]uint64(nil), res.CPUBackingPages...),
	}
	for w := uint64(0); w < numWords; w++ {
		bits := res.PageCount - w*64
		if bits > 64 {
			bits = 64
		}
		reg.ValidMask[w] = (uint64(1) << bits) - 1
	}
	// sbin_codex (todo 4): build the 2 MB VA-block model over the masks.
	reg.VABlocks = buildVABlocks(reg)
	return reg, nil
}

// copyRegionKey identifies one ownership-table slot: a 64 KB region on one
// GPU for one process. sbin_codex (todo 5)
type copyRegionKey struct {
	PID        vm.PID
	GPU        int // 1-based GPU ID (device 0 is the CPU)
	RegionBase uint64
}

// ownershipFor returns the ownership slot for key, creating it when absent.
// The caller must hold the manager lock. sbin_codex (todo 5)
func (m *UVMManager) ownershipFor(key copyRegionKey) *OwnershipEntry {
	e, ok := m.ownership[key]
	if !ok {
		e = &OwnershipEntry{}
		m.ownership[key] = e
	}
	return e
}

// AcquireOwnership claims key for ownerID of ownerType when the slot is idle.
// It returns false without mutating anything when the slot is busy. The same
// table serves copies, faults, migrations, prefetches, and evictions. // sbin_codex
func (m *UVMManager) AcquireOwnership(
	key copyRegionKey,
	ownerType OwnershipType,
	ownerID uint64,
) bool {
	m.Lock()
	defer m.Unlock()

	e := m.ownershipFor(key)
	if e.OwnerType != OwnershipIdle {
		return false
	}
	e.OwnerType = ownerType
	e.OwnerID = ownerID
	return true
}

// ReleaseOwnership releases key when it is owned by ownerID. A release of a
// slot owned by another transaction is a no-op. // sbin_codex
func (m *UVMManager) ReleaseOwnership(key copyRegionKey, ownerID uint64) {
	m.Lock()
	defer m.Unlock()

	e := m.ownershipFor(key)
	if e.OwnerID == ownerID {
		e.OwnerType = OwnershipIdle
		e.OwnerID = 0
	}
}

// OwnerOf returns the current owner type and ID of key. // sbin_codex
func (m *UVMManager) OwnerOf(key copyRegionKey) (OwnershipType, uint64) {
	m.Lock()
	defer m.Unlock()

	e := m.ownershipFor(key)
	return e.OwnerType, e.OwnerID
}

// IsKeyIdle reports whether key has no owner. // sbin_codex
func (m *UVMManager) IsKeyIdle(key copyRegionKey) bool {
	m.Lock()
	defer m.Unlock()

	return m.ownershipFor(key).OwnerType == OwnershipIdle
}

// NextCopyTicket returns the next global monotonically increasing copy
// ticket. Each copy request obtains exactly one ticket. // sbin_codex
func (m *UVMManager) NextCopyTicket() uint64 {
	m.Lock()
	defer m.Unlock()

	m.nextCopyTicket++
	return m.nextCopyTicket
}

// claimCopy atomically claims every key of tx as one COPY transaction when
// every key is idle; otherwise it claims none. The caller must hold the
// manager lock. // sbin_codex
func (m *UVMManager) claimCopyLocked(tx *copyTransaction) bool {
	for _, key := range tx.Keys {
		if m.ownershipFor(key).OwnerType != OwnershipIdle {
			return false
		}
	}
	for _, key := range tx.Keys {
		e := m.ownershipFor(key)
		e.OwnerType = OwnershipCopy
		e.OwnerID = tx.Ticket
	}
	tx.claimed = true
	return true
}

// claimCopy claims all keys of tx under one manager lock. // sbin_codex
func (m *UVMManager) claimCopy(tx *copyTransaction) bool {
	m.Lock()
	defer m.Unlock()

	return m.claimCopyLocked(tx)
}

// enqueueCopy registers tx once in the ticket-ordered waiting queue. A copy
// that fails to claim never re-enqueues. // sbin_codex
func (m *UVMManager) enqueueCopy(tx *copyTransaction) {
	m.Lock()
	defer m.Unlock()

	if tx.enqueued {
		return
	}
	tx.enqueued = true
	m.copyWaiters = append(m.copyWaiters, tx)
}

// releaseCopyKeys atomically releases every key of tx under one manager lock.
// When wake is true the waiting queue is re-evaluated in ticket order. // sbin_codex
func (m *UVMManager) releaseCopyKeys(tx *copyTransaction, wake bool) {
	m.Lock()
	defer m.Unlock()

	for _, key := range tx.Keys {
		e := m.ownershipFor(key)
		if e.OwnerID == tx.Ticket {
			e.OwnerType = OwnershipIdle
			e.OwnerID = 0
		}
	}
	if wake {
		m.reevaluateLocked()
	}
}

// wakeTickets re-evaluates the whole waiting set after any release. // sbin_codex
func (m *UVMManager) wakeTickets() {
	m.Lock()
	defer m.Unlock()

	m.reevaluateLocked()
}

// reevaluateLocked scans the waiting copies in ticket order and claims each
// whose whole key set is idle; a claimed copy leaves the queue. // sbin_codex
func (m *UVMManager) reevaluateLocked() {
	remaining := m.copyWaiters[:0]
	for _, tx := range m.copyWaiters {
		if m.claimCopyLocked(tx) {
			continue
		}
		remaining = append(remaining, tx)
	}
	m.copyWaiters = remaining
}

// classifySpan reports whether the whole span [startVA, startVA+size) lies
// inside managed allocations of pid. A span that is partly managed and partly
// not (or has VA gaps) is rejected with an error. // sbin_codex
func (m *UVMManager) classifySpan(
	pid vm.PID,
	startVA, size uint64,
) (allManaged bool, err error) {
	end := startVA + size
	managed := 0
	total := 0
	for pageVA := startVA &^ (basePageSize - 1); pageVA < end; pageVA += basePageSize {
		total++
		if m.registrationForPage(pid, pageVA) != nil {
			managed++
		}
	}
	if total == 0 || managed == 0 {
		return false, nil
	}
	if managed == total {
		return true, nil
	}
	return false, fmt.Errorf(
		"uvm: copy span %#x+%d is mixed managed/unmanaged (%d/%d pages)",
		startVA, size, managed, total)
}

// copyKeysForSpan returns the sorted unique (PID, GPU, regionBase) ownership
// keys covering the whole span. // sbin_codex
func (m *UVMManager) copyKeysForSpan(
	pid vm.PID,
	gpu int,
	startVA, size uint64,
) []copyRegionKey {
	seen := make(map[copyRegionKey]bool)
	end := startVA + size
	for pageVA := startVA &^ (basePageSize - 1); pageVA < end; pageVA += basePageSize {
		seen[copyRegionKey{
			PID:        pid,
			GPU:        gpu,
			RegionBase: SubBlockStartVA(pageVA),
		}] = true
	}
	keys := make([]copyRegionKey, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].PID != keys[j].PID {
			return keys[i].PID < keys[j].PID
		}
		if keys[i].GPU != keys[j].GPU {
			return keys[i].GPU < keys[j].GPU
		}
		return keys[i].RegionBase < keys[j].RegionBase
	})
	return keys
}

// registrationForPage returns the registration of pid whose VA range covers
// pageVA, or nil. // sbin_codex
func (m *UVMManager) registrationForPage(
	pid vm.PID,
	pageVA uint64,
) *ManagedAllocationRegistration {
	m.Lock()
	defer m.Unlock()

	return m.registrationForPageLocked(pid, pageVA)
}

// registrationForPageLocked is registrationForPage under the manager lock. // sbin_codex
func (m *UVMManager) registrationForPageLocked(
	pid vm.PID,
	pageVA uint64,
) *ManagedAllocationRegistration {
	for _, reg := range m.registrations {
		if reg.PID != pid {
			continue
		}
		if pageVA >= reg.Base && pageVA < reg.Base+reg.PageCount*reg.PageSize {
			return reg
		}
	}
	return nil
}

// managedPageInfo describes the managed state of one 4 KB page for the copy
// data path. // sbin_codex
type managedPageInfo struct {
	CPUPhysicalPage uint64
	GPUPhysicalPage uint64
	Resident        bool
}

// pageInfo returns the managed page detail for pageVA, or ok=false when the
// page is not managed. // sbin_codex
func (m *UVMManager) pageInfo(
	pid vm.PID,
	pageVA uint64,
) (info managedPageInfo, ok bool) {
	m.Lock()
	defer m.Unlock()

	reg := m.registrationForPageLocked(pid, pageVA)
	if reg == nil {
		return managedPageInfo{}, false
	}
	allocPage := (pageVA - reg.Base) / basePageSize
	blockIdx := (BlockForVA(pageVA) - BlockForVA(reg.Base)) / vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	blockLocal := (pageVA - block.StartVA) / basePageSize
	p := &block.Pages[blockLocal]
	return managedPageInfo{
		CPUPhysicalPage: p.CPUPhysicalPage,
		GPUPhysicalPage: p.GPUPhysicalPage,
		Resident:        maskBit(reg.ResidentMask, allocPage),
	}, true
}

// forEachSpanPage visits every managed 4 KB page overlapping
// [startVA, startVA+size) in VA order, with the page's byte offset and
// overlap length within the span. // sbin_codex
func (m *UVMManager) forEachSpanPage(
	pid vm.PID,
	startVA, size uint64,
	fn func(va, offset, length uint64, info managedPageInfo),
) {
	end := startVA + size
	for pageVA := startVA &^ (basePageSize - 1); pageVA < end; pageVA += basePageSize {
		lo := pageVA
		if lo < startVA {
			lo = startVA
		}
		hi := pageVA + basePageSize
		if hi > end {
			hi = end
		}
		info, ok := m.pageInfo(pid, pageVA)
		if ok {
			fn(pageVA, lo-startVA, hi-lo, info)
		}
	}
}
