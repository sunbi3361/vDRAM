package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/vm"
)

// sbin_codex: projected-occupancy pre-eviction (plan todo 20 of
// mgpusim-uvm-manager, uvm-manager.md §17.1, §23.1.2, decisions at
// 3002-3017/3071-3074). Before admitting an incoming H2D migration the
// manager computes the projected occupancy R+I+N (R = resident bytes, I =
// in-flight eviction bytes, N = already-reserved incoming bytes) plus the
// new bytes; the admission queues ONLY for a hard capacity/frame shortage
// (R+I+N+bytes > C or no destination frames). When the incoming fits, the
// admission is reserved and the H2D is launched immediately. The optional 64
// KB free-headroom target is then enforced proactively: when
// C-(R+I+N)+E < H (E = bytes whose pre-eviction is already in flight), the
// gate deterministically reserves
// NeedToEvict = max(0, H-(C-(R+I+N)+E)) worth of 64 KB LRU victims
// (NumVictims = ceil(NeedToEvict/64KB)) and launches their eviction
// transactions CONCURRENTLY with the H2D — the H2D never waits for the D2H
// completion, both use the existing DMA backpressure, R+I+N <= C holds
// throughout, and the headroom is required only after the victims complete.
// When the optional target is infeasible (e.g. every eligible region is
// pinned), the admission still proceeds and records the shortfall as a
// diagnostic. Reservations and victim ordinals are tracked; there is no
// fixed pre-eviction depth.

// preEvictionHeadroomBytes is the 64 KB free-capacity headroom target H
// (decision table: "Pre-eviction free-headroom target: 64 KB").
const preEvictionHeadroomBytes = subblockSizeBytes // 64 KB

// admissionDecision is the outcome of the projected-occupancy admission
// gate. // sbin_codex
type admissionDecision struct {
	// HardShortage reports R+I+N+bytes > C: the admission must wait for
	// capacity (no reservation is made).
	HardShortage bool
	// Fits reports the admission was reserved (N += bytes).
	Fits bool
	// NeedToEvict is max(0, H-(C-(R+I+N)+E)): bytes to schedule for
	// pre-eviction.
	NeedToEvict uint64
	// NumVictims is ceil(NeedToEvict/64KB): the 64 KB victims to reserve.
	NumVictims int
	// HeadroomFeasible reports the optional headroom target was reached:
	// no victims were needed or every required victim was reserved.
	HeadroomFeasible bool
	// Shortfall is the remaining headroom deficit when victims could not be
	// reserved (diagnostic; the admission still proceeds).
	Shortfall uint64
}

// preEvictionStats tracks the §17.1 pre-eviction statistics. Todo 22 exposes
// them through the reporter; this todo owns the update points.
type preEvictionStats struct {
	numPreEvictions                  uint64
	bytesPreEvicted                  uint64
	numConcurrentPreEvictions        uint64
	maxConcurrentPreEvictions        uint64
	numPreEvictionsOverlappedWithH2D uint64
	migrationWaitCyclesForCapacity   uint64
	optionalHeadroomShortfallCount   uint64
	optionalHeadroomShortfallBytes   uint64
}

// projectedOccupancyLocked returns the projected occupancy terms under the
// manager lock: R = resident bytes, I = in-flight eviction bytes, N =
// reserved incoming bytes, E = bytes whose pre-eviction is already in flight
// (launched victims, whether or not their R->I move has started). The caller
// must hold the manager lock. // sbin_codex
func (m *UVMManager) projectedOccupancyLocked() (r, i, n, e uint64) {
	r = m.reservation.ResidentBytes()
	i = m.reservation.InFlightBytes()
	n = m.reservation.ReservedBytes()
	for _, tx := range m.evictByKey {
		if tx.preEviction {
			e += tx.bytes
		}
	}
	return
}

// ProjectedOccupancy returns the projected occupancy terms R, I, N, E under
// the manager lock. // sbin_codex
func (m *UVMManager) ProjectedOccupancy() (r, i, n, e uint64) {
	m.Lock()
	defer m.Unlock()

	return m.projectedOccupancyLocked()
}

// computeAdmissionDecisionLocked computes the projected-occupancy admission
// decision WITHOUT mutating anything: the hard-shortage test
// R+I+N+bytes <= C and the headroom trigger
// NeedToEvict = max(0, H-(C-(R+I+N)+E)) with
// NumVictims = ceil(NeedToEvict/64KB). The caller must hold the manager
// lock. // sbin_codex
func (m *UVMManager) computeAdmissionDecisionLocked(
	bytes uint64,
) admissionDecision {
	r, i, n, e := m.projectedOccupancyLocked()
	c := m.reservation.CapacityBytes()

	var dec admissionDecision
	if r+i+n+bytes > c {
		dec.HardShortage = true
	} else {
		dec.Fits = true
	}
	free := uint64(0)
	if c > r+i+n+bytes {
		free = c - (r + i + n + bytes)
	}
	if free+e < preEvictionHeadroomBytes {
		dec.NeedToEvict = preEvictionHeadroomBytes - (free + e)
	}
	dec.NumVictims = int((dec.NeedToEvict + subblockSizeBytes - 1) /
		subblockSizeBytes)
	return dec
}

// admitWithPreEvictionLocked runs the projected-occupancy admission gate:
// a hard capacity shortage reserves nothing and returns an error (the
// admission queues); otherwise the admission is reserved and the required
// 64 KB LRU victims are marked EVICTING and returned for concurrent
// pre-eviction. The victims are launched on BOTH paths: the headroom trigger
// includes the new bytes, so a queued admission also launches victims whose
// completion frees the capacity it waits for (§17.1). The caller must hold
// the manager lock. // sbin_codex
func (m *UVMManager) admitWithPreEvictionLocked(
	pid vm.PID,
	gpu int,
	bytes uint64,
) ([]*evictionTransaction, error) {
	dec := m.computeAdmissionDecisionLocked(bytes)
	if !dec.HardShortage {
		m.reservation.ReserveAdmission(bytes)
	}
	victims := m.launchPreEvictionVictimsLocked(pid, gpu, dec)
	if dec.HardShortage {
		r, i, n, _ := m.projectedOccupancyLocked()
		return victims, fmt.Errorf(
			"uvm: admission of %d bytes exceeds capacity %d (R=%d I=%d N=%d)",
			bytes, m.reservation.CapacityBytes(), r, i, n)
	}
	return victims, nil
}

// launchPreEvictionVictimsLocked reserves the deterministic 64 KB LRU
// victims required by the headroom formula and records the §17.1 statistics.
// When a required victim cannot be reserved (no eligible region, e.g.
// pinned-only), the optional target is infeasible: the shortfall is recorded
// as a diagnostic and the admission still proceeds. The caller must hold the
// manager lock. // sbin_codex
func (m *UVMManager) launchPreEvictionVictimsLocked(
	pid vm.PID,
	gpu int,
	dec admissionDecision,
) []*evictionTransaction {
	var victims []*evictionTransaction
	for v := 0; v < dec.NumVictims; v++ {
		tx, err := m.intakeEvictionVictimLocked(pid, gpu)
		if err != nil {
			// Optional headroom infeasible: admit anyway, record shortfall.
			dec.HeadroomFeasible = false
			dec.Shortfall = dec.NeedToEvict - uint64(v)*subblockSizeBytes
			m.preEviction.optionalHeadroomShortfallCount++
			m.preEviction.optionalHeadroomShortfallBytes += dec.Shortfall
			break
		}
		tx.preEviction = true
		victims = append(victims, tx)
		m.preEviction.numPreEvictions++
		m.preEviction.bytesPreEvicted += tx.bytes
		m.preEviction.numConcurrentPreEvictions++
		if m.preEviction.numConcurrentPreEvictions >
			m.preEviction.maxConcurrentPreEvictions {
			m.preEviction.maxConcurrentPreEvictions =
				m.preEviction.numConcurrentPreEvictions
		}
		// A victim launched while an H2D admission is reserved overlaps with
		// that H2D transfer (§17.1 DMA concurrency).
		if m.reservation.ReservedBytes() > 0 {
			m.preEviction.numPreEvictionsOverlappedWithH2D++
		}
	}
	return victims
}

// recordCapacityWait records one admission wait cycle for capacity/frames
// (migration_wait_cycles_for_capacity, §17.1). // sbin_codex
func (m *UVMManager) recordCapacityWait() {
	m.Lock()
	defer m.Unlock()

	m.preEviction.migrationWaitCyclesForCapacity++
}

// NumPreEvictions returns the number of pre-eviction victims launched
// (§17.1 num_pre_evictions). // sbin_codex
func (m *UVMManager) NumPreEvictions() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.numPreEvictions
}

// BytesPreEvicted returns the bytes scheduled for pre-eviction (§17.1
// bytes_pre_evicted). // sbin_codex
func (m *UVMManager) BytesPreEvicted() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.bytesPreEvicted
}

// NumConcurrentPreEvictions returns the number of pre-evictions currently in
// flight (§17.1 num_concurrent_pre_evictions). // sbin_codex
func (m *UVMManager) NumConcurrentPreEvictions() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.numConcurrentPreEvictions
}

// MaxConcurrentPreEvictions returns the peak concurrent pre-eviction depth
// (§17.1 max_concurrent_pre_evictions). // sbin_codex
func (m *UVMManager) MaxConcurrentPreEvictions() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.maxConcurrentPreEvictions
}

// NumPreEvictionsOverlappedWithH2D returns the number of pre-evictions that
// overlapped with an H2D migration transfer (§17.1
// num_pre_evictions_overlapped_with_h2d). // sbin_codex
func (m *UVMManager) NumPreEvictionsOverlappedWithH2D() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.numPreEvictionsOverlappedWithH2D
}

// MigrationWaitCyclesForCapacity returns the number of admission wait cycles
// for hard capacity/frame shortage (§17.1
// migration_wait_cycles_for_capacity). // sbin_codex
func (m *UVMManager) MigrationWaitCyclesForCapacity() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.migrationWaitCyclesForCapacity
}

// OptionalHeadroomShortfallCount returns the number of admissions whose
// optional 64 KB headroom target was infeasible (diagnostic). // sbin_codex
func (m *UVMManager) OptionalHeadroomShortfallCount() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.optionalHeadroomShortfallCount
}

// OptionalHeadroomShortfallBytes returns the total headroom deficit recorded
// when the optional target was infeasible (diagnostic). // sbin_codex
func (m *UVMManager) OptionalHeadroomShortfallBytes() uint64 {
	m.Lock()
	defer m.Unlock()

	return m.preEviction.optionalHeadroomShortfallBytes
}
