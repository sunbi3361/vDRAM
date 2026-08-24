package cache

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// sbin_codex: contract tests for the UVM cache control messages (todo 2 of
// plan mgpusim-uvm-manager). Written first (RED), then made to pass (GREEN)
// by implementing the UVMCacheRangeFlush* messages in
// akita/mem/cache/protocol.go.

// TestUVMProtocolCacheRangeFlushOp asserts the flush operations are distinct.
func TestUVMProtocolCacheRangeFlushOp(t *testing.T) {
	if UVMCacheRangeFlushWritebackOnly == UVMCacheRangeFlushWritebackInvalidate {
		t.Fatal("flush operations must be distinct")
	}
}

// TestUVMProtocolCacheRangeFlushReq asserts the range flush request carries
// operation, PID, VA base, valid-page mask, and physical runs.
func TestUVMProtocolCacheRangeFlushReq(t *testing.T) {
	req := UVMCacheRangeFlushReqBuilder{}.
		WithPID(4).
		WithVABase(0x10000).
		WithValidPageMask(0xFFFF).
		WithPhysicalRuns([]PhysicalRun{{Start: 0x2000, Length: 0x4000}}).
		WithOperation(UVMCacheRangeFlushWritebackInvalidate).
		Build()

	if req.PID != 4 {
		t.Errorf("PID = %v, want 4", req.PID)
	}
	if req.VABase != 0x10000 {
		t.Errorf("VABase = %#x, want 0x10000", req.VABase)
	}
	if req.ValidPageMask != 0xFFFF {
		t.Errorf("ValidPageMask = %#x, want 0xFFFF", req.ValidPageMask)
	}
	if len(req.PhysicalRuns) != 1 || req.PhysicalRuns[0].Start != 0x2000 ||
		req.PhysicalRuns[0].Length != 0x4000 {
		t.Errorf("PhysicalRuns = %+v", req.PhysicalRuns)
	}
	if req.Operation != UVMCacheRangeFlushWritebackInvalidate {
		t.Errorf("Operation = %v, want WritebackInvalidate", req.Operation)
	}
	if req.Meta() == nil {
		t.Error("Meta() must not be nil")
	}
	if req.Clone() == req {
		t.Error("Clone must return a distinct message")
	}
}

// TestUVMProtocolCacheRangeFlushRsp asserts the response carries the request
// ID it replies to.
func TestUVMProtocolCacheRangeFlushRsp(t *testing.T) {
	rsp := UVMCacheRangeFlushRspBuilder{}.
		WithRspTo("req-1").
		Build()

	if rsp.GetRspTo() != "req-1" {
		t.Errorf("GetRspTo() = %q, want req-1", rsp.GetRspTo())
	}
}

// TestUVMProtocolCacheRangeFlushGenerateRsp asserts a request generates a
// response addressed back to the requester.
func TestUVMProtocolCacheRangeFlushGenerateRsp(t *testing.T) {
	req := UVMCacheRangeFlushReqBuilder{}.
		WithSrc(sim.RemotePort("src")).
		WithDst(sim.RemotePort("dst")).
		Build()

	rsp := req.GenerateRsp()
	if rsp == nil {
		t.Fatal("GenerateRsp must return a response")
	}
	if rsp.Meta().Dst != sim.RemotePort("src") {
		t.Errorf("response dst = %v, want src", rsp.Meta().Dst)
	}
	if rsp.GetRspTo() != req.ID {
		t.Errorf("response rspTo = %q, want %q", rsp.GetRspTo(), req.ID)
	}
}

// TestUVMProtocolCacheRangeFlushReqPIDType asserts the request scopes the flush
// by the typed process ID.
func TestUVMProtocolCacheRangeFlushReqPIDType(t *testing.T) {
	req := UVMCacheRangeFlushReqBuilder{}.
		WithPID(vm.PID(12)).
		Build()

	if req.PID != vm.PID(12) {
		t.Errorf("PID = %v, want 12", req.PID)
	}
}
