package vm

import (
	"testing"
)

// sbin_codex: contract tests for the explicit UVM translation protocol
// (todo 2 of plan mgpusim-uvm-manager). Written first (RED), then made to pass
// (GREEN) by implementing akita/mem/vm/uvmprotocol.go and extending
// akita/mem/vm/protocol.go and akita/mem/vm/pagetable.go.

// TestMemoryLocationZeroValueIsUNMANAGED verifies that the zero value of
// MemoryLocation is UNMANAGED, so unmanaged page constructors keep the legacy
// behavior without touching the field.
func TestMemoryLocationZeroValueIsUNMANAGED(t *testing.T) {
	var loc MemoryLocation
	if loc != MemoryLocationUNMANAGED {
		t.Fatalf("zero value MemoryLocation = %v, want UNMANAGED", loc)
	}
}

// TestMemoryLocationEnumDistinct verifies the four locations are distinct and
// UNMANAGED is the zero value.
func TestMemoryLocationEnumDistinct(t *testing.T) {
	if MemoryLocationUNMANAGED == MemoryLocationINVALID ||
		MemoryLocationINVALID == MemoryLocationCPU_REMOTE ||
		MemoryLocationCPU_REMOTE == MemoryLocationGPU_LOCAL {
		t.Fatal("MemoryLocation values must be distinct")
	}
	if MemoryLocationUNMANAGED != 0 {
		t.Fatalf("UNMANAGED must be the zero value, got %d", MemoryLocationUNMANAGED)
	}
}

// TestMemoryLocationString verifies String() names the locations.
func TestMemoryLocationString(t *testing.T) {
	cases := []struct {
		loc  MemoryLocation
		want string
	}{
		{MemoryLocationUNMANAGED, "UNMANAGED"},
		{MemoryLocationINVALID, "INVALID"},
		{MemoryLocationCPU_REMOTE, "CPU_REMOTE"},
		{MemoryLocationGPU_LOCAL, "GPU_LOCAL"},
	}
	for _, c := range cases {
		if got := c.loc.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

// TestMemoryLocationExhaustiveSwitchPanicsOnUnknown proves switches on
// MemoryLocation are exhaustive: an unknown value fails loudly instead of
// silently falling through.
func TestMemoryLocationExhaustiveSwitchPanicsOnUnknown(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for unknown MemoryLocation")
		}
	}()
	ConsumableAddress(MemoryLocation(99), 0x1000, false)
}

// TestTranslationAddressAuthorityCPU_REMOTE asserts the CPU_REMOTE translation
// carries the authoritative CPU-backing PA consumed only by the remote
// (CPU) endpoint.
func TestTranslationAddressAuthorityCPU_REMOTE(t *testing.T) {
	const cpuPA = uint64(0x1000)

	addr, ok := ConsumableAddress(MemoryLocationCPU_REMOTE, cpuPA, true)
	if !ok || addr != cpuPA {
		t.Fatalf("remote endpoint should consume the CPU PA, got (%#x, %v)", addr, ok)
	}
	if _, ok := ConsumableAddress(MemoryLocationCPU_REMOTE, cpuPA, false); ok {
		t.Fatal("GPU endpoint must not consume the CPU-backing PA of a CPU_REMOTE translation")
	}
}

// TestTranslationAddressAuthorityGPU_LOCAL asserts the GPU_LOCAL translation
// carries the HBM PA consumed by the GPU endpoint.
func TestTranslationAddressAuthorityGPU_LOCAL(t *testing.T) {
	const hbmPA = uint64(0x80000000)

	addr, ok := ConsumableAddress(MemoryLocationGPU_LOCAL, hbmPA, false)
	if !ok || addr != hbmPA {
		t.Fatalf("GPU endpoint should consume the HBM PA, got (%#x, %v)", addr, ok)
	}
	if _, ok := ConsumableAddress(MemoryLocationGPU_LOCAL, hbmPA, true); ok {
		t.Fatal("remote endpoint must not consume the HBM PA of a GPU_LOCAL translation")
	}
}

// TestTranslationAddressAuthorityINVALID asserts INVALID exposes no consumable
// address to any endpoint.
func TestTranslationAddressAuthorityINVALID(t *testing.T) {
	if _, ok := ConsumableAddress(MemoryLocationINVALID, 0, false); ok {
		t.Fatal("INVALID must expose no consumable address to the GPU endpoint")
	}
	if _, ok := ConsumableAddress(MemoryLocationINVALID, 0, true); ok {
		t.Fatal("INVALID must expose no consumable address to the remote endpoint")
	}
}

// TestTranslationAddressAuthorityUNMANAGED asserts unmanaged translations keep
// the legacy behavior: both endpoints consume the plain PAddr.
func TestTranslationAddressAuthorityUNMANAGED(t *testing.T) {
	const pAddr = uint64(0x4000)

	for _, remote := range []bool{false, true} {
		addr, ok := ConsumableAddress(MemoryLocationUNMANAGED, pAddr, remote)
		if !ok || addr != pAddr {
			t.Fatalf("unmanaged translation must be directly addressable (remote=%v), got (%#x, %v)", remote, addr, ok)
		}
	}
}

// TestUVMProtocolPageLocationManagedOnly asserts Location is only meaningful
// for managed pages: unmanaged page literals keep the UNMANAGED zero value.
func TestUVMProtocolPageLocationManagedOnly(t *testing.T) {
	unmanaged := Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x2000, PageSize: 4096, Valid: true,
	}
	if unmanaged.Location != MemoryLocationUNMANAGED {
		t.Fatalf("unmanaged page Location = %v, want UNMANAGED", unmanaged.Location)
	}

	managed := Page{
		PID: 1, VAddr: 0x1000, PAddr: 0x2000, PageSize: 4096, Valid: true,
		Managed: true, Location: MemoryLocationGPU_LOCAL,
	}
	if managed.Location != MemoryLocationGPU_LOCAL {
		t.Fatalf("managed page Location = %v, want GPU_LOCAL", managed.Location)
	}
}

// TestUVMProtocolTranslationReqFields asserts TranslationReq carries access
// kind, location, fault-pending token, and waiter delta.
func TestUVMProtocolTranslationReqFields(t *testing.T) {
	req := TranslationReqBuilder{}.
		WithVAddr(0x1000).
		WithPID(7).
		WithAccessKind(AccessKindWrite).
		WithLocation(MemoryLocationCPU_REMOTE).
		WithFaultPendingToken(FaultPendingToken(42)).
		WithWaiterDelta(WaiterDelta{InitialWaiters: 3, LateMSHRWaiters: 2}).
		Build()

	if req.AccessKind != AccessKindWrite {
		t.Errorf("AccessKind = %v, want Write", req.AccessKind)
	}
	if req.Location != MemoryLocationCPU_REMOTE {
		t.Errorf("Location = %v, want CPU_REMOTE", req.Location)
	}
	if req.FaultPendingToken != FaultPendingToken(42) {
		t.Errorf("FaultPendingToken = %v, want 42", req.FaultPendingToken)
	}
	if req.WaiterDelta.InitialWaiters != 3 || req.WaiterDelta.LateMSHRWaiters != 2 {
		t.Errorf("WaiterDelta = %+v", req.WaiterDelta)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}
}

// TestUVMProtocolTranslationRspFields asserts TranslationRsp carries the
// resolved location, fault-pending token, and waiter delta.
func TestUVMProtocolTranslationRspFields(t *testing.T) {
	rsp := TranslationRspBuilder{}.
		WithPage(Page{PAddr: 0x5000, Valid: true}).
		WithLocation(MemoryLocationGPU_LOCAL).
		WithFaultPendingToken(FaultPendingToken(7)).
		WithWaiterDelta(WaiterDelta{InitialWaiters: 1, LateMSHRWaiters: 4}).
		Build()

	if rsp.Location != MemoryLocationGPU_LOCAL {
		t.Errorf("Location = %v, want GPU_LOCAL", rsp.Location)
	}
	if rsp.FaultPendingToken != FaultPendingToken(7) {
		t.Errorf("FaultPendingToken = %v, want 7", rsp.FaultPendingToken)
	}
	if rsp.WaiterDelta.InitialWaiters != 1 || rsp.WaiterDelta.LateMSHRWaiters != 4 {
		t.Errorf("WaiterDelta = %+v", rsp.WaiterDelta)
	}
}

// TestUVMProtocolTranslationRspConsumableAddress asserts the remote endpoint
// reads only the CPU PA from a CPU_REMOTE response.
func TestUVMProtocolTranslationRspConsumableAddress(t *testing.T) {
	const cpuPA = uint64(0x9000)

	rsp := TranslationRspBuilder{}.
		WithPage(Page{PAddr: cpuPA, Valid: true}).
		WithLocation(MemoryLocationCPU_REMOTE).
		Build()

	addr, ok := rsp.ConsumableAddress(true)
	if !ok || addr != cpuPA {
		t.Fatalf("remote endpoint must read the CPU PA, got (%#x, %v)", addr, ok)
	}
	if _, ok := rsp.ConsumableAddress(false); ok {
		t.Fatal("GPU endpoint must not read the CPU PA from a CPU_REMOTE response")
	}
}

// TestUVMProtocolFaultPendingToken asserts the fault-pending token type
// round-trips its value.
func TestUVMProtocolFaultPendingToken(t *testing.T) {
	var zero FaultPendingToken
	if zero != 0 {
		t.Fatalf("zero FaultPendingToken = %v, want 0", zero)
	}
	if FaultPendingToken(123) != 123 {
		t.Fatal("FaultPendingToken must round-trip its value")
	}
}

// TestUVMProtocolReplayToken asserts the replay token type round-trips its
// value.
func TestUVMProtocolReplayToken(t *testing.T) {
	if ReplayToken(99) != 99 {
		t.Fatal("ReplayToken must round-trip its value")
	}
}

// TestUVMProtocolWaiterDelta asserts the waiter-delta fields.
func TestUVMProtocolWaiterDelta(t *testing.T) {
	d := WaiterDelta{InitialWaiters: 5, LateMSHRWaiters: 1}
	if d.InitialWaiters != 5 || d.LateMSHRWaiters != 1 {
		t.Fatalf("WaiterDelta = %+v", d)
	}
}

// TestUVMProtocolBlockRangeContract asserts BlockRange carries only
// (commandID, PID, range) and the block ack carries (commandID, gateID,
// watermark).
func TestUVMProtocolBlockRangeContract(t *testing.T) {
	br := BlockRange{CommandID: 3, PID: 9, StartVA: 0x10000, Size: 0x10000}
	if br.CommandID != 3 || br.PID != 9 || br.StartVA != 0x10000 || br.Size != 0x10000 {
		t.Fatalf("BlockRange = %+v", br)
	}

	ack := BlockAck{CommandID: 3, GateID: 2, Watermark: 7}
	if ack.CommandID != 3 || ack.GateID != 2 || ack.Watermark != 7 {
		t.Fatalf("BlockAck = %+v", ack)
	}
}

// TestUVMProtocolAccessKind asserts the Read/Write access kinds are distinct.
func TestUVMProtocolAccessKind(t *testing.T) {
	if AccessKindRead == AccessKindWrite {
		t.Fatal("Read and Write access kinds must be distinct")
	}
}
