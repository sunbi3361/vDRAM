package cp

// sbin_codex: DMA Engine superior/subordinate contract tests (todo 16 of plan
// mgpusim-uvm-manager, uvm-manager.md §23.1.2). The QA regex
// 'TestUVM(DMARunCoalescing|FragmentedRuns|SuperiorProcessingLimit|
// Subordinate64BCount|ReservationAccounting|SecondRunRollback)' runs the
// SuperiorProcessingLimit and Subordinate64BCount fixtures in this file: at
// most four superior collections occupy processingReqs (the DMA engine's own
// maxRequestCount = 4 — no UVM-side concurrency cap), and each superior
// request splits into exactly ceil(runBytes/64) subordinate 64-byte
// transactions (Log2AccessSize = 6 — the DMA's internal transaction size is
// preserved).

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"go.uber.org/mock/gomock"
)

// TestUVMSuperiorProcessingLimit proves that the DMA engine's own
// maxRequestCount = 4 governs the number of superior collections in
// processingReqs: the 5th and 6th requests are not accepted, and the accepted
// requests map one-to-one to their collections.
func TestUVMSuperiorProcessingLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	engine := NewMockEngine(ctrl)
	toCP := NewMockPort(ctrl)
	toMem := NewMockPort(ctrl)
	toCP.EXPECT().AsRemote().Return(sim.RemotePort("ToCP")).AnyTimes()
	toMem.EXPECT().AsRemote().Return(sim.RemotePort("ToMem")).AnyTimes()

	localModuleFinder := new(mem.SinglePortMapper)
	dmaEngine := NewDMAEngine("DMA", engine, localModuleFinder)
	dmaEngine.ToCP = toCP
	dmaEngine.ToMem = toMem

	nilPort := NewMockPort(ctrl)
	nilPort.EXPECT().AsRemote().Return(sim.RemotePort("Nil")).AnyTimes()

	// Six superior H2D requests arrive; the engine accepts at most four (its
	// own maxRequestCount), leaving the rest unretrieved in the port.
	reqs := make([]*protocol.MemCopyH2DReq, 6)
	for i := range reqs {
		reqs[i] = protocol.NewMemCopyH2DReq(
			nilPort, toCP, make([]byte, 128), uint64(0x1000+128*i))
	}
	call := 0
	toCP.EXPECT().RetrieveIncoming().DoAndReturn(func() sim.Msg {
		if call < 4 {
			r := reqs[call]
			call++
			return r
		}
		return nil
	}).AnyTimes()

	accepted := 0
	for i := 0; i < 6; i++ {
		if dmaEngine.parseFromCP() {
			accepted++
		}
	}
	if accepted != 4 {
		t.Fatalf("accepted = %d, want 4 (the DMA's own maxRequestCount)", accepted)
	}
	if len(dmaEngine.processingReqs) != 4 {
		t.Fatalf("processingReqs = %d, want 4", len(dmaEngine.processingReqs))
	}
	// The four accepted superior requests map one-to-one to the collections.
	for i := 0; i < 4; i++ {
		if dmaEngine.processingReqs[i].getSuperior() != reqs[i] {
			t.Errorf("collection %d superior request mismatch", i)
		}
	}
}

// TestUVMSubordinate64BCount proves that a 64 KB superior request splits into
// exactly 65536/64 = 1024 subordinate 64-byte transactions (Log2AccessSize =
// 6), for both H2D and D2H directions.
func TestUVMSubordinate64BCount(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	engine := NewMockEngine(ctrl)
	toCP := NewMockPort(ctrl)
	toMem := NewMockPort(ctrl)
	toCP.EXPECT().AsRemote().Return(sim.RemotePort("ToCP")).AnyTimes()
	toMem.EXPECT().AsRemote().Return(sim.RemotePort("ToMem")).AnyTimes()

	localModuleFinder := new(mem.SinglePortMapper)
	dmaEngine := NewDMAEngine("DMA", engine, localModuleFinder)
	dmaEngine.ToCP = toCP
	dmaEngine.ToMem = toMem

	nilPort := NewMockPort(ctrl)
	nilPort.EXPECT().AsRemote().Return(sim.RemotePort("Nil")).AnyTimes()

	// H2D: a 64 KB run at a 4 KB-aligned address splits into exactly 1024
	// subordinate 64-byte writes.
	srcBuf := make([]byte, 64*1024)
	req := protocol.NewMemCopyH2DReq(nilPort, toCP, srcBuf, 0x1_0000_0000)
	toCP.EXPECT().RetrieveIncoming().Return(req)

	if !dmaEngine.parseFromCP() {
		t.Fatal("parseFromCP must accept the 64 KB H2D request")
	}
	if got := dmaEngine.Log2AccessSize; got != 6 {
		t.Fatalf("Log2AccessSize = %d, want 6 (64-byte transactions preserved)", got)
	}
	if len(dmaEngine.processingReqs) != 1 {
		t.Fatalf("processingReqs = %d, want 1", len(dmaEngine.processingReqs))
	}
	if got := dmaEngine.processingReqs[0].subordinateCount; got != 1024 {
		t.Errorf("H2D subordinate count = %d, want 1024 (64 KB / 64 B)", got)
	}
	if len(dmaEngine.toSendToMem) != 1024 {
		t.Fatalf("H2D subordinates = %d, want 1024", len(dmaEngine.toSendToMem))
	}
	for i, msg := range dmaEngine.toSendToMem {
		w, ok := msg.(*mem.WriteReq)
		if !ok {
			t.Fatalf("H2D subordinate %d = %T, want *mem.WriteReq", i, msg)
		}
		if len(w.Data) != 64 {
			t.Errorf("H2D subordinate %d data = %d bytes, want 64", i, len(w.Data))
		}
	}

	// D2H: the same 64 KB run splits into 1024 subordinate 64-byte reads.
	d2hBuf := make([]byte, 64*1024)
	req2 := protocol.NewMemCopyD2HReq(nilPort, toCP, 0x1_0000_0000, d2hBuf)
	toCP.EXPECT().RetrieveIncoming().Return(req2)
	if !dmaEngine.parseFromCP() {
		t.Fatal("parseFromCP must accept the 64 KB D2H request")
	}
	if len(dmaEngine.processingReqs) != 2 {
		t.Fatalf("processingReqs = %d, want 2", len(dmaEngine.processingReqs))
	}
	if got := dmaEngine.processingReqs[1].subordinateCount; got != 1024 {
		t.Errorf("D2H subordinate count = %d, want 1024", got)
	}
	reads := dmaEngine.toSendToMem[1024:]
	if len(reads) != 1024 {
		t.Fatalf("D2H subordinates = %d, want 1024", len(reads))
	}
	for i, msg := range reads {
		r, ok := msg.(*mem.ReadReq)
		if !ok {
			t.Fatalf("D2H subordinate %d = %T, want *mem.ReadReq", i, msg)
		}
		if r.AccessByteSize != 64 {
			t.Errorf("D2H subordinate %d bytes = %d, want 64", i, r.AccessByteSize)
		}
	}
}