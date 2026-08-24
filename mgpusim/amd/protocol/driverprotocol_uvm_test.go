package protocol

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: contract tests for the UVM driver/CP/DMA envelopes (todo 2 of
// plan mgpusim-uvm-manager). Written first (RED), then made to pass (GREEN)
// by implementing the messages in mgpusim/amd/protocol/driverprotocol.go.

// TestUVMProtocolPageFaultReq asserts the page-fault envelope carries PID, GPU,
// VAddr, access type, source CU, and the fault-pending token.
func TestUVMProtocolPageFaultReq(t *testing.T) {
	req := PageFaultReqBuilder{}.
		WithPID(3).
		WithGPU(1).
		WithVAddr(0x1000).
		WithAccessType(vm.AccessKindWrite).
		WithSourceCU(5).
		WithFaultPendingToken(vm.FaultPendingToken(11)).
		Build()

	if req.PID != 3 || req.GPU != 1 || req.VAddr != 0x1000 ||
		req.AccessType != vm.AccessKindWrite || req.SourceCU != 5 ||
		req.FaultPendingToken != vm.FaultPendingToken(11) {
		t.Fatalf("PageFaultReq = %+v", req)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}

	rsp := req.GenerateRsp()
	if rsp == nil {
		t.Fatal("GenerateRsp must return a response")
	}
	if rsp.GetRspTo() != req.ID {
		t.Errorf("rspTo = %q, want %q", rsp.GetRspTo(), req.ID)
	}
}

// TestUVMProtocolPageFaultReqNewConstructor asserts the New* constructor
// follows the driver protocol file convention.
func TestUVMProtocolPageFaultReqNewConstructor(t *testing.T) {
	src := sim.NewPort(nil, 1, 1, "Src")
	dst := sim.NewPort(nil, 1, 1, "Dst")

	req := NewPageFaultReq(src, dst)
	if req == nil {
		t.Fatal("NewPageFaultReq must return a request")
	}
	if req.Meta().Src != sim.RemotePort("Src") {
		t.Errorf("src = %v, want Src", req.Meta().Src)
	}
	if req.Meta().Dst != sim.RemotePort("Dst") {
		t.Errorf("dst = %v, want Dst", req.Meta().Dst)
	}
}

// TestUVMProtocolPageFaultRsp asserts the response carries the request ID.
func TestUVMProtocolPageFaultRsp(t *testing.T) {
	rsp := PageFaultRspBuilder{}.
		WithRspTo("fault-1").
		WithFaultPendingToken(vm.FaultPendingToken(2)).
		Build()

	if rsp.GetRspTo() != "fault-1" {
		t.Errorf("GetRspTo() = %q, want fault-1", rsp.GetRspTo())
	}
	if rsp.FaultPendingToken != vm.FaultPendingToken(2) {
		t.Errorf("FaultPendingToken = %v, want 2", rsp.FaultPendingToken)
	}
}

// TestUVMProtocolAccessCounterNotification asserts the notification carries
// PID, GPU, VAddr, access kind, and the observed count.
func TestUVMProtocolAccessCounterNotification(t *testing.T) {
	n := AccessCounterNotificationBuilder{}.
		WithPID(6).
		WithGPU(0).
		WithVAddr(0x2000).
		WithAccessKind(vm.AccessKindRead).
		WithAccessCount(17).
		Build()

	if n.PID != 6 || n.GPU != 0 || n.VAddr != 0x2000 ||
		n.AccessKind != vm.AccessKindRead || n.AccessCount != 17 {
		t.Fatalf("AccessCounterNotification = %+v", n)
	}
	if n.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if n.Clone() == n {
		t.Error("Clone must return a distinct message")
	}
}

// TestUVMProtocolMigrationReq asserts the migration envelope carries PID, GPU,
// VAddr, size, and direction.
func TestUVMProtocolMigrationReq(t *testing.T) {
	req := MigrationReqBuilder{}.
		WithPID(2).
		WithGPU(1).
		WithVAddr(0x4000).
		WithSize(0x10000).
		WithDirection(MigrationCPUToGPU).
		Build()

	if req.PID != 2 || req.GPU != 1 || req.VAddr != 0x4000 ||
		req.Size != 0x10000 || req.Direction != MigrationCPUToGPU {
		t.Fatalf("MigrationReq = %+v", req)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}

	rsp := req.GenerateRsp()
	if rsp == nil {
		t.Fatal("GenerateRsp must return a response")
	}
	if rsp.GetRspTo() != req.ID {
		t.Errorf("rspTo = %q, want %q", rsp.GetRspTo(), req.ID)
	}
}

// TestUVMProtocolMigrationRsp asserts the migration response carries the
// request ID.
func TestUVMProtocolMigrationRsp(t *testing.T) {
	rsp := MigrationRspBuilder{}.
		WithRspTo("mig-1").
		Build()

	if rsp.GetRspTo() != "mig-1" {
		t.Errorf("GetRspTo() = %q, want mig-1", rsp.GetRspTo())
	}
}

// TestUVMProtocolUVMTLBInvalidateReq asserts the TLB invalidation envelope is
// scoped by PID/ASID, StartVA, and Size.
func TestUVMProtocolUVMTLBInvalidateReq(t *testing.T) {
	req := UVMTLBInvalidateReqBuilder{}.
		WithPID(8).
		WithStartVA(0x10000).
		WithSize(0x10000).
		Build()

	if req.PID != 8 || req.StartVA != 0x10000 || req.Size != 0x10000 {
		t.Fatalf("UVMTLBInvalidateReq = %+v", req)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}

	rsp := req.GenerateRsp()
	if rsp == nil {
		t.Fatal("GenerateRsp must return a response")
	}
	if rsp.GetRspTo() != req.ID {
		t.Errorf("rspTo = %q, want %q", rsp.GetRspTo(), req.ID)
	}
}

// TestUVMProtocolUVMTLBInvalidateRsp asserts the response carries the request
// ID.
func TestUVMProtocolUVMTLBInvalidateRsp(t *testing.T) {
	rsp := UVMTLBInvalidateRspBuilder{}.
		WithRspTo("tlb-1").
		Build()

	if rsp.GetRspTo() != "tlb-1" {
		t.Errorf("GetRspTo() = %q, want tlb-1", rsp.GetRspTo())
	}
}

// TestUVMProtocolUVMCacheRangeFlushReq asserts the driver cache-flush envelope
// carries operation, PID, VA base, valid-page mask, and physical runs.
func TestUVMProtocolUVMCacheRangeFlushReq(t *testing.T) {
	req := UVMCacheRangeFlushReqBuilder{}.
		WithOperation(cache.UVMCacheRangeFlushWritebackInvalidate).
		WithPID(5).
		WithVABase(0x10000).
		WithValidPageMask(0xFF00).
		WithPhysicalRuns([]cache.PhysicalRun{{Start: 0x2000, Length: 0x4000}}).
		Build()

	if req.Operation != cache.UVMCacheRangeFlushWritebackInvalidate || req.PID != 5 ||
		req.VABase != 0x10000 || req.ValidPageMask != 0xFF00 ||
		len(req.PhysicalRuns) != 1 || req.PhysicalRuns[0].Start != 0x2000 {
		t.Fatalf("UVMCacheRangeFlushReq = %+v", req)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}

	rsp := req.GenerateRsp()
	if rsp == nil {
		t.Fatal("GenerateRsp must return a response")
	}
	if rsp.GetRspTo() != req.ID {
		t.Errorf("rspTo = %q, want %q", rsp.GetRspTo(), req.ID)
	}
}

// TestUVMProtocolUVMCacheRangeFlushRsp asserts the response carries the request
// ID.
func TestUVMProtocolUVMCacheRangeFlushRsp(t *testing.T) {
	rsp := UVMCacheRangeFlushRspBuilder{}.
		WithRspTo("flush-1").
		Build()

	if rsp.GetRspTo() != "flush-1" {
		t.Errorf("GetRspTo() = %q, want flush-1", rsp.GetRspTo())
	}
}

// TestUVMProtocolUVMFaultReplayReq asserts the replay envelope carries PID,
// GPU, the serviced VA range, and the replay token.
func TestUVMProtocolUVMFaultReplayReq(t *testing.T) {
	req := UVMFaultReplayReqBuilder{}.
		WithPID(9).
		WithGPU(0).
		WithStartVA(0x10000).
		WithSize(0x10000).
		WithReplayToken(vm.ReplayToken(4)).
		Build()

	if req.PID != 9 || req.GPU != 0 || req.StartVA != 0x10000 ||
		req.Size != 0x10000 || req.ReplayToken != vm.ReplayToken(4) {
		t.Fatalf("UVMFaultReplayReq = %+v", req)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}

	rsp := req.GenerateRsp()
	if rsp == nil {
		t.Fatal("GenerateRsp must return a response")
	}
	if rsp.GetRspTo() != req.ID {
		t.Errorf("rspTo = %q, want %q", rsp.GetRspTo(), req.ID)
	}
}

// TestUVMProtocolUVMFaultReplayRsp asserts the response carries the request ID.
func TestUVMProtocolUVMFaultReplayRsp(t *testing.T) {
	rsp := UVMFaultReplayRspBuilder{}.
		WithRspTo("replay-1").
		Build()

	if rsp.GetRspTo() != "replay-1" {
		t.Errorf("GetRspTo() = %q, want replay-1", rsp.GetRspTo())
	}
}
