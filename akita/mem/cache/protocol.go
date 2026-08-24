package cache

import (
	"reflect"

	"github.com/rs/xid"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// FlushReq is the request send to a cache unit to request it to flush all
// the cache lines.
type FlushReq struct {
	sim.MsgMeta

	InvalidateAllCachelines bool
	DiscardInflight         bool
	PauseAfterFlushing      bool
}

// Meta returns the meta data associated with the message.
func (r *FlushReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned FlushReq with different ID
func (r *FlushReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

func (r *FlushReq) GenerateRsp() sim.Rsp {
	rsp := FlushRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		Build()

	return rsp
}

// FlushReqBuilder can build flush requests.
type FlushReqBuilder struct {
	src, dst                sim.RemotePort
	invalidateAllCacheLines bool
	discardInflight         bool
	pauseAfterFlushing      bool
}

// WithSrc sets the source of the message to build
func (b FlushReqBuilder) WithSrc(src sim.RemotePort) FlushReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the message to build.
func (b FlushReqBuilder) WithDst(dst sim.RemotePort) FlushReqBuilder {
	b.dst = dst
	return b
}

// InvalidateAllCacheLines allows the flush request to build to invalidate
// all the cachelines in a cache unit.
func (b FlushReqBuilder) InvalidateAllCacheLines() FlushReqBuilder {
	b.invalidateAllCacheLines = true
	return b
}

// DiscardInflight allows the flush request to build to discard all inflight
// requests.
func (b FlushReqBuilder) DiscardInflight() FlushReqBuilder {
	b.discardInflight = true
	return b
}

// PauseAfterFlushing sets the flush request to build to pause the cache unit
// from processing future request until restart request is received.
func (b FlushReqBuilder) PauseAfterFlushing() FlushReqBuilder {
	b.pauseAfterFlushing = true
	return b
}

// Build creates a new FlushReq
func (b FlushReqBuilder) Build() *FlushReq {
	r := &FlushReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.InvalidateAllCachelines = b.invalidateAllCacheLines
	r.DiscardInflight = b.discardInflight
	r.PauseAfterFlushing = b.pauseAfterFlushing
	r.TrafficClass = reflect.TypeOf(FlushReq{}).String()

	return r
}

// FlushRsp is the respond sent from the a cache unit for finishing a cache
// flush
type FlushRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (r *FlushRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned FlushRsp with different ID
func (r *FlushRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

func (r *FlushRsp) GetRspTo() string {
	return r.RspTo
}

// FlushRspBuilder can build data ready responds.
type FlushRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the request to build.
func (b FlushRspBuilder) WithSrc(src sim.RemotePort) FlushRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b FlushRspBuilder) WithDst(dst sim.RemotePort) FlushRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b FlushRspBuilder) WithRspTo(id string) FlushRspBuilder {
	b.rspTo = id
	return b
}

// Build creates a new FlushRsp
func (b FlushRspBuilder) Build() *FlushRsp {
	r := &FlushRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	r.TrafficClass = reflect.TypeOf(FlushReq{}).String()

	return r
}

// RestartReq is the request send to a cache unit to request it unpause the
// cache
type RestartReq struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (r *RestartReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned RestartReq with different ID
func (r *RestartReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

func (r *RestartReq) GenerateRsp() sim.Rsp {
	rsp := RestartRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		Build()

	return rsp
}

// RestartReqBuilder can build data ready responds.
type RestartReqBuilder struct {
	src, dst sim.RemotePort
}

// WithSrc sets the source of the request to build.
func (b RestartReqBuilder) WithSrc(src sim.RemotePort) RestartReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b RestartReqBuilder) WithDst(dst sim.RemotePort) RestartReqBuilder {
	b.dst = dst
	return b
}

// Build creates a new RestartReq
func (b RestartReqBuilder) Build() *RestartReq {
	r := &RestartReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficClass = reflect.TypeOf(RestartReq{}).String()

	return r
}

// RestartRsp is the respond sent from the a cache unit for finishing a cache
// flush
type RestartRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (r *RestartRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned RestartRsp with different ID
func (r *RestartRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = xid.New().String()

	return &cloneMsg
}

func (r *RestartRsp) GetRspTo() string {
	return r.RspTo
}

// RestartRspBuilder can build data ready responds.
type RestartRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the request to build.
func (b RestartRspBuilder) WithSrc(src sim.RemotePort) RestartRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b RestartRspBuilder) WithDst(dst sim.RemotePort) RestartRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b RestartRspBuilder) WithRspTo(id string) RestartRspBuilder {
	b.rspTo = id
	return b
}

// Build creates a new RestartRsp
func (b RestartRspBuilder) Build() *RestartRsp {
	r := &RestartRsp{}
	r.ID = xid.New().String()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	r.TrafficClass = reflect.TypeOf(RestartReq{}).String()

	return r
}

// sbin_codex: UVM cache range flush control (todo 2 of plan
// mgpusim-uvm-manager). This defines the message contract only; the
// writeback/invalidate mechanics live in a later todo.

// UVMCacheRangeFlushOp describes the operation of a UVM cache range flush.
type UVMCacheRangeFlushOp uint8

const (
	// UVMCacheRangeFlushWritebackOnly writes back dirty lines without
	// invalidating them.
	UVMCacheRangeFlushWritebackOnly UVMCacheRangeFlushOp = iota
	// UVMCacheRangeFlushWritebackInvalidate writes back dirty lines and
	// invalidates them.
	UVMCacheRangeFlushWritebackInvalidate
)

// PhysicalRun describes a contiguous run of physical addresses to flush.
type PhysicalRun struct {
	Start  uint64
	Length uint64
}

// UVMCacheRangeFlushReq asks a cache to flush a virtual range owned by a
// managed process. The operation, PID, VA base, valid-page mask, and physical
// runs scope the flush.
type UVMCacheRangeFlushReq struct {
	sim.MsgMeta

	Operation     UVMCacheRangeFlushOp
	PID           vm.PID
	VABase        uint64
	ValidPageMask uint64
	PhysicalRuns  []PhysicalRun
}

// Meta returns the meta data associated with the message.
func (r *UVMCacheRangeFlushReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned UVMCacheRangeFlushReq with different ID
func (r *UVMCacheRangeFlushReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a UVMCacheRangeFlushRsp addressed back to the requester.
func (r *UVMCacheRangeFlushReq) GenerateRsp() sim.Rsp {
	rsp := UVMCacheRangeFlushRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		Build()

	return rsp
}

// UVMCacheRangeFlushReqBuilder can build UVM cache range flush requests.
type UVMCacheRangeFlushReqBuilder struct {
	src, dst      sim.RemotePort
	operation     UVMCacheRangeFlushOp
	pid           vm.PID
	vaBase        uint64
	validPageMask uint64
	physicalRuns  []PhysicalRun
}

// WithSrc sets the source of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithSrc(src sim.RemotePort) UVMCacheRangeFlushReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithDst(dst sim.RemotePort) UVMCacheRangeFlushReqBuilder {
	b.dst = dst
	return b
}

// WithOperation sets the flush operation of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithOperation(
	operation UVMCacheRangeFlushOp,
) UVMCacheRangeFlushReqBuilder {
	b.operation = operation
	return b
}

// WithPID sets the process ID of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithPID(pid vm.PID) UVMCacheRangeFlushReqBuilder {
	b.pid = pid
	return b
}

// WithVABase sets the virtual base address of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithVABase(vaBase uint64) UVMCacheRangeFlushReqBuilder {
	b.vaBase = vaBase
	return b
}

// WithValidPageMask sets the valid-page mask of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithValidPageMask(
	mask uint64,
) UVMCacheRangeFlushReqBuilder {
	b.validPageMask = mask
	return b
}

// WithPhysicalRuns sets the physical runs of the request to build.
func (b UVMCacheRangeFlushReqBuilder) WithPhysicalRuns(
	runs []PhysicalRun,
) UVMCacheRangeFlushReqBuilder {
	b.physicalRuns = runs
	return b
}

// Build creates a new UVMCacheRangeFlushReq
func (b UVMCacheRangeFlushReqBuilder) Build() *UVMCacheRangeFlushReq {
	r := &UVMCacheRangeFlushReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.Operation = b.operation
	r.PID = b.pid
	r.VABase = b.vaBase
	r.ValidPageMask = b.validPageMask
	r.PhysicalRuns = b.physicalRuns
	r.TrafficClass = reflect.TypeOf(UVMCacheRangeFlushReq{}).String()

	return r
}

// UVMCacheRangeFlushRsp is the completion response for a UVMCacheRangeFlushReq.
type UVMCacheRangeFlushRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMCacheRangeFlushRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned UVMCacheRangeFlushRsp with different ID
func (r *UVMCacheRangeFlushRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (r *UVMCacheRangeFlushRsp) GetRspTo() string {
	return r.RspTo
}

// UVMCacheRangeFlushRspBuilder can build UVM cache range flush responses.
type UVMCacheRangeFlushRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the response to build.
func (b UVMCacheRangeFlushRspBuilder) WithSrc(src sim.RemotePort) UVMCacheRangeFlushRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build.
func (b UVMCacheRangeFlushRspBuilder) WithDst(dst sim.RemotePort) UVMCacheRangeFlushRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID that the response replies to.
func (b UVMCacheRangeFlushRspBuilder) WithRspTo(rspTo string) UVMCacheRangeFlushRspBuilder {
	b.rspTo = rspTo
	return b
}

// Build creates a new UVMCacheRangeFlushRsp
func (b UVMCacheRangeFlushRspBuilder) Build() *UVMCacheRangeFlushRsp {
	r := &UVMCacheRangeFlushRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	r.TrafficClass = reflect.TypeOf(UVMCacheRangeFlushReq{}).String()

	return r
}
