package driver

import (
	"fmt"
	"sort"
	"sync"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim" // sbin_codex (todo 15): fault service region transitions.
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

	// sbin_codex (todo 15): the fault coalescing table: one live fault-service
	// transaction per (PID, GPU, regionBase). An entry exists from the first
	// raw fault of a region until the transaction completes after replay.
	faultByKey map[copyRegionKey]*faultTransaction

	// sbin_codex (todo 15): the per-episode replay token counter. The GMMU
	// assigns a region replay token per fault episode; the driver mirrors it
	// per transaction (the PageFaultReq envelope carries no ReplayToken yet —
	// the correlation is a later-todo concern).
	nextReplayToken vm.ReplayToken

	// sbin_codex (todo 18): the AC/write migration coalescing table: one live
	// migration transaction per (PID, GPU, regionBase). An entry exists from
	// the first notification/write trigger of a region until the transaction
	// completes after the unblock; a later trigger is suppressed (§16).
	migrationByKey map[copyRegionKey]*migrationTransaction

	// sbin_codex (todo 18): migration statistics (uvm-manager.md §16, §31.1).
	// The trigger-specific counters record created transactions; the
	// suppressed counter records ignored notifications/writes (§16).
	accessCounterMigrationCount uint64
	remoteWriteMigrationCount   uint64
	suppressedMigrationCount    uint64

	// sbin_codex (todo 15): fault statistics (uvm-manager.md §8.4).
	rawPageFaultCount       uint64
	coalescedPageFaultCount uint64
	uniqueFaultServiceCount uint64

	// sbin_codex (todo 16): the migration destination frame allocator (the
	// driver), installed after construction. prepareFaultMigration allocates
	// destination PFNs through it and rollbackFaultMigration returns them.
	frames migrationFrameAllocator

	// sbin_codex (todo 17): TBN selection statistics (uvm-manager.md §11.12),
	// recorded by recomputeTBN at every fault service.
	tbnStats tbnStatistics

	// sbin_codex (todo 19): the eviction coalescing table: one live eviction
	// transaction per (PID, GPU, regionBase). An entry exists from the victim
	// selection until the transaction completes after the unblock; a region is
	// never selected twice (§18.2).
	evictByKey map[copyRegionKey]*evictionTransaction

	// sbin_codex (todo 19): the pinned-region registry (uvm-manager.md §18.2:
	// a victim must not be pinned). No pin API exists in the initial model;
	// the registry is exercised by the PinnedExclusion contract test.
	pinned map[copyRegionKey]bool

	// sbin_codex (todo 20): the projected-occupancy pre-eviction statistics
	// (uvm-manager.md §17.1): num_pre_evictions, bytes_pre_evicted,
	// num/max_concurrent_pre_evictions, num_pre_evictions_overlapped_with_h2d,
	// migration_wait_cycles_for_capacity, and the optional-headroom shortfall
	// diagnostic. Todo 22 exposes them through the reporter.
	preEviction preEvictionStats
}

// NewUVMManager constructs a UVM manager for an enabled UVM configuration.
// availableGPUMemory is the total GPU DRAM the allocator can back; the
// resolved capacity is the explicit -uvm-gpu-memory-capacity when set,
// otherwise the full available GPU memory. sbin_codex
func NewUVMManager(cfg UVMConfig, availableGPUMemory uint64) *UVMManager {
	// sbin_codex (todo 20): runtime capacity enforcement — the
	// projected-occupancy policy requires a page-aligned capacity of at least
	// 64 KB bounded by the available GPU DRAM/frames (validated at config
	// time in todo 1; enforced defensively here).
	capacity := cfg.ResolvedCapacity(availableGPUMemory)
	if capacity < preEvictionHeadroomBytes {
		panic(fmt.Sprintf(
			"uvm: GPU memory capacity %d must be >= 64KB", capacity))
	}
	if capacity%basePageSize != 0 {
		panic(fmt.Sprintf(
			"uvm: GPU memory capacity %d must be 4KB-aligned", capacity))
	}
	if capacity > availableGPUMemory {
		panic(fmt.Sprintf(
			"uvm: GPU memory capacity %d exceeds available GPU memory %d",
			capacity, availableGPUMemory))
	}
	return &UVMManager{
		config:      cfg,
		capacity:    capacity,
		reservation: NewAdmissionReservation(capacity),
		ownership:   make(map[copyRegionKey]*OwnershipEntry),   // sbin_codex (todo 5)
		faultByKey:  make(map[copyRegionKey]*faultTransaction), // sbin_codex (todo 15)
		// sbin_codex (todo 18): the AC/write migration coalescing table.
		migrationByKey: make(map[copyRegionKey]*migrationTransaction),
		// sbin_codex (todo 19): the eviction coalescing table and the
		// pinned-region registry.
		evictByKey: make(map[copyRegionKey]*evictionTransaction),
		pinned:     make(map[copyRegionKey]bool),
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
		// sbin_codex (todo 17): the TBN prefetch-provenance mask (uvm-manager.md
		// §11.11) starts empty: no page is prefetched at allocation.
		PrefetchedMask: make([]uint64, numWords),
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

	return m.nextTicketLocked()
}

// nextTicketLocked returns the next global ticket under the manager lock.
// Fault transactions share the same ticket space so ticket order is the
// global creation order across copies and faults. // sbin_codex
func (m *UVMManager) nextTicketLocked() uint64 {
	m.nextCopyTicket++
	return m.nextCopyTicket
}

// nextReplayTokenLocked returns the next per-episode replay token under the
// manager lock. // sbin_codex
func (m *UVMManager) nextReplayTokenLocked() vm.ReplayToken {
	m.nextReplayToken++
	return m.nextReplayToken
}

// setMaskBit sets or clears bit `page` of a registration mask. // sbin_codex
func setMaskBit(mask []uint64, page uint64, on bool) {
	if on {
		mask[page/64] |= uint64(1) << (page % 64)
	} else {
		mask[page/64] &^= uint64(1) << (page % 64)
	}
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

// intakePageFault consumes one raw 4 KB fault request: it counts the request,
// coalesces it into the live transaction of its 64 KB region when one exists,
// or creates the region's first unique fault-service transaction. A fault on
// an unmanaged address or in an illegal region state is rejected before any
// transaction state is created. // sbin_codex
func (m *UVMManager) intakePageFault(
	pid vm.PID,
	gpu int,
	vaddr uint64,
) (tx *faultTransaction, isNew bool, err error) {
	m.Lock()
	defer m.Unlock()

	m.rawPageFaultCount++

	reg := m.registrationForPageLocked(pid, vaddr)
	if reg == nil {
		return nil, false, fmt.Errorf(
			"uvm: fault on unmanaged address pid=%d va=%#x", pid, vaddr)
	}

	key := copyRegionKey{PID: pid, GPU: gpu, RegionBase: SubBlockStartVA(vaddr)}
	if tx := m.faultByKey[key]; tx != nil {
		m.coalescedPageFaultCount++
		return tx, false, nil
	}

	sm := m.faultRegionMachineLocked(reg, gpu, key.RegionBase)
	switch sm.Region.State {
	case RegionIDLE, RegionCPUResident:
		if err := sm.Transition(RegionFaultPending, 0); err != nil {
			return nil, false, err
		}
	case RegionFaultPending, RegionMigratingToGPU:
		// A migration without a fault transaction (access-counter migration /
		// prefetch) is in flight; the new transaction coalesces into it and
		// the service re-reads residency from the current masks.
	default:
		return nil, false, fmt.Errorf(
			"uvm: fault in illegal region state %s (pid=%d gpu=%d region=%#x)",
			sm.Region.State, pid, gpu, key.RegionBase)
	}

	m.uniqueFaultServiceCount++
	tx = &faultTransaction{
		Ticket:      m.nextTicketLocked(),
		PID:         pid,
		GPU:         gpu,
		RegionBase:  key.RegionBase,
		Key:         key,
		DemandPages: m.demandPagesLocked(reg, key.RegionBase),
		ReplayToken: m.nextReplayTokenLocked(),
		reg:         reg,
	}
	m.faultByKey[key] = tx
	return tx, true, nil
}

// RawPageFaultCount returns the number of raw 4 KB fault requests consumed
// (uvm-manager.md §8.4). // sbin_codex
func (m *UVMManager) RawPageFaultCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.rawPageFaultCount
}

// CoalescedPageFaultCount returns the number of raw faults attached to an
// existing transaction of their 64 KB region. // sbin_codex
func (m *UVMManager) CoalescedPageFaultCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.coalescedPageFaultCount
}

// UniqueFaultServiceCount returns the number of unique fault-service
// transactions created (one per 64 KB region per episode). // sbin_codex
func (m *UVMManager) UniqueFaultServiceCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.uniqueFaultServiceCount
}

// faultRegionMachineLocked binds a region state machine to the 64 KB region
// containing regionBase of reg. The caller must hold the manager lock. // sbin_codex
func (m *UVMManager) faultRegionMachineLocked(
	reg *ManagedAllocationRegistration,
	gpu int,
	regionBase uint64,
) *RegionStateMachine {
	blockIdx := (BlockForVA(regionBase) - BlockForVA(reg.Base)) / vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (regionBase - block.StartVA) / subblockSizeBytes
	return NewRegionStateMachine(
		RegionContext{PID: reg.PID, GPU: gpu, Block: blockIdx, Region: regionIdx},
		block.SubBlocks[regionIdx])
}

// demandPagesLocked returns the allocation page indices of the 64 KB region's
// valid pages: the demand set of a fault transaction. The caller must hold
// the manager lock. // sbin_codex
func (m *UVMManager) demandPagesLocked(
	reg *ManagedAllocationRegistration,
	regionBase uint64,
) []uint64 {
	blockIdx := (BlockForVA(regionBase) - BlockForVA(reg.Base)) / vablockSizeBytes
	block := reg.VABlocks[blockIdx]
	regionIdx := (regionBase - block.StartVA) / subblockSizeBytes
	allocStart, valid := (&InvariantContext{
		Reg: reg, Block: block, RegionIdx: regionIdx,
	}).regionPageRange()
	pages := make([]uint64, 0, valid)
	for i := uint64(0); i < valid; i++ {
		pages = append(pages, allocStart+i)
	}
	return pages
}

// missingDemandPages returns the transaction's demand pages that are neither
// GPU-resident nor in flight: the pages a new migration must transfer. // sbin_codex
// sbin_codex (todo 18): delegates to the generic page-based missingPages so
// AC/write migration transactions reuse the same residency re-read.
func (m *UVMManager) missingDemandPages(tx *faultTransaction) []uint64 {
	return m.missingPages(tx.reg, tx.DemandPages)
}

// missingPages returns the demand pages that are neither GPU-resident nor in
// flight: the pages a new migration must transfer. // sbin_codex
func (m *UVMManager) missingPages(
	reg *ManagedAllocationRegistration,
	demand []uint64,
) []uint64 {
	m.Lock()
	defer m.Unlock()

	if reg == nil {
		return nil
	}
	missing := make([]uint64, 0, len(demand))
	for _, page := range demand {
		if !maskBit(reg.ResidentMask, page) && !maskBit(reg.InFlightMask, page) {
			missing = append(missing, page)
		}
	}
	return missing
}

// prepareFaultMigration marks the missing pages in flight and returns their
// transfer descriptors (CPU source, HBM destination). // sbin_codex
// sbin_codex (todo 16): superseded by the maximal-run implementation in
// uvm_dma.go (reservation -> destination frame allocation -> run formation).
// func (m *UVMManager) prepareFaultMigration(
// 	tx *faultTransaction,
// 	missing []uint64,
// ) ([]faultMigrationPage, error) {
// 	m.Lock()
// 	defer m.Unlock()
//
// 	reg := tx.reg
// 	if reg == nil {
// 		return nil, fmt.Errorf("uvm: fault migration without a registration")
// 	}
// 	pages := make([]faultMigrationPage, 0, len(missing))
// 	for _, page := range missing {
// 		blockIdx := (BlockForVA(reg.Base+page*basePageSize) -
// 			BlockForVA(reg.Base)) / vablockSizeBytes
// 		block := reg.VABlocks[blockIdx]
// 		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
// 		p := &block.Pages[blockLocal]
// 		if p.GPUPhysicalPage == 0 {
// 			return nil, fmt.Errorf(
// 				"uvm: fault migration page %d has no GPU physical page", page)
// 		}
// 		setMaskBit(reg.InFlightMask, page, true)
// 		pages = append(pages, faultMigrationPage{
// 			PageVA:  reg.Base + page*basePageSize,
// 			CPUPage: p.CPUPhysicalPage,
// 			GPUPage: p.GPUPhysicalPage,
// 		})
// 	}
// 	return pages, nil
// }

// commitFaultMigration publishes GPU residency for the migrated pages and
// returns their (VA, HBM PA) pairs for the GPU PTE publication. // sbin_codex
// sbin_codex (todo 16): superseded by the plan-based implementation in
// uvm_dma.go (commits only after ALL runs succeed).
// func (m *UVMManager) commitFaultMigration(
// 	tx *faultTransaction,
// ) ([]faultMigratedPage, error) {
// 	m.Lock()
// 	defer m.Unlock()
//
// 	reg := tx.reg
// 	if reg == nil {
// 		return nil, fmt.Errorf("uvm: fault migration commit without a registration")
// 	}
// 	pages := make([]faultMigratedPage, 0, len(tx.missingPages))
// 	for _, page := range tx.missingPages {
// 		setMaskBit(reg.ResidentMask, page, true)
// 		setMaskBit(reg.InFlightMask, page, false)
// 		blockIdx := (BlockForVA(reg.Base+page*basePageSize) -
// 			BlockForVA(reg.Base)) / vablockSizeBytes
// 		block := reg.VABlocks[blockIdx]
// 		blockLocal := (reg.Base + page*basePageSize - block.StartVA) / basePageSize
// 		pages = append(pages, faultMigratedPage{
// 			PageVA:  reg.Base + page*basePageSize,
// 			GPUPage: block.Pages[blockLocal].GPUPhysicalPage,
// 		})
// 	}
// 	return pages, nil
// }

// beginFaultMigration reserves admission for the migrated bytes and
// transitions the fault region to MIGRATING_TO_GPU. // sbin_codex
// sbin_codex (todo 17): the transition now covers every region touched by
// the migration: the fault region advances from FAULT_PENDING and each
// TBN-prefetched region admits without a pending fault (§23
// IDLE/CPU_RESIDENT -> MIGRATING_TO_GPU).
// func (m *UVMManager) beginFaultMigration(
//
//	tx *faultTransaction,
//	bytes uint64,
//	now sim.VTimeInSec,
//
//	) error {
//		m.Lock()
//		defer m.Unlock()
//
//		// sbin_codex (todo 16): the admission reservation now happens in
//		// prepareFaultMigration BEFORE any DMA is emitted (uvm-manager.md §17.1
//		// "Reserve required GPU pages before H2D"); beginFaultMigration only
//		// performs the FAULT_PENDING -> MIGRATING_TO_GPU transition.
//		// if err := m.reservation.ReserveAdmission(bytes); err != nil {
//		// 	return err
//		// }
//		reg := tx.reg
//		if reg == nil {
//			return fmt.Errorf("uvm: fault migration without a registration")
//		}
//		sm := m.faultRegionMachineLocked(reg, tx.GPU, tx.RegionBase)
//		switch sm.Region.State {
//		case RegionFaultPending:
//			if err := sm.Transition(RegionMigratingToGPU, now); err != nil {
//				return err
//			}
//		case RegionMigratingToGPU:
//			// An earlier migration (prefetch / access-counter) is already in
//			// flight; this transaction's DMA joins it.
//		default:
//			return fmt.Errorf(
//				"uvm: fault service in illegal region state %s", sm.Region.State)
//		}
//		return nil
//	}
func (m *UVMManager) beginFaultMigration(
	tx *faultTransaction,
	bytes uint64,
	now sim.VTimeInSec,
) error {
	m.Lock()
	defer m.Unlock()

	// sbin_codex (todo 16): the admission reservation now happens in
	// prepareFaultMigration BEFORE any DMA is emitted (uvm-manager.md §17.1
	// "Reserve required GPU pages before H2D"); beginFaultMigration only
	// performs the region transitions.
	// if err := m.reservation.ReserveAdmission(bytes); err != nil {
	// 	return err
	// }
	reg := tx.reg
	if reg == nil {
		return fmt.Errorf("uvm: fault migration without a registration")
	}
	if tx.plan == nil {
		return fmt.Errorf("uvm: fault migration begin without a plan")
	}
	// sbin_codex (todo 17): transition the fault region and every region
	// touched by the migration to MIGRATING_TO_GPU. A region already
	// migrating (an earlier prefetch / access-counter migration) joins the
	// in-flight migration.
	regions := m.regionsTouchedByPlanLocked(reg, tx)
	regions[tx.RegionBase] = true
	for regionBase := range regions {
		sm := m.faultRegionMachineLocked(reg, tx.GPU, regionBase)
		switch sm.Region.State {
		case RegionFaultPending, RegionIDLE, RegionCPUResident:
			if err := sm.Transition(RegionMigratingToGPU, now); err != nil {
				return err
			}
		case RegionMigratingToGPU:
			// An earlier migration (prefetch / access-counter) is already in
			// flight; this transaction's DMA joins it.
		case RegionGPUResident:
			// The fault region is already fully resident (its demand was
			// satisfied before this service); the TBN prefetch DMA for the
			// other regions needs no transition here.
		default:
			return fmt.Errorf(
				"uvm: fault service in illegal region state %s", sm.Region.State)
		}
	}
	return nil
}

// completeFaultMigration commits the migrated bytes to resident and
// transitions the region to GPU_RESIDENT. // sbin_codex
// sbin_codex (todo 18): delegates to the generic completeMigrationAdmission
// so AC/write migration transactions reuse the same admission completion.
// sbin_codex (todo 17): the TBN-prefetched regions touched by the migration
// are completed separately by completePrefetchRegions (uvm_tbn.go).
// func (m *UVMManager) completeFaultMigration(
//
//	tx *faultTransaction,
//	bytes uint64,
//	now sim.VTimeInSec,
//
//	) error {
//		m.Lock()
//		defer m.Unlock()
//
//		reg := tx.reg
//		if reg == nil {
//			return fmt.Errorf("uvm: fault migration commit without a registration")
//		}
//		if tx.plan == nil {
//			return fmt.Errorf("uvm: fault migration commit without a plan")
//		}
//		regions := m.regionsTouchedByPlanLocked(reg, tx)
//		regions[tx.RegionBase] = true
//		for regionBase := range regions {
//			if err := m.completeRegionAdmissionLocked(reg, tx.GPU, regionBase, now); err != nil {
//				return err
//			}
//		}
//		m.reservation.CommitAdmission(bytes)
//		return nil
//	}
func (m *UVMManager) completeFaultMigration(
	tx *faultTransaction,
	bytes uint64,
	now sim.VTimeInSec,
) error {
	return m.completeMigrationAdmission(tx.reg, tx.GPU, tx.RegionBase, bytes, now)
}

// completeMigrationAdmission commits the migrated bytes to resident and
// transitions the region to GPU_RESIDENT (recency updated on admission only,
// §31.2). // sbin_codex
func (m *UVMManager) completeMigrationAdmission(
	reg *ManagedAllocationRegistration,
	gpu int,
	regionBase uint64,
	bytes uint64,
	now sim.VTimeInSec,
) error {
	m.Lock()
	defer m.Unlock()

	if reg == nil {
		return fmt.Errorf("uvm: migration admission commit without a registration")
	}
	sm := m.faultRegionMachineLocked(reg, gpu, regionBase)
	switch sm.Region.State {
	case RegionMigratingToGPU:
		if err := sm.Transition(RegionGPUResident, now); err != nil {
			return err
		}
	case RegionGPUResident:
		// Already resident (an overlapping migration completed first).
	default:
		return fmt.Errorf(
			"uvm: migration completion in illegal region state %s", sm.Region.State)
	}
	m.reservation.CommitAdmission(bytes)
	return nil
}

// completeFault retires a serviced fault transaction: it removes the
// coalescing-table entry, releases the ownership slot, and wakes the ticket
// queue so a waiting copy can claim. // sbin_codex
func (m *UVMManager) completeFault(tx *faultTransaction) {
	m.Lock()
	defer m.Unlock()

	delete(m.faultByKey, tx.Key)
	if e := m.ownershipFor(tx.Key); e.OwnerID == tx.Ticket {
		e.OwnerType = OwnershipIdle
		e.OwnerID = 0
	}
	m.reevaluateLocked()
}

// intakeMigration consumes one access-counter notification or remote-write
// trigger: it creates the region's migration transaction when the region is
// IDLE/CPU_RESIDENT, and suppresses the trigger when the region is already
// being brought to the GPU (FAULT_PENDING / MIGRATING_TO_GPU, §16) or a
// transaction is already in flight — no additional transaction, no duplicate
// DMA. // sbin_codex
func (m *UVMManager) intakeMigration(
	pid vm.PID,
	gpu int,
	vaddr uint64,
	trigger migrationTrigger,
	now sim.VTimeInSec,
) (tx *migrationTransaction, err error) {
	m.Lock()
	defer m.Unlock()

	reg := m.registrationForPageLocked(pid, vaddr)
	if reg == nil {
		return nil, fmt.Errorf(
			"uvm: migration trigger on unmanaged address pid=%d va=%#x", pid, vaddr)
	}

	key := copyRegionKey{PID: pid, GPU: gpu, RegionBase: SubBlockStartVA(vaddr)}
	if m.migrationByKey[key] != nil {
		m.suppressedMigrationCount++
		return nil, nil
	}

	sm := m.faultRegionMachineLocked(reg, gpu, key.RegionBase)
	switch sm.Region.State {
	case RegionIDLE, RegionCPUResident:
		if err := sm.Transition(RegionMigratingToGPU, now); err != nil {
			return nil, err
		}
	case RegionFaultPending, RegionMigratingToGPU:
		// §16: a demand migration or prefetch already owns the region; the
		// notification is ignored and never fires retroactively.
		m.suppressedMigrationCount++
		return nil, nil
	default:
		// GPU_RESIDENT (no remote accesses should notify), EVICT_PENDING /
		// MIGRATING_TO_CPU (an eviction owns the region): ignore.
		m.suppressedMigrationCount++
		return nil, nil
	}

	switch trigger {
	case migrationTriggerAccessCounter:
		m.accessCounterMigrationCount++
	case migrationTriggerRemoteWrite:
		m.remoteWriteMigrationCount++
	}

	tx = &migrationTransaction{
		Ticket:      m.nextTicketLocked(),
		PID:         pid,
		GPU:         gpu,
		RegionBase:  key.RegionBase,
		Key:         key,
		Trigger:     trigger,
		DemandPages: m.demandPagesLocked(reg, key.RegionBase),
		ReplayToken: m.nextReplayTokenLocked(),
		reg:         reg,
		phase:       migrationPhaseClaiming,
	}
	m.migrationByKey[key] = tx
	return tx, nil
}

// completeMigration retires a completed migration transaction: it removes the
// coalescing-table entry and wakes the ticket queue so a waiting copy can
// claim. The ownership slot was already released before the unblock. // sbin_codex
func (m *UVMManager) completeMigration(tx *migrationTransaction) {
	m.Lock()
	defer m.Unlock()

	delete(m.migrationByKey, tx.Key)
	m.reevaluateLocked()
}

// AccessCounterMigrationCount returns the number of threshold-triggered
// migration transactions created. // sbin_codex
func (m *UVMManager) AccessCounterMigrationCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.accessCounterMigrationCount
}

// RemoteWriteMigrationCount returns the number of write-triggered migration
// transactions created. // sbin_codex
func (m *UVMManager) RemoteWriteMigrationCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.remoteWriteMigrationCount
}

// SuppressedMigrationCount returns the number of ignored notifications/writes
// (§16): triggers while the region is already being brought to the GPU or a
// migration transaction is in flight. // sbin_codex
func (m *UVMManager) SuppressedMigrationCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.suppressedMigrationCount
}

// PinRegion marks a 64 KB region pinned: it is never selected as an eviction
// victim (§18.2). // sbin_codex
func (m *UVMManager) PinRegion(key copyRegionKey) {
	m.Lock()
	defer m.Unlock()

	m.pinned[key] = true
}

// UnpinRegion removes the pin of a 64 KB region. // sbin_codex
func (m *UVMManager) UnpinRegion(key copyRegionKey) {
	m.Lock()
	defer m.Unlock()

	delete(m.pinned, key)
}

// IsPinned reports whether a 64 KB region is pinned. // sbin_codex
func (m *UVMManager) IsPinned(key copyRegionKey) bool {
	m.Lock()
	defer m.Unlock()

	return m.pinned[key]
}
