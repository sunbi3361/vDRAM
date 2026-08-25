package vm

// sbin_codex: UVM control-plane messages exchanged between the host UVM driver
// and the GPU. The driver never talks to GPU-internal components directly: the
// Command Processor is the GPU-side endpoint and forwards these to the GMMU,
// which coordinates the TLB hierarchy and owns the replayable-fault queue.
//
// None of these operations may quiesce the GPU. They are scoped to one UVM
// region (64KB by default) and leave unrelated translations untouched.

import (
	"reflect"

	"github.com/sarchlab/akita/v4/sim"
)

// UVMTLBInvalidateReq asks the GPU to invalidate every cached translation of
// one address space whose page overlaps [StartVA, StartVA+Size). It is
// mandatory for a REMOTE -> GPU_LOCAL transition because a valid remote
// translation may be cached in the L2 TLB.
type UVMTLBInvalidateReq struct {
	sim.MsgMeta

	PID      PID
	StartVA  uint64
	Size     uint64
	DeviceID uint64
}

// Meta returns the meta data associated with the message.
func (r *UVMTLBInvalidateReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the request with a different ID.
func (r *UVMTLBInvalidateReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewUVMTLBInvalidateReq creates a new UVMTLBInvalidateReq.
func NewUVMTLBInvalidateReq(src, dst sim.RemotePort) *UVMTLBInvalidateReq {
	cmd := new(UVMTLBInvalidateReq)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(UVMTLBInvalidateReq{}).String()

	return cmd
}

// UVMTLBInvalidateRsp reports that every TLB level acknowledged the range
// invalidation.
type UVMTLBInvalidateRsp struct {
	sim.MsgMeta

	RespondTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMTLBInvalidateRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the response with a different ID.
func (r *UVMTLBInvalidateRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to.
func (r *UVMTLBInvalidateRsp) GetRspTo() string {
	return r.RespondTo
}

// NewUVMTLBInvalidateRsp creates a new UVMTLBInvalidateRsp.
func NewUVMTLBInvalidateRsp(
	src, dst sim.RemotePort,
	respondTo string,
) *UVMTLBInvalidateRsp {
	cmd := new(UVMTLBInvalidateRsp)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src
	cmd.Dst = dst
	cmd.RespondTo = respondTo
	cmd.TrafficClass = reflect.TypeOf(UVMTLBInvalidateRsp{}).String()

	return cmd
}

// UVMFaultReplayReq tells the GPU that one UVM region became usable again. The
// GMMU re-runs translation for every stalled request inside the range and
// re-injects it into the memory path. It replaces the GPU-wide restart used by
// the legacy migration path.
type UVMFaultReplayReq struct {
	sim.MsgMeta

	PID      PID
	StartVA  uint64
	Size     uint64
	DeviceID uint64
	// Refused reports that the driver could not make the region GPU-local,
	// typically because GPU capacity is exhausted and eviction is unavailable.
	// A request stalled purely to force migration must then be completed the
	// ordinary way instead of waiting for a mapping that will not arrive.
	// sbin_codex
	Refused bool
}

// Meta returns the meta data associated with the message.
func (r *UVMFaultReplayReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the request with a different ID.
func (r *UVMFaultReplayReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewUVMFaultReplayReq creates a new UVMFaultReplayReq.
func NewUVMFaultReplayReq(src, dst sim.RemotePort) *UVMFaultReplayReq {
	cmd := new(UVMFaultReplayReq)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(UVMFaultReplayReq{}).String()

	return cmd
}

// UVMRemoteRetryRsp tells an address translator that one request it routed to
// remote host memory was not performed and must be translated again. The UVM
// access counter uses it to release a write it stalled on a CPU-remote page:
// by the time it is sent, the page is GPU-local and the stale remote
// translation has been invalidated, so the retry lands in GPU memory.
type UVMRemoteRetryRsp struct {
	sim.MsgMeta

	RespondTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMRemoteRetryRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the response with a different ID.
func (r *UVMRemoteRetryRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to.
func (r *UVMRemoteRetryRsp) GetRspTo() string {
	return r.RespondTo
}

// NewUVMRemoteRetryRsp creates a new UVMRemoteRetryRsp.
func NewUVMRemoteRetryRsp(
	src, dst sim.RemotePort,
	respondTo string,
) *UVMRemoteRetryRsp {
	rsp := new(UVMRemoteRetryRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src
	rsp.Dst = dst
	rsp.RespondTo = respondTo
	rsp.TrafficClass = reflect.TypeOf(UVMRemoteRetryRsp{}).String()

	return rsp
}

// UVMDrainRangeReq asks an address translator to report when it has no
// outstanding memory request left inside one UVM region.
//
// It is what makes a region-scoped eviction safe. Once the driver has
// invalidated the region's translations, no new request for it can be issued,
// but requests translated a moment earlier may still be travelling toward the
// caches. Waiting for every translator to drain the range guarantees those
// stores have committed before the cache writeback runs, so the D2H copy of a
// victim can never miss one. The translator keeps serving unrelated addresses
// throughout.
type UVMDrainRangeReq struct {
	sim.MsgMeta

	PID     PID
	StartVA uint64
	Size    uint64
}

// Meta returns the meta data associated with the message.
func (r *UVMDrainRangeReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the request with a different ID.
func (r *UVMDrainRangeReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewUVMDrainRangeReq creates a new UVMDrainRangeReq.
func NewUVMDrainRangeReq(src, dst sim.RemotePort) *UVMDrainRangeReq {
	req := new(UVMDrainRangeReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src
	req.Dst = dst
	req.TrafficClass = reflect.TypeOf(UVMDrainRangeReq{}).String()

	return req
}

// UVMDrainRangeRsp reports that a translator has drained one region.
type UVMDrainRangeRsp struct {
	sim.MsgMeta

	RespondTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMDrainRangeRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the response with a different ID.
func (r *UVMDrainRangeRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to.
func (r *UVMDrainRangeRsp) GetRspTo() string {
	return r.RespondTo
}

// NewUVMDrainRangeRsp creates a new UVMDrainRangeRsp.
func NewUVMDrainRangeRsp(
	src, dst sim.RemotePort,
	respondTo string,
) *UVMDrainRangeRsp {
	rsp := new(UVMDrainRangeRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src
	rsp.Dst = dst
	rsp.RespondTo = respondTo
	rsp.TrafficClass = reflect.TypeOf(UVMDrainRangeRsp{}).String()

	return rsp
}
