package driver

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: checked region-state transitions (uvm-manager.md §23) and
// admission reservations (R+I+N <= C). The state machine rejects illegal
// transitions before any mutation so a failed transition never corrupts prior
// state.

// RegionContext identifies a region for error reporting and invariant context.
type RegionContext struct {
	PID    vm.PID
	GPU    int
	Block  uint64 // VA block index within the allocation
	Region uint64 // 64 KB sub-block index within the block
}

// TransitionError describes a rejected region-state transition with full
// PID/GPU/block/region/state context.
type TransitionError struct {
	Context RegionContext
	From    RegionState
	To      RegionState
	Reason  string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf(
		"uvm: illegal region transition pid=%d gpu=%d block=%d region=%d: %s -> %s: %s",
		e.Context.PID, e.Context.GPU, e.Context.Block, e.Context.Region,
		e.From, e.To, e.Reason)
}

// legalTransitions is the §23 transition table. Transitions not listed are
// rejected deterministically.
var legalTransitions = map[RegionState]map[RegionState]bool{
	RegionIDLE: {
		RegionFaultPending:   true, // GPU fault on a non-resident region
		RegionMigratingToGPU: true, // admission / prefetch without a pending fault
	},
	RegionFaultPending: {
		RegionMigratingToGPU: true, // fault service begins
	},
	RegionMigratingToGPU: {
		RegionGPUResident: true, // migration completes
	},
	RegionGPUResident: {
		RegionEvictPending: true, // eviction scheduled
	},
	RegionEvictPending: {
		RegionMigratingToCPU: true, // eviction begins
	},
	RegionMigratingToCPU: {
		RegionCPUResident: true, // eviction completes
	},
	RegionCPUResident: {
		RegionFaultPending:   true, // new GPU fault
		RegionMigratingToGPU: true, // access-counter migration / prefetch
	},
}

// RegionStateMachine performs checked transitions on one 64 KB region. It is
// the single authority over the region's transaction state.
type RegionStateMachine struct {
	Context RegionContext
	Region  *SubBlockState
}

// NewRegionStateMachine binds a state machine to a region and its context.
func NewRegionStateMachine(
	ctx RegionContext,
	region *SubBlockState,
) *RegionStateMachine {
	return &RegionStateMachine{Context: ctx, Region: region}
}

// Transition moves the region to `to` if legal. Illegal transitions return a
// *TransitionError BEFORE any mutation. On migration/admission transitions the
// migration-recency timestamp is updated (§31.2).
func (m *RegionStateMachine) Transition(to RegionState, now sim.VTimeInSec) error {
	from := m.Region.State
	if !legalTransitions[from][to] {
		return &TransitionError{
			Context: m.Context,
			From:    from,
			To:      to,
			Reason:  "transition not in the §23 table",
		}
	}
	m.Region.State = to
	if isMigrationOrAdmission(to) {
		m.Region.RecordMigration(now)
	}
	return nil
}

// isMigrationOrAdmission reports whether a transition starts or completes a
// migration/admission, the only events that update recency (§31.2).
func isMigrationOrAdmission(to RegionState) bool {
	switch to {
	case RegionMigratingToGPU, RegionGPUResident,
		RegionMigratingToCPU, RegionCPUResident:
		return true
	}
	return false
}

// CoalesceFault absorbs a second fault while the region is already being
// serviced (§23: MIGRATING_TO_GPU + second fault -> coalesce). It is a no-op
// success when a fault/migration is already in flight; otherwise it reports
// that no fault is in flight to coalesce.
func (m *RegionStateMachine) CoalesceFault() error {
	switch m.Region.State {
	case RegionFaultPending, RegionMigratingToGPU:
		return nil // coalesced: fault already being serviced
	default:
		return &TransitionError{
			Context: m.Context,
			From:    m.Region.State,
			To:      RegionFaultPending,
			Reason:  "no fault in flight to coalesce",
		}
	}
}

// CoalesceAccessCounterMigration ignores an access-counter migration while a
// GPU migration is already in flight (§23: MIGRATING_TO_GPU + access-counter
// migration -> ignore/coalesce).
func (m *RegionStateMachine) CoalesceAccessCounterMigration() error {
	if m.Region.State == RegionMigratingToGPU {
		return nil // ignored: migration already in flight
	}
	return &TransitionError{
		Context: m.Context,
		From:    m.Region.State,
		To:      RegionMigratingToGPU,
		Reason:  "no GPU migration in flight to coalesce",
	}
}

// StallOnGPUAccess reports whether a GPU access must stall. It returns nil when
// the access may proceed; a *TransitionError when the region is migrating to
// CPU and the access must stall and resolve after the eviction state is known
// (§23).
func (m *RegionStateMachine) StallOnGPUAccess() error {
	if m.Region.State == RegionMigratingToCPU {
		return &TransitionError{
			Context: m.Context,
			From:    m.Region.State,
			To:      m.Region.State,
			Reason:  "GPU access stalls while region migrates to CPU",
		}
	}
	return nil
}

// GPUWrite models a GPU write to the region. A write must not complete before
// the region is GPU-resident (§28 write invariant); a write to any other state
// is rejected so it cannot complete against a CPU_REMOTE mapping.
func (m *RegionStateMachine) GPUWrite() error {
	if m.Region.State == RegionGPUResident {
		return nil // write completes against GPU-local memory
	}
	return &TransitionError{
		Context: m.Context,
		From:    m.Region.State,
		To:      RegionGPUResident,
		Reason:  "GPU write to CPU_REMOTE must not complete before migration to GPU-local memory",
	}
}

// OwnershipType classifies the holder of one ownership-table slot (plan todo
// 5, uvm-manager.md §23). The ownership table is shared by copies, faults,
// migrations, prefetches, and evictions: a slot is idle, or owned by exactly
// one transaction of one type. COPY (todo 5) and FAULT (todo 15) are the
// types driven so far; the remaining types are reserved for the owning todos
// (16, 17, 19) and are exercised through the generic
// AcquireOwnership/ReleaseOwnership API.
type OwnershipType int

const (
	// OwnershipIdle marks a slot with no owner.
	OwnershipIdle OwnershipType = iota
	// OwnershipCopy marks a slot owned by a managed host copy transaction.
	OwnershipCopy
	// OwnershipFault marks a slot owned by a fault service (plan todo 15).
	OwnershipFault
	// OwnershipMigration marks a slot owned by a migration (plan todo 16).
	OwnershipMigration
	// OwnershipPrefetch marks a slot owned by a prefetch (plan todo 17).
	OwnershipPrefetch
	// OwnershipEviction marks a slot owned by an eviction (plan todo 19).
	OwnershipEviction
)

// String returns the symbolic ownership-type name.
func (t OwnershipType) String() string {
	switch t {
	case OwnershipIdle:
		return "IDLE"
	case OwnershipCopy:
		return "COPY"
	case OwnershipFault:
		return "FAULT"
	case OwnershipMigration:
		return "MIGRATION"
	case OwnershipPrefetch:
		return "PREFETCH"
	case OwnershipEviction:
		return "EVICTION"
	default:
		return "UNKNOWN"
	}
}

// OwnershipEntry is one slot of the shared ownership table, keyed by
// (PID, GPU, regionBase). A COPY-owned region is ineligible as an eviction
// victim and later faults queue on it, so no victim dependency cycle exists:
// waiters never hold a slot.
type OwnershipEntry struct {
	OwnerType OwnershipType
	OwnerID   uint64 // transaction ticket / holder ID; 0 when idle
}

// faultPhase is the progress of one fault-service transaction (plan todo 15,
// uvm-manager.md §8.4, §9). A transaction is created at the first raw fault
// of its 64 KB region, waits FIFO, and is serviced when it reaches the head.
type faultPhase int

const (
	// faultPhaseQueued waits in the FIFO for the head.
	faultPhaseQueued faultPhase = iota
	// faultPhaseLatency has reached the head; its one software latency is
	// scheduled as a simulation event.
	faultPhaseLatency
	// faultPhaseClaiming waits for its ownership slot (e.g. a copy).
	faultPhaseClaiming
	// sbin_codex (todo 20): faultPhaseWaitingCapacity queues the admission
	// for a hard capacity/frame shortage; the retry re-runs the
	// projected-occupancy gate once in-flight pre-evictions free capacity.
	faultPhaseWaitingCapacity
	// faultPhaseMigrating transfers the missing pages.
	faultPhaseMigrating
	// faultPhaseTLBI waits for the 64 KB TLB invalidation ack.
	faultPhaseTLBI
	// faultPhaseReplaying waits for the GMMU replay ack.
	faultPhaseReplaying
	// faultPhaseDone completed after the replay; the next FIFO may start.
	faultPhaseDone
)

// AdmissionReservation tracks GPU capacity reservations for the R+I+N <= C
// invariant (plan todo 20 enforces eviction; this todo owns the tracking
// structure): R = resident bytes, I = in-flight migration bytes, N = reserved
// (admitted but not yet resident) bytes, C = configured capacity.
type AdmissionReservation struct {
	capacityBytes uint64
	residentBytes uint64 // R
	inFlightBytes uint64 // I
	reservedBytes uint64 // N
}

// NewAdmissionReservation creates an empty reservation against capacityBytes.
func NewAdmissionReservation(capacityBytes uint64) *AdmissionReservation {
	return &AdmissionReservation{capacityBytes: capacityBytes}
}

// CapacityBytes returns the configured capacity C.
func (r *AdmissionReservation) CapacityBytes() uint64 { return r.capacityBytes }

// ResidentBytes returns R.
func (r *AdmissionReservation) ResidentBytes() uint64 { return r.residentBytes }

// InFlightBytes returns I.
func (r *AdmissionReservation) InFlightBytes() uint64 { return r.inFlightBytes }

// ReservedBytes returns N.
func (r *AdmissionReservation) ReservedBytes() uint64 { return r.reservedBytes }

// ReserveAdmission reserves `bytes` for a new admission (N += bytes) only if
// R+I+N+bytes <= C. On failure it returns a descriptive error and mutates
// nothing, so a rejected reservation never corrupts prior capacity state.
func (r *AdmissionReservation) ReserveAdmission(bytes uint64) error {
	if r.residentBytes+r.inFlightBytes+r.reservedBytes+bytes > r.capacityBytes {
		return fmt.Errorf(
			"uvm: admission reservation %d exceeds capacity %d (R=%d I=%d N=%d)",
			bytes, r.capacityBytes, r.residentBytes, r.inFlightBytes, r.reservedBytes)
	}
	r.reservedBytes += bytes
	return nil
}

// CommitAdmission moves a reserved admission to resident (N -= bytes, R +=
// bytes) when the migration completes.
func (r *AdmissionReservation) CommitAdmission(bytes uint64) {
	r.reservedBytes -= bytes
	r.residentBytes += bytes
}

// ReleaseAdmission returns an unreserved admission (N -= bytes), e.g. a
// cancelled migration.
func (r *AdmissionReservation) ReleaseAdmission(bytes uint64) {
	r.reservedBytes -= bytes
}

// StartMigration moves resident bytes into flight (R -= bytes, I += bytes),
// e.g. an eviction begins.
func (r *AdmissionReservation) StartMigration(bytes uint64) {
	r.residentBytes -= bytes
	r.inFlightBytes += bytes
}

// CompleteMigrationToGPU moves in-flight bytes to resident (I -= bytes, R +=
// bytes), e.g. a migration to GPU completes.
func (r *AdmissionReservation) CompleteMigrationToGPU(bytes uint64) {
	r.inFlightBytes -= bytes
	r.residentBytes += bytes
}

// CompleteMigrationToCPU releases in-flight bytes (I -= bytes), e.g. an
// eviction completes and the bytes are no longer GPU-resident.
func (r *AdmissionReservation) CompleteMigrationToCPU(bytes uint64) {
	r.inFlightBytes -= bytes
}
