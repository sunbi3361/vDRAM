package vm

import (
	"fmt"
)

// sbin_codex: explicit UVM translation and control contracts (todo 2 of plan
// mgpusim-uvm-manager). This file owns the typed locations, access kinds,
// fault/replay tokens, waiter deltas, and block/unblock contracts that the UVM
// messages and envelopes carry. Akita never imports MGPUSim.

// MemoryLocation classifies where a managed translation currently resides.
//
// The zero value is UNMANAGED so that unmanaged pages (which never set
// Location) keep the legacy behavior. Location is only meaningful when
// Page.Managed is true; unmanaged page constructors must leave it UNMANAGED.
type MemoryLocation uint8

const (
	// MemoryLocationUNMANAGED is the zero value. It marks translations that are
	// not part of the UVM managed pool.
	MemoryLocationUNMANAGED MemoryLocation = iota
	// MemoryLocationINVALID marks a managed translation with no usable mapping.
	// Its PTE carries PAddr=0 and Valid=false and exposes no consumable address.
	MemoryLocationINVALID
	// MemoryLocationCPU_REMOTE marks a managed translation whose authoritative
	// backing is on the CPU. The PTE carries the CPU-backing PA, which is
	// consumed only by the remote (CPU) endpoint.
	MemoryLocationCPU_REMOTE
	// MemoryLocationGPU_LOCAL marks a managed translation that currently resides
	// in GPU HBM. The PTE carries the HBM PA.
	MemoryLocationGPU_LOCAL
)

// String returns a human-readable name for the location.
func (l MemoryLocation) String() string {
	switch l {
	case MemoryLocationUNMANAGED:
		return "UNMANAGED"
	case MemoryLocationINVALID:
		return "INVALID"
	case MemoryLocationCPU_REMOTE:
		return "CPU_REMOTE"
	case MemoryLocationGPU_LOCAL:
		return "GPU_LOCAL"
	default:
		return fmt.Sprintf("MemoryLocation(%d)", uint8(l))
	}
}

// ConsumableAddress resolves the physical address that an endpoint may consume
// for a managed translation.
//
//   - CPU_REMOTE: only the remote (CPU) endpoint may consume the CPU-backing PA.
//   - GPU_LOCAL: only the GPU endpoint may consume the HBM PA.
//   - INVALID: no endpoint may consume any address.
//   - UNMANAGED: both endpoints consume the plain PAddr (legacy behavior).
//
// ok is false when the endpoint has no consumable address for the translation.
// The switch is exhaustive and fails loudly for unknown locations.
func ConsumableAddress(location MemoryLocation, pAddr uint64, remote bool) (uint64, bool) {
	switch location {
	case MemoryLocationUNMANAGED:
		return pAddr, true
	case MemoryLocationCPU_REMOTE:
		if remote {
			return pAddr, true
		}
		return 0, false
	case MemoryLocationGPU_LOCAL:
		if remote {
			return 0, false
		}
		return pAddr, true
	case MemoryLocationINVALID:
		return 0, false
	default:
		panic(fmt.Sprintf("unknown MemoryLocation %d", uint8(location)))
	}
}

// AccessKind describes the kind of memory access that triggered a UVM event.
type AccessKind uint8

const (
	// AccessKindRead marks a read access.
	AccessKindRead AccessKind = iota
	// AccessKindWrite marks a write access.
	AccessKindWrite
)

// FaultPendingToken identifies a GMMU-visible translation request that is
// pending fault service. The GMMU assigns the token when the request first
// becomes visible and propagates it in the fault-pending response so the
// driver can match the service to the stalled request (see uvm-manager.md §22
// replay ownership).
type FaultPendingToken uint64

// ReplayToken identifies a replay ownership grant. The GMMU owns replayable
// stalled memory requests; the driver returns the replay token with the fault
// replay request so the GMMU can match the replay to the serviced region.
type ReplayToken uint64

// WaiterDelta reports the original waiter counts observed by a leaf data
// translation point. InitialWaiters is the count recorded when the translation
// first became pending; LateMSHRWaiters is the delta of waiters that joined
// while the translation was already pending in the MSHR. This file defines the
// contract only; the counting logic lives in a later todo.
type WaiterDelta struct {
	InitialWaiters  uint32
	LateMSHRWaiters uint32
}

// BlockRange is the block contract for a UVM mapping transition. It carries
// only the command ID, the process ID, and the virtual range to block.
type BlockRange struct {
	CommandID uint64
	PID       PID
	StartVA   uint64
	Size      uint64
}

// BlockAck is the acknowledgement for a BlockRange. It carries only the
// command ID, the gate ID that reached the watermark, and the local watermark.
type BlockAck struct {
	CommandID uint64
	GateID    uint64
	Watermark uint64
}

// UnblockRange is the unblock contract for a UVM mapping transition. It
// mirrors BlockRange and carries only the command ID, the process ID, and the
// virtual range to unblock.
type UnblockRange struct {
	CommandID uint64
	PID       PID
	StartVA   uint64
	Size      uint64
}

// UnblockAck is the acknowledgement for an UnblockRange. It carries only the
// command ID.
type UnblockAck struct {
	CommandID uint64
}
