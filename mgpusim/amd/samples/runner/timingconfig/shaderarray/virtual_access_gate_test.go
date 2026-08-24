package shaderarray

// sbin_codex: virtual-caching access gate waiter reporting contract test
// (plan todo 7 of mgpusim-uvm-manager). Virtual-caching has no leaf data TLB,
// so the L1V/L1S access gate is the leaf data translation point: it records
// each original request and reports raw = unique + coalesced.

import (
	"testing"
)

func TestVirtualUVMLeafWaiterDelta(t *testing.T) {
	// The virtual-caching L1V and L1S access gates each own a waiter counter;
	// the full gate admission logic is a later todo, this seam only records
	// the original request counts.
	l1vGate := &VirtualAccessGateWaiterCounter{}
	l1sGate := &VirtualAccessGateWaiterCounter{}

	// Phase 1: before fault-pending. The first original request for a 64 KB
	// fault-service region is unique.
	l1vGate.RecordOriginalRequest(true)
	if l1vGate.Raw() != 1 || l1vGate.Unique() != 1 || l1vGate.Coalesced() != 0 {
		t.Fatalf("phase 1: raw=1 unique=1 coalesced=0 expected, got raw=%d unique=%d coalesced=%d",
			l1vGate.Raw(), l1vGate.Unique(), l1vGate.Coalesced())
	}
	if l1vGate.Raw() != l1vGate.Unique()+l1vGate.Coalesced() {
		t.Fatalf("phase 1: raw = unique + coalesced must hold, got %d != %d + %d",
			l1vGate.Raw(), l1vGate.Unique(), l1vGate.Coalesced())
	}

	// Phase 2: during service. Duplicate waiters for the pending region are
	// coalesced into the same transaction.
	l1vGate.RecordOriginalRequest(false)
	l1vGate.RecordOriginalRequest(false)
	if l1vGate.Raw() != 3 || l1vGate.Unique() != 1 || l1vGate.Coalesced() != 2 {
		t.Fatalf("phase 2: raw=3 unique=1 coalesced=2 expected, got raw=%d unique=%d coalesced=%d",
			l1vGate.Raw(), l1vGate.Unique(), l1vGate.Coalesced())
	}
	if l1vGate.Raw() != l1vGate.Unique()+l1vGate.Coalesced() {
		t.Fatalf("phase 2: raw = unique + coalesced must hold, got %d != %d + %d",
			l1vGate.Raw(), l1vGate.Unique(), l1vGate.Coalesced())
	}

	// Phase 3: immediately before replay. The equation still holds with the
	// final waiter joining the pending transaction.
	l1vGate.RecordOriginalRequest(false)
	if l1vGate.Raw() != 4 || l1vGate.Unique() != 1 || l1vGate.Coalesced() != 3 {
		t.Fatalf("phase 3: raw=4 unique=1 coalesced=3 expected, got raw=%d unique=%d coalesced=%d",
			l1vGate.Raw(), l1vGate.Unique(), l1vGate.Coalesced())
	}
	if l1vGate.Raw() != l1vGate.Unique()+l1vGate.Coalesced() {
		t.Fatalf("phase 3: raw = unique + coalesced must hold, got %d != %d + %d",
			l1vGate.Raw(), l1vGate.Unique(), l1vGate.Coalesced())
	}

	// The scalar gate reports its own original requests independently.
	l1sGate.RecordOriginalRequest(true)
	l1sGate.RecordOriginalRequest(false)
	if l1sGate.Raw() != 2 || l1sGate.Unique() != 1 || l1sGate.Coalesced() != 1 {
		t.Fatalf("scalar gate: raw=2 unique=1 coalesced=1 expected, got raw=%d unique=%d coalesced=%d",
			l1sGate.Raw(), l1sGate.Unique(), l1sGate.Coalesced())
	}
	if l1sGate.Raw() != l1sGate.Unique()+l1sGate.Coalesced() {
		t.Fatalf("scalar gate: raw = unique + coalesced must hold, got %d != %d + %d",
			l1sGate.Raw(), l1sGate.Unique(), l1sGate.Coalesced())
	}
}
