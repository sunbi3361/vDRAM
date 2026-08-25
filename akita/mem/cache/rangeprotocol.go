package cache

// sbin_codex: address-range scoped cache maintenance. Unlike FlushReq, a
// RangeFlushReq never pauses the cache and never touches lines outside the
// requested range, so unrelated memory traffic keeps flowing. The UVM control
// path uses it for the mandatory 64KB writeback+invalidate before an eviction.

import (
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// RangeFlushReq asks a cache to write back and/or invalidate every line that
// belongs to one UVM region.
//
// A cache in this simulator may be tagged virtually (blocks carry the real PID
// and a virtual tag) or physically (blocks carry PID 0 and a physical tag), and
// the 4KB frames backing a 64KB virtual region are not necessarily contiguous
// in physical memory. The request therefore carries both forms: the virtual
// range [StartVAddr, StartVAddr+Size) and the list of physical frames covering
// it. Use MatchesBlock to test a block against either.
type RangeFlushReq struct {
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
func (r *RangeFlushReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a cloned RangeFlushReq with a different ID.
func (r *RangeFlushReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates the matching response.
func (r *RangeFlushReq) GenerateRsp() sim.Rsp {
	return RangeFlushRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		Build()
}

// MatchesBlock reports whether a cache block identified by its stored PID and
// tag belongs to the region this request targets.
//
// Both tagging schemes are accepted rather than inferred from the PID: a
// virtually tagged cache stores the real PID with a virtual tag, a physically
// tagged one stores PID 0 with a physical tag, and a single GPU can host both.
// Testing the union keeps the operation correct either way; the worst case is
// invalidating an unrelated clean line, which only costs a refetch.
// sbin_codex
func (r *RangeFlushReq) MatchesBlock(pid vm.PID, tag uint64) bool {
	if pid == r.PID && tag >= r.StartVAddr && tag < r.StartVAddr+r.Size {
		return true
	}

	return r.matchesPhysical(tag)
}

// MatchesAddress reports whether an in-flight request address falls inside the
// region, using the same virtual/physical rules as MatchesBlock.
func (r *RangeFlushReq) MatchesAddress(pid vm.PID, addr uint64) bool {
	return r.MatchesBlock(pid, addr)
}

func (r *RangeFlushReq) matchesPhysical(addr uint64) bool {
	if r.PageSize == 0 {
		return false
	}

	frame := addr / r.PageSize * r.PageSize
	for _, pAddr := range r.PAddrs {
		if pAddr == frame {
			return true
		}
	}

	return false
}

// RangeFlushReqBuilder builds range flush requests.
type RangeFlushReqBuilder struct {
	src, dst   sim.RemotePort
	pid        vm.PID
	startVAddr uint64
	size       uint64
	pAddrs     []uint64
	pageSize   uint64
	writeback  bool
	invalidate bool
}

// WithSrc sets the source of the message to build.
func (b RangeFlushReqBuilder) WithSrc(src sim.RemotePort) RangeFlushReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the message to build.
func (b RangeFlushReqBuilder) WithDst(dst sim.RemotePort) RangeFlushReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the address space of the region.
func (b RangeFlushReqBuilder) WithPID(pid vm.PID) RangeFlushReqBuilder {
	b.pid = pid
	return b
}

// WithVirtualRange sets the virtual address range of the region.
func (b RangeFlushReqBuilder) WithVirtualRange(
	startVAddr, size uint64,
) RangeFlushReqBuilder {
	b.startVAddr = startVAddr
	b.size = size

	return b
}

// WithPhysicalFrames sets the physical frames backing the region.
func (b RangeFlushReqBuilder) WithPhysicalFrames(
	pAddrs []uint64,
	pageSize uint64,
) RangeFlushReqBuilder {
	b.pAddrs = pAddrs
	b.pageSize = pageSize

	return b
}

// Writeback asks the cache to write dirty matching lines back.
func (b RangeFlushReqBuilder) Writeback() RangeFlushReqBuilder {
	b.writeback = true
	return b
}

// Invalidate asks the cache to invalidate matching lines.
func (b RangeFlushReqBuilder) Invalidate() RangeFlushReqBuilder {
	b.invalidate = true
	return b
}

// Build creates a new RangeFlushReq.
func (b RangeFlushReqBuilder) Build() *RangeFlushReq {
	r := &RangeFlushReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.StartVAddr = b.startVAddr
	r.Size = b.size
	r.PAddrs = b.pAddrs
	r.PageSize = b.pageSize
	r.Writeback = b.writeback
	r.Invalidate = b.invalidate
	r.TrafficClass = reflect.TypeOf(RangeFlushReq{}).String()

	return r
}

// RangeFlushRsp acknowledges a completed range flush.
type RangeFlushRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (r *RangeFlushRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a cloned RangeFlushRsp with a different ID.
func (r *RangeFlushRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that this response replies to.
func (r *RangeFlushRsp) GetRspTo() string {
	return r.RspTo
}

// RangeFlushRspBuilder builds range flush responses.
type RangeFlushRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the response to build.
func (b RangeFlushRspBuilder) WithSrc(src sim.RemotePort) RangeFlushRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build.
func (b RangeFlushRspBuilder) WithDst(dst sim.RemotePort) RangeFlushRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request that this response replies to.
func (b RangeFlushRspBuilder) WithRspTo(id string) RangeFlushRspBuilder {
	b.rspTo = id
	return b
}

// Build creates a new RangeFlushRsp.
func (b RangeFlushRspBuilder) Build() *RangeFlushRsp {
	r := &RangeFlushRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	r.TrafficClass = reflect.TypeOf(RangeFlushRsp{}).String()

	return r
}
