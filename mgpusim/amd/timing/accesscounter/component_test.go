// sbin_codex: Behavioral tests for the PCIe-visible access-counter proxy.
package accesscounter

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

const (
	testCPU    sim.RemotePort = "CPU.Memory"
	testDriver sim.RemotePort = "Driver.AccessCounter"
	testClient sim.RemotePort = "GPU.PCIe"
)

type testConnection struct {
	sim.HookableBase
	name string
}

func (c *testConnection) Name() string             { return c.name }
func (c *testConnection) PlugIn(port sim.Port)     { port.SetConnection(c) }
func (c *testConnection) Unplug(sim.Port)          {}
func (c *testConnection) NotifyAvailable(sim.Port) {}
func (c *testConnection) NotifySend()              {}

func newTestComponent(threshold uint64, bufferSize int) *Comp {
	c := MakeBuilder().
		WithEngine(sim.NewSerialEngine()).
		WithThreshold(threshold).
		WithBufferSize(bufferSize).
		WithBottomDestination(testCPU).
		WithDriverDestination(testDriver).
		Build("AccessCounter")
	(&testConnection{name: "TopConnection"}).PlugIn(c.Top)
	(&testConnection{name: "BottomConnection"}).PlugIn(c.Bottom)
	return c
}

func markedRead(c *Comp, pid vm.PID, vAddr uint64, deviceID uint64) *mem.ReadReq {
	return mem.ReadReqBuilder{}.
		WithSrc(testClient).
		WithDst(c.Top.AsRemote()).
		WithAddress(0x80000000).
		WithByteSize(64).
		WithRemoteDemandInfo(mem.RemoteDemandInfo{
			PID: pid, VAddr: vAddr, DeviceID: deviceID,
		}).
		Build()
}

func markedWrite(c *Comp, pid vm.PID, vAddr uint64, deviceID uint64) *mem.WriteReq {
	return mem.WriteReqBuilder{}.
		WithSrc(testClient).
		WithDst(c.Top.AsRemote()).
		WithAddress(0x80000040).
		WithData([]byte{1, 2, 3, 4}).
		WithRemoteDemandInfo(mem.RemoteDemandInfo{
			PID: pid, VAddr: vAddr, DeviceID: deviceID,
		}).
		Build()
}

func deliver(t *testing.T, port sim.Port, msg sim.Msg) {
	t.Helper()
	if err := port.Deliver(msg); err != nil {
		t.Fatalf("deliver failed: %v", err)
	}
}

func outgoing(t *testing.T, port sim.Port) sim.Msg {
	t.Helper()
	msg := port.RetrieveOutgoing()
	if msg == nil {
		t.Fatal("expected outgoing message")
	}
	return msg
}

func Test_Comp_forwards_marked_read_and_counts_after_acceptance(t *testing.T) {
	// Given
	c := newTestComponent(8, 4)
	req := markedRead(c, vm.PID(7), 0x12345, 3)
	deliver(t, c.Top, req)

	// When
	c.Tick()

	// Then
	forwarded, ok := outgoing(t, c.Bottom).(*mem.ReadReq)
	if !ok {
		t.Fatalf("expected read request, got %T", forwarded)
	}
	if forwarded.Src != c.Bottom.AsRemote() || forwarded.Dst != testCPU {
		t.Fatalf("unexpected route: %s -> %s", forwarded.Src, forwarded.Dst)
	}
	assertRegion(t, c, RegionKey{PID: vm.PID(7), RegionBase: 0x10000}, 1, false)
}

func Test_Comp_forwards_marked_write_and_counts_identically(t *testing.T) {
	// Given
	c := newTestComponent(8, 4)
	req := markedWrite(c, vm.PID(7), 0x1ffff, 3)
	deliver(t, c.Top, req)

	// When
	c.Tick()

	// Then
	forwarded, ok := outgoing(t, c.Bottom).(*mem.WriteReq)
	if !ok {
		t.Fatalf("expected write request, got %T", forwarded)
	}
	if forwarded.Src != c.Bottom.AsRemote() || forwarded.Dst != testCPU {
		t.Fatalf("unexpected route: %s -> %s", forwarded.Src, forwarded.Dst)
	}
	assertRegion(t, c, RegionKey{PID: vm.PID(7), RegionBase: 0x10000}, 1, false)
}

func Test_Comp_forwards_unmarked_request_without_counting(t *testing.T) {
	// Given
	c := newTestComponent(1, 4)
	req := mem.ReadReqBuilder{}.
		WithSrc(testClient).
		WithDst(c.Top.AsRemote()).
		WithAddress(0x2000).
		WithByteSize(64).
		Build()
	deliver(t, c.Top, req)

	// When
	c.Tick()

	// Then
	_ = outgoing(t, c.Bottom)
	if got := c.Snapshot(); len(got.Regions) != 0 {
		t.Fatalf("unmarked request changed counters: %+v", got)
	}
	if got := c.Top.RetrieveOutgoing(); got != nil {
		t.Fatalf("unmarked request emitted notification: %T", got)
	}
}

func Test_Comp_counts_only_once_after_bottom_backpressure_clears(t *testing.T) {
	// Given
	c := newTestComponent(8, 1)
	blocker := mem.ReadReqBuilder{}.
		WithSrc(c.Bottom.AsRemote()).
		WithDst(testCPU).
		WithAddress(0).
		WithByteSize(1).
		Build()
	if err := c.Bottom.Send(blocker); err != nil {
		t.Fatalf("prefill bottom failed: %v", err)
	}
	deliver(t, c.Top, markedRead(c, vm.PID(1), 0x22000, 2))

	// When
	c.Tick()
	if got := c.Snapshot(); len(got.Regions) != 0 {
		t.Fatalf("backpressured request counted early: %+v", got)
	}
	_ = outgoing(t, c.Bottom)
	c.Tick()
	c.Tick()

	// Then
	_ = outgoing(t, c.Bottom)
	assertRegion(t, c, RegionKey{PID: vm.PID(1), RegionBase: 0x20000}, 1, false)
}

func Test_Comp_forwards_response_to_original_requester_with_original_correlation(t *testing.T) {
	// Given
	c := newTestComponent(8, 4)
	original := markedRead(c, vm.PID(4), 0x30000, 1)
	deliver(t, c.Top, original)
	c.Tick()
	forwarded := outgoing(t, c.Bottom).(*mem.ReadReq)
	rsp := mem.DataReadyRspBuilder{}.
		WithSrc(testCPU).
		WithDst(c.Bottom.AsRemote()).
		WithRspTo(forwarded.ID).
		WithData([]byte{9, 8, 7}).
		Build()
	deliver(t, c.Bottom, rsp)

	// When
	c.Tick()

	// Then
	returned, ok := outgoing(t, c.Top).(*mem.DataReadyRsp)
	if !ok {
		t.Fatalf("expected data response, got %T", returned)
	}
	if returned.GetRspTo() != original.ID || returned.Dst != original.Src {
		t.Fatalf("response correlation lost: rspTo=%s dst=%s", returned.GetRspTo(), returned.Dst)
	}
}
