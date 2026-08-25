// sbin_codex: UVM control messages that only the host driver and the GPU
// Command Processor exchange. Messages the GMMU must also understand live in
// akita's vm package instead.
package protocol

import (
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// UVMCacheRangeFlushReq asks the GPU to write back and invalidate every cache
// line belonging to one UVM region. It is required before a GPU_LOCAL ->
// REMOTE/INVALID transition so that dirty data reaches HBM before the D2H
// copy. It is region-scoped: unrelated cache lines and unrelated memory
// traffic continue normally.
type UVMCacheRangeFlushReq struct {
	sim.MsgMeta

	PID        vm.PID
	StartVAddr uint64
	Size       uint64
	PAddrs     []uint64
	PageSize   uint64
	Writeback  bool
	Invalidate bool
}

// Meta returns the meta data associated with the message.
func (r *UVMCacheRangeFlushReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the request with a different ID.
func (r *UVMCacheRangeFlushReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewUVMCacheRangeFlushReq creates a writeback+invalidate request for one
// region.
func NewUVMCacheRangeFlushReq(
	src, dst sim.RemotePort,
	pid vm.PID,
	startVAddr, size uint64,
	pAddrs []uint64,
	pageSize uint64,
) *UVMCacheRangeFlushReq {
	req := new(UVMCacheRangeFlushReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src
	req.Dst = dst
	req.PID = pid
	req.StartVAddr = startVAddr
	req.Size = size
	req.PAddrs = pAddrs
	req.PageSize = pageSize
	req.Writeback = true
	req.Invalidate = true
	req.TrafficClass = reflect.TypeOf(UVMCacheRangeFlushReq{}).String()

	return req
}

// UVMCacheRangeFlushRsp reports that every cache acknowledged the region
// operation.
type UVMCacheRangeFlushRsp struct {
	sim.MsgMeta

	RespondTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMCacheRangeFlushRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the response with a different ID.
func (r *UVMCacheRangeFlushRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to.
func (r *UVMCacheRangeFlushRsp) GetRspTo() string {
	return r.RespondTo
}

// NewUVMCacheRangeFlushRsp creates a new UVMCacheRangeFlushRsp.
func NewUVMCacheRangeFlushRsp(
	src, dst sim.RemotePort,
	respondTo string,
) *UVMCacheRangeFlushRsp {
	rsp := new(UVMCacheRangeFlushRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src
	rsp.Dst = dst
	rsp.RespondTo = respondTo
	rsp.TrafficClass = reflect.TypeOf(UVMCacheRangeFlushRsp{}).String()

	return rsp
}

// UVMRemoteDrainReq asks the GPU to report when no remote memory access to one
// UVM region is outstanding any more.
//
// It closes a lost-update window on the admission path. A remote write that the
// driver declined to migrate is performed over PCIe and lands in host memory
// asynchronously. If the region were admitted to the GPU in the meantime, the
// H2D copy would snapshot host memory before that store arrived, the GPU copy
// would miss it, and the eventual eviction would write the stale GPU frame back
// over the correct host data. Draining first makes the snapshot authoritative.
type UVMRemoteDrainReq struct {
	sim.MsgMeta

	PID        vm.PID
	StartVAddr uint64
	Size       uint64
}

// Meta returns the meta data associated with the message.
func (r *UVMRemoteDrainReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the request with a different ID.
func (r *UVMRemoteDrainReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewUVMRemoteDrainReq creates a new UVMRemoteDrainReq.
func NewUVMRemoteDrainReq(
	src, dst sim.RemotePort,
	pid vm.PID,
	startVAddr, size uint64,
) *UVMRemoteDrainReq {
	req := new(UVMRemoteDrainReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src
	req.Dst = dst
	req.PID = pid
	req.StartVAddr = startVAddr
	req.Size = size
	req.TrafficClass = reflect.TypeOf(UVMRemoteDrainReq{}).String()

	return req
}

// UVMRemoteDrainRsp reports that a region has no outstanding remote access.
type UVMRemoteDrainRsp struct {
	sim.MsgMeta

	RespondTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMRemoteDrainRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the response with a different ID.
func (r *UVMRemoteDrainRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to.
func (r *UVMRemoteDrainRsp) GetRspTo() string {
	return r.RespondTo
}

// NewUVMRemoteDrainRsp creates a new UVMRemoteDrainRsp.
func NewUVMRemoteDrainRsp(
	src, dst sim.RemotePort,
	respondTo string,
) *UVMRemoteDrainRsp {
	rsp := new(UVMRemoteDrainRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src
	rsp.Dst = dst
	rsp.RespondTo = respondTo
	rsp.TrafficClass = reflect.TypeOf(UVMRemoteDrainRsp{}).String()

	return rsp
}
