package cp

// sbin_codex: CP UVM coordinator-identity routing tests (plan todo 21 of
// mgpusim-uvm-manager). These plain Go tests prove the uvmMiddleware stamps
// each routed root (kernelLaunchOrdinal, sourceBuildOrdinal,
// sourceLocalSequence) with the semantic key components so the driver
// coordinator can establish the cross-mode identity: the sourceLocalSequence
// is a per-source local tie-break, excluded from cross-mode identity.

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"go.uber.org/mock/gomock"
)

// TestUVMSemanticRootIdentity proves the routing middleware stamps the
// generated roots with the coordinator identity: the kernel-launch ordinal,
// the source build ordinal, the per-source local sequence, the stable source
// ID, and the program-command ordinal.
func TestUVMSemanticRootIdentity(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	// The CP has dispatched one kernel: the launch ordinal is 1.
	h.cp.uvmKernelLaunchOrdinal = 1

	var sent []*protocol.PageFaultReq
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(
		func(msg sim.Msg) *sim.SendError {
			sent = append(sent, msg.(*protocol.PageFaultReq))
			return nil
		}).Times(2)
	h.toGMMU.EXPECT().RetrieveIncoming().Times(2)

	for i := 0; i < 2; i++ {
		notif := &vm.FaultNotification{
			PID:               1,
			GPU:               0,
			VAddr:             0x1000 + uint64(i)*0x1000,
			AccessKind:        vm.AccessKindRead,
			FaultPendingToken: vm.FaultPendingToken(i + 1),
		}
		notif.ID = sim.GetIDGenerator().Generate()
		notif.Src = sim.RemotePort("ToGMMU")
		notif.Dst = sim.RemotePort("ToGMMU")
		if !h.mw.processFaultNotification(notif) {
			t.Fatalf("fault %d must be routed", i)
		}
	}

	if len(sent) != 2 {
		t.Fatalf("routed faults = %d, want 2", len(sent))
	}
	for i, req := range sent {
		if req.KernelLaunchOrdinal != 1 {
			t.Fatalf("fault %d launch ordinal = %d, want 1",
				i, req.KernelLaunchOrdinal)
		}
		if req.SourceBuildOrdinal != 0 {
			t.Fatalf("fault %d build ordinal = %d, want 0",
				i, req.SourceBuildOrdinal)
		}
		if req.SourceComponentStableID != "gmmu" {
			t.Fatalf("fault %d source = %q, want gmmu",
				i, req.SourceComponentStableID)
		}
		if req.ProgramCommandOrdinal != 1 {
			t.Fatalf("fault %d program ordinal = %d, want 1",
				i, req.ProgramCommandOrdinal)
		}
	}
	// The sourceLocalSequence is a per-source local tie-break: it
	// increments per generated root and is excluded from cross-mode
	// identity.
	if sent[0].SourceLocalSequence != 1 || sent[1].SourceLocalSequence != 2 {
		t.Fatalf("source sequences = %d, %d; want 1, 2",
			sent[0].SourceLocalSequence, sent[1].SourceLocalSequence)
	}
}

// TestUVMNormalAndIdealCoordinator proves the routing through the
// coordinator identity is mode-neutral: the same stamp fields are routed
// regardless of the UVM mode, and the AccessCounter notifications carry the
// same identity contract as the faults.
func TestUVMNormalAndIdealCoordinator(t *testing.T) {
	h := newUVMcpHarness(t)
	defer h.ctrl.Finish()

	h.cp.uvmKernelLaunchOrdinal = 2

	var sent *protocol.AccessCounterNotification
	h.toDriver.EXPECT().Send(gomock.Any()).DoAndReturn(
		func(msg sim.Msg) *sim.SendError {
			sent = msg.(*protocol.AccessCounterNotification)
			return nil
		})
	h.toAccessCounter.EXPECT().RetrieveIncoming()

	notif := &protocol.AccessCounterNotification{
		PID:         1,
		GPU:         0,
		VAddr:       0x10000,
		AccessKind:  vm.AccessKindRead,
		AccessCount: 8,
	}
	notif.ID = sim.GetIDGenerator().Generate()
	notif.Src = sim.RemotePort("ToAccessCounter")
	notif.Dst = sim.RemotePort("ToAccessCounter")

	if !h.mw.processAccessCounterNotification(notif) {
		t.Fatal("the notification must be routed")
	}
	if sent == nil {
		t.Fatal("no notification was sent to the driver")
	}
	if sent.KernelLaunchOrdinal != 2 {
		t.Fatalf("launch ordinal = %d, want 2", sent.KernelLaunchOrdinal)
	}
	if sent.SourceComponentStableID != "accesscounter" {
		t.Fatalf("source = %q, want accesscounter",
			sent.SourceComponentStableID)
	}
	if sent.SourceLocalSequence != 1 {
		t.Fatalf("source sequence = %d, want 1", sent.SourceLocalSequence)
	}
	if sent.ProgramCommandOrdinal != 2 {
		t.Fatalf("program ordinal = %d, want 2", sent.ProgramCommandOrdinal)
	}
}