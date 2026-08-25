package tlb

import (
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// A FlushReq asks the TLB to invalidate certain entries. It will also not block
// all incoming and outgoing ports
type FlushReq struct {
	sim.MsgMeta

	VAddr []uint64
	PID   vm.PID
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

// FlushReqBuilder can build AT flush requests
type FlushReqBuilder struct {
	src, dst sim.RemotePort
	vAddrs   []uint64
	pid      vm.PID
}

// WithSrc sets the source of the request to build.
func (b FlushReqBuilder) WithSrc(src sim.RemotePort) FlushReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b FlushReqBuilder) WithDst(dst sim.RemotePort) FlushReqBuilder {
	b.dst = dst
	return b
}

// WithVAddrs sets the Vaddr of the pages to be flushed
func (b FlushReqBuilder) WithVAddrs(vAddrs []uint64) FlushReqBuilder {
	b.vAddrs = vAddrs
	return b
}

// WithPID sets the pid whose entries are to be flushed
func (b FlushReqBuilder) WithPID(pid vm.PID) FlushReqBuilder {
	b.pid = pid
	return b
}

// Build creates a new TLBFlushReq
func (b FlushReqBuilder) Build() *FlushReq {
	r := &FlushReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.VAddr = b.vAddrs
	r.PID = b.pid
	r.TrafficClass = reflect.TypeOf(FlushReq{}).String()

	return r
}

// A FlushRsp is a response from AT indicating flush is complete
type FlushRsp struct {
	sim.MsgMeta
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

// FlushRspBuilder can build AT flush rsp
type FlushRspBuilder struct {
	src, dst sim.RemotePort
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

// Build creates a new TLBFlushRsps.
func (b FlushRspBuilder) Build() *FlushRsp {
	r := &FlushRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficClass = reflect.TypeOf(FlushReq{}).String()

	return r
}

// An InvalidateRangeReq asks the TLB to invalidate every entry of one PID whose
// covered virtual page overlaps [StartVA, StartVA+Size). Unlike FlushReq it is
// non-stalling: the TLB stays in its current state, keeps accepting
// translations, and only the matching entries are dropped. UVM uses it for the
// mandatory 64KB range invalidation. // sbin_codex
type InvalidateRangeReq struct {
	sim.MsgMeta

	PID     vm.PID
	StartVA uint64
	Size    uint64
}

// Meta returns the meta data associated with the message. // sbin_codex
func (r *InvalidateRangeReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a cloned InvalidateRangeReq with a different ID. // sbin_codex
func (r *InvalidateRangeReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// InvalidateRangeReqBuilder builds range invalidation requests. // sbin_codex
type InvalidateRangeReqBuilder struct {
	src, dst sim.RemotePort
	pid      vm.PID
	startVA  uint64
	size     uint64
}

// WithSrc sets the source of the request to build. // sbin_codex
func (b InvalidateRangeReqBuilder) WithSrc(
	src sim.RemotePort,
) InvalidateRangeReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build. // sbin_codex
func (b InvalidateRangeReqBuilder) WithDst(
	dst sim.RemotePort,
) InvalidateRangeReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the address space whose entries are invalidated. // sbin_codex
func (b InvalidateRangeReqBuilder) WithPID(
	pid vm.PID,
) InvalidateRangeReqBuilder {
	b.pid = pid
	return b
}

// WithRange sets the virtual address range to invalidate. // sbin_codex
func (b InvalidateRangeReqBuilder) WithRange(
	startVA, size uint64,
) InvalidateRangeReqBuilder {
	b.startVA = startVA
	b.size = size

	return b
}

// Build creates a new InvalidateRangeReq. // sbin_codex
func (b InvalidateRangeReqBuilder) Build() *InvalidateRangeReq {
	r := &InvalidateRangeReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.StartVA = b.startVA
	r.Size = b.size
	r.TrafficClass = reflect.TypeOf(InvalidateRangeReq{}).String()

	return r
}

// An InvalidateRangeRsp acknowledges a completed range invalidation.
// sbin_codex
type InvalidateRangeRsp struct {
	sim.MsgMeta

	RespondTo string
}

// Meta returns the meta data associated with the message. // sbin_codex
func (r *InvalidateRangeRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a cloned InvalidateRangeRsp with a different ID. // sbin_codex
func (r *InvalidateRangeRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to. // sbin_codex
func (r *InvalidateRangeRsp) GetRspTo() string {
	return r.RespondTo
}

// InvalidateRangeRspBuilder builds range invalidation responses. // sbin_codex
type InvalidateRangeRspBuilder struct {
	src, dst  sim.RemotePort
	respondTo string
}

// WithSrc sets the source of the response to build. // sbin_codex
func (b InvalidateRangeRspBuilder) WithSrc(
	src sim.RemotePort,
) InvalidateRangeRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build. // sbin_codex
func (b InvalidateRangeRspBuilder) WithDst(
	dst sim.RemotePort,
) InvalidateRangeRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request that this response replies to. // sbin_codex
func (b InvalidateRangeRspBuilder) WithRspTo(
	id string,
) InvalidateRangeRspBuilder {
	b.respondTo = id
	return b
}

// Build creates a new InvalidateRangeRsp. // sbin_codex
func (b InvalidateRangeRspBuilder) Build() *InvalidateRangeRsp {
	r := &InvalidateRangeRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RespondTo = b.respondTo
	r.TrafficClass = reflect.TypeOf(InvalidateRangeRsp{}).String()

	return r
}

// A RestartReq is a request to TLB to start accepting requests and resume
// operations
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

// RestartReqBuilder can build TLB restart requests.
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

// Build creates a new TLBRestartReq.
func (b RestartReqBuilder) Build() *RestartReq {
	r := &RestartReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficClass = reflect.TypeOf(RestartReq{}).String()

	return r
}

// A RestartRsp is a response from AT indicating it has resumed working
type RestartRsp struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (r *RestartRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned RestartRsp with different ID
func (r *RestartRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// RestartRspBuilder can build AT flush rsp
type RestartRspBuilder struct {
	src, dst sim.RemotePort
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

// Build creates a new TLBRestartRsp
func (b RestartRspBuilder) Build() *RestartRsp {
	r := &RestartRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.TrafficClass = reflect.TypeOf(RestartReq{}).String()

	return r
}
