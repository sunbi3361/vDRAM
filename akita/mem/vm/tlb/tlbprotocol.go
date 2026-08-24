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

// sbin_codex: UVM range TLB invalidation control (todo 2 of plan
// mgpusim-uvm-manager). The GMMU is the invalidation coordinator: it
// broadcasts the request to the shared L2 TLB and every private L1 TLB and
// returns one aggregated response (uvm-manager.md §21.1).

// UVMTLBInvalidateReq asks a TLB to invalidate every entry for the given
// PID/ASID whose covered VA range overlaps the requested 64 KB region.
// Unrelated entries and unrelated translation requests remain active; no
// full-TLB flush is permitted for ordinary UVM migration.
type UVMTLBInvalidateReq struct {
	sim.MsgMeta

	PID     vm.PID
	StartVA uint64
	Size    uint64
}

// Meta returns the meta data associated with the message.
func (r *UVMTLBInvalidateReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned UVMTLBInvalidateReq with different ID
func (r *UVMTLBInvalidateReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a UVMTLBInvalidateRsp addressed back to the requester.
func (r *UVMTLBInvalidateReq) GenerateRsp() sim.Rsp {
	rsp := UVMTLBInvalidateRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		Build()

	return rsp
}

// UVMTLBInvalidateReqBuilder can build UVM range TLB invalidation requests.
type UVMTLBInvalidateReqBuilder struct {
	src, dst sim.RemotePort
	pid      vm.PID
	startVA  uint64
	size     uint64
}

// WithSrc sets the source of the request to build.
func (b UVMTLBInvalidateReqBuilder) WithSrc(src sim.RemotePort) UVMTLBInvalidateReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b UVMTLBInvalidateReqBuilder) WithDst(dst sim.RemotePort) UVMTLBInvalidateReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID/ASID whose entries are to be invalidated.
func (b UVMTLBInvalidateReqBuilder) WithPID(pid vm.PID) UVMTLBInvalidateReqBuilder {
	b.pid = pid
	return b
}

// WithStartVA sets the start of the virtual range to invalidate.
func (b UVMTLBInvalidateReqBuilder) WithStartVA(startVA uint64) UVMTLBInvalidateReqBuilder {
	b.startVA = startVA
	return b
}

// WithSize sets the size of the virtual range to invalidate (64 KB for UVM).
func (b UVMTLBInvalidateReqBuilder) WithSize(size uint64) UVMTLBInvalidateReqBuilder {
	b.size = size
	return b
}

// Build creates a new UVMTLBInvalidateReq
func (b UVMTLBInvalidateReqBuilder) Build() *UVMTLBInvalidateReq {
	r := &UVMTLBInvalidateReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.StartVA = b.startVA
	r.Size = b.size
	r.TrafficClass = reflect.TypeOf(UVMTLBInvalidateReq{}).String()

	return r
}

// UVMTLBInvalidateRsp is the aggregated completion response for a
// UVMTLBInvalidateReq.
type UVMTLBInvalidateRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (r *UVMTLBInvalidateRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned UVMTLBInvalidateRsp with different ID
func (r *UVMTLBInvalidateRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (r *UVMTLBInvalidateRsp) GetRspTo() string {
	return r.RspTo
}

// UVMTLBInvalidateRspBuilder can build UVM range TLB invalidation responses.
type UVMTLBInvalidateRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the response to build.
func (b UVMTLBInvalidateRspBuilder) WithSrc(src sim.RemotePort) UVMTLBInvalidateRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build.
func (b UVMTLBInvalidateRspBuilder) WithDst(dst sim.RemotePort) UVMTLBInvalidateRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID that the response replies to.
func (b UVMTLBInvalidateRspBuilder) WithRspTo(rspTo string) UVMTLBInvalidateRspBuilder {
	b.rspTo = rspTo
	return b
}

// Build creates a new UVMTLBInvalidateRsp
func (b UVMTLBInvalidateRspBuilder) Build() *UVMTLBInvalidateRsp {
	r := &UVMTLBInvalidateRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	r.TrafficClass = reflect.TypeOf(UVMTLBInvalidateReq{}).String()

	return r
}
