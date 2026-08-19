package pagewalkcache

import (
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// LookupReq asks all cacheable page-table levels whether the address is
// present. // sbin_codex: one request probes every level in parallel.
type LookupReq struct {
	sim.MsgMeta

	PID   vm.PID
	VAddr uint64
}

// Meta returns the message metadata.
func (r *LookupReq) Meta() *sim.MsgMeta { return &r.MsgMeta }

// Clone returns a copy with a fresh message ID.
func (r *LookupReq) Clone() sim.Msg {
	clone := *r
	clone.ID = sim.GetIDGenerator().Generate()
	return &clone
}

// GenerateRsp creates an empty aggregate response. // sbin_codex
func (r *LookupReq) GenerateRsp() sim.Rsp {
	return &LookupRsp{
		MsgMeta: sim.MsgMeta{
			ID:           sim.GetIDGenerator().Generate(),
			Src:          r.Dst,
			Dst:          r.Src,
			TrafficClass: reflect.TypeOf(LookupRsp{}).String(),
		},
		RspTo: r.ID,
		PID:   r.PID,
		VAddr: r.VAddr,
		Level: -1,
	}
}

// LookupReqBuilder builds page-walk cache lookup requests.
type LookupReqBuilder struct {
	src, dst sim.RemotePort
	pid      vm.PID
	vAddr    uint64
}

// WithSrc sets the request source.
func (b LookupReqBuilder) WithSrc(src sim.RemotePort) LookupReqBuilder {
	b.src = src
	return b
}

// WithDst sets the request destination.
func (b LookupReqBuilder) WithDst(dst sim.RemotePort) LookupReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the process identifier.
func (b LookupReqBuilder) WithPID(pid vm.PID) LookupReqBuilder {
	b.pid = pid
	return b
}

// WithVAddr sets the virtual address whose page-table segment is queried.
func (b LookupReqBuilder) WithVAddr(vAddr uint64) LookupReqBuilder {
	b.vAddr = vAddr
	return b
}

// Build creates a lookup request.
func (b LookupReqBuilder) Build() *LookupReq {
	return &LookupReq{
		MsgMeta: sim.MsgMeta{
			ID:           sim.GetIDGenerator().Generate(),
			Src:          b.src,
			Dst:          b.dst,
			TrafficClass: reflect.TypeOf(LookupReq{}).String(),
		},
		PID:   b.pid,
		VAddr: b.vAddr,
	}
}

// LookupRsp reports whether a page-table segment was cached.
// A miss is represented by Hit=false and is still returned immediately; the
// cache does not retain a miss until a later fill arrives. // sbin_codex:
// Level is the deepest cache hit across levels 4..1, or -1 when all miss.
type LookupRsp struct {
	sim.MsgMeta

	RspTo   string
	Hit     bool
	PID     vm.PID
	VAddr   uint64
	Level   int
}

// Meta returns the message metadata.
func (r *LookupRsp) Meta() *sim.MsgMeta { return &r.MsgMeta }

// Clone returns a copy with a fresh message ID.
func (r *LookupRsp) Clone() sim.Msg {
	clone := *r
	clone.ID = sim.GetIDGenerator().Generate()
	return &clone
}

// GetRspTo returns the lookup request ID.
func (r *LookupRsp) GetRspTo() string { return r.RspTo }

// FillReq inserts a page-table segment into the cache. GMMU owns the fill
// timing and sends this message after it has completed the corresponding walk.
// sbin_codex: level zero fills are ignored by the cache.
type FillReq struct {
	sim.MsgMeta

	PID   vm.PID
	VAddr uint64
	Level int
}

// Meta returns the message metadata.
func (r *FillReq) Meta() *sim.MsgMeta { return &r.MsgMeta }

// Clone returns a copy with a fresh message ID.
func (r *FillReq) Clone() sim.Msg {
	clone := *r
	clone.ID = sim.GetIDGenerator().Generate()
	return &clone
}

// FillReqBuilder builds cache fill messages.
type FillReqBuilder struct {
	src, dst sim.RemotePort
	pid      vm.PID
	vAddr    uint64
	level    int
}

// WithSrc sets the fill source.
func (b FillReqBuilder) WithSrc(src sim.RemotePort) FillReqBuilder {
	b.src = src
	return b
}

// WithDst sets the fill destination.
func (b FillReqBuilder) WithDst(dst sim.RemotePort) FillReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the process identifier.
func (b FillReqBuilder) WithPID(pid vm.PID) FillReqBuilder {
	b.pid = pid
	return b
}

// WithVAddr sets the virtual address whose page-table segment is filled.
func (b FillReqBuilder) WithVAddr(vAddr uint64) FillReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithLevel sets the page-table level being filled.
func (b FillReqBuilder) WithLevel(level int) FillReqBuilder {
	b.level = level
	return b
}

// Build creates a fill message.
func (b FillReqBuilder) Build() *FillReq {
	return &FillReq{
		MsgMeta: sim.MsgMeta{
			ID:           sim.GetIDGenerator().Generate(),
			Src:          b.src,
			Dst:          b.dst,
			TrafficClass: reflect.TypeOf(FillReq{}).String(),
		},
		PID:   b.pid,
		VAddr: b.vAddr,
		Level: b.level,
	}
}

// Short aliases keep the protocol convenient for GMMU code.
type Req = LookupReq
type Rsp = LookupRsp
type Fill = FillReq
type ReqBuilder = LookupReqBuilder
type FillBuilder = FillReqBuilder
