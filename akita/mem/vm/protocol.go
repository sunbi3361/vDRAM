// Package vm provides the models for address translations
package vm

import (
	"reflect"

	"github.com/sarchlab/akita/v4/sim"
)

// TranslationGroupHint carries the LATPC Regularity Detector's triple for
// one translation of a warp memory instruction (MICRO'25, refs/latpc-plan.md
// 1.1). A demand translation has StridePages = 0 and Index = 0; a prefetch
// translation has both non-zero. Translations that share a GroupID belong to
// the same detected stride group: their VPNs are BaseVPN + StridePages*i,
// all inside one 512-page region, so their L4 PTEs share one page-table
// page. A nil hint means "no group information" and preserves pre-LATPC
// behavior everywhere. // sbin_claude_latpc
type TranslationGroupHint struct {
	GroupID     string
	StridePages int64
	Index       int
}

// A TranslationReq asks the receiver component to translate the request.
type TranslationReq struct {
	sim.MsgMeta

	VAddr    uint64
	PID      PID
	DeviceID uint64
	// IsWrite marks a translation for a write access. UVM uses it to migrate
	// remotely-accessible pages on write immediately instead of counting the
	// remote access. // sbin_codex
	IsWrite bool

	// GroupID, GroupStride, and GroupIndex are the LATPC Regularity
	// Detector's triple (see TranslationGroupHint). Zero values mean the
	// request carries no group information (a demand request), which is the
	// pre-LATPC behavior. // sbin_claude_latpc
	GroupID     string
	GroupStride int64
	GroupIndex  int
}

// Meta returns the meta data associated with the message.
func (r *TranslationReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned TranslationReq with different ID
func (r *TranslationReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp generates response to original translation request
func (r *TranslationReq) GenerateRsp(page Page) sim.Rsp {
	rsp := TranslationRspBuilder{}.
		WithSrc(r.Dst).
		WithDst(r.Src).
		WithRspTo(r.ID).
		WithPage(page).
		Build()

	return rsp
}

// TranslationReqBuilder can build translation requests
type TranslationReqBuilder struct {
	src, dst sim.RemotePort
	vAddr    uint64
	pid      PID
	deviceID uint64
	isWrite  bool // sbin_codex
	// sbin_claude_latpc: LATPC group triple, zero when absent.
	groupID     string
	groupStride int64
	groupIndex  int
}

// WithSrc sets the source of the request to build.
func (b TranslationReqBuilder) WithSrc(
	src sim.RemotePort,
) TranslationReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b TranslationReqBuilder) WithDst(
	dst sim.RemotePort,
) TranslationReqBuilder {
	b.dst = dst
	return b
}

// WithVAddr sets the virtual address of the request to build.
func (b TranslationReqBuilder) WithVAddr(vAddr uint64) TranslationReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithPID sets the virtual address of the request to build.
func (b TranslationReqBuilder) WithPID(pid PID) TranslationReqBuilder {
	b.pid = pid
	return b
}

// WithDeviceID sets the GPU ID of the request to build.
func (b TranslationReqBuilder) WithDeviceID(
	deviceID uint64,
) TranslationReqBuilder {
	b.deviceID = deviceID
	return b
}

// WithIsWrite marks the translation as serving a write access. // sbin_codex
func (b TranslationReqBuilder) WithIsWrite(isWrite bool) TranslationReqBuilder {
	b.isWrite = isWrite
	return b
}

// WithGroupHint attaches the LATPC group triple from a hint. A nil hint
// leaves the request without group information. // sbin_claude_latpc
func (b TranslationReqBuilder) WithGroupHint(
	hint *TranslationGroupHint,
) TranslationReqBuilder {
	if hint == nil {
		return b
	}

	b.groupID = hint.GroupID
	b.groupStride = hint.StridePages
	b.groupIndex = hint.Index

	return b
}

// WithGroup sets the LATPC group triple field-by-field, for propagating the
// triple of an existing TranslationReq downstream. // sbin_claude_latpc
func (b TranslationReqBuilder) WithGroup(
	groupID string,
	stridePages int64,
	index int,
) TranslationReqBuilder {
	b.groupID = groupID
	b.groupStride = stridePages
	b.groupIndex = index

	return b
}

// Build creates a new TranslationReq
func (b TranslationReqBuilder) Build() *TranslationReq {
	r := &TranslationReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.VAddr = b.vAddr
	r.PID = b.pid
	r.DeviceID = b.deviceID
	r.IsWrite = b.isWrite
	// sbin_claude_latpc: LATPC group triple, zero when absent.
	r.GroupID = b.groupID
	r.GroupStride = b.groupStride
	r.GroupIndex = b.groupIndex
	r.TrafficClass = reflect.TypeOf(TranslationReq{}).String()

	return r
}

// A TranslationCancelReq asks a translation provider to abandon an earlier
// TranslationReq that its requester no longer needs (Avatar Early TLB Fill,
// refs/avatar.md 5.9). It is delivered out of band - on a dedicated Cancel
// port - so it can overtake the queued request it names. Cancellation is
// best effort: a request that already completed, or a page walk that already
// started, is simply left alone and its late response is dropped by the
// requester. // sbin_claude_avatar
type TranslationCancelReq struct {
	sim.MsgMeta

	// CancelID is the ID of the TranslationReq to abandon.
	CancelID string
	VAddr    uint64
	PID      PID

	// Page is the translation Avatar's Early TLB Fill already validated.
	// When it is valid the receiver must not merely drop the walk: it fills
	// the shared TLB with this page and hands it to every other requester
	// coalesced on the same VPN before releasing the MSHR (refs/avatar.md
	// 5.9 steps 5-7). An invalid Page means a plain cancel.
	// sbin_claude_avatar
	Page Page
}

// Meta returns the meta data associated with the message.
// sbin_claude_avatar
func (r *TranslationCancelReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned TranslationCancelReq with different ID
// sbin_claude_avatar
func (r *TranslationCancelReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// TranslationCancelReqBuilder can build translation cancel requests.
// sbin_claude_avatar
type TranslationCancelReqBuilder struct {
	src, dst sim.RemotePort
	cancelID string
	vAddr    uint64
	pid      PID
	page     Page // sbin_claude_avatar
}

// WithSrc sets the source of the request to build. // sbin_claude_avatar
func (b TranslationCancelReqBuilder) WithSrc(
	src sim.RemotePort,
) TranslationCancelReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build. // sbin_claude_avatar
func (b TranslationCancelReqBuilder) WithDst(
	dst sim.RemotePort,
) TranslationCancelReqBuilder {
	b.dst = dst
	return b
}

// WithCancelID names the TranslationReq to abandon. // sbin_claude_avatar
func (b TranslationCancelReqBuilder) WithCancelID(
	cancelID string,
) TranslationCancelReqBuilder {
	b.cancelID = cancelID
	return b
}

// WithVAddr sets the page-aligned virtual address of the canceled request.
// sbin_claude_avatar
func (b TranslationCancelReqBuilder) WithVAddr(
	vAddr uint64,
) TranslationCancelReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithPID sets the PID of the canceled request. // sbin_claude_avatar
func (b TranslationCancelReqBuilder) WithPID(
	pid PID,
) TranslationCancelReqBuilder {
	b.pid = pid
	return b
}

// WithPage attaches the Early-TLB-Fill translation, turning a plain cancel
// into a fill-and-cancel (refs/avatar.md 5.9 steps 5-7). // sbin_claude_avatar
func (b TranslationCancelReqBuilder) WithPage(
	page Page,
) TranslationCancelReqBuilder {
	b.page = page
	return b
}

// Build creates a new TranslationCancelReq. // sbin_claude_avatar
func (b TranslationCancelReqBuilder) Build() *TranslationCancelReq {
	r := &TranslationCancelReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.CancelID = b.cancelID
	r.VAddr = b.vAddr
	r.PID = b.pid
	r.Page = b.page // sbin_claude_avatar
	r.TrafficClass = reflect.TypeOf(TranslationReq{}).String()

	return r
}

// A TranslationRsp is the respond for a TranslationReq. It carries the physical
// address.
type TranslationRsp struct {
	sim.MsgMeta

	RespondTo string // The ID of the request it replies
	Page      Page
}

// Meta returns the meta data associated with the message.
func (r *TranslationRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns cloned TranslationRsp with different ID
func (r *TranslationRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the respond is responding to.
func (r *TranslationRsp) GetRspTo() string {
	return r.RespondTo
}

// TranslationRspBuilder can build translation requests
type TranslationRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
	page     Page
}

// WithSrc sets the source of the respond to build.
func (b TranslationRspBuilder) WithSrc(
	src sim.RemotePort,
) TranslationRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the respond to build.
func (b TranslationRspBuilder) WithDst(
	dst sim.RemotePort,
) TranslationRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID of the respond to build.
func (b TranslationRspBuilder) WithRspTo(rspTo string) TranslationRspBuilder {
	b.rspTo = rspTo
	return b
}

// WithPage sets the page of the respond to build.
func (b TranslationRspBuilder) WithPage(page Page) TranslationRspBuilder {
	b.page = page
	return b
}

// Build creates a new TranslationRsp
func (b TranslationRspBuilder) Build() *TranslationRsp {
	r := &TranslationRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RespondTo = b.rspTo
	r.Page = b.page
	r.TrafficClass = reflect.TypeOf(TranslationReq{}).String()

	return r
}

// PageMigrationInfo records the information required for the driver to perform
// a page migration.
type PageMigrationInfo struct {
	GPUReqToVAddrMap map[uint64][]uint64
}

// PageMigrationReqToDriver is a req to driver from MMU to start page migration
// process
type PageMigrationReqToDriver struct {
	sim.MsgMeta

	StartTime         sim.VTimeInSec
	EndTime           sim.VTimeInSec
	MigrationInfo     *PageMigrationInfo
	CurrAccessingGPUs []uint64
	PID               PID
	CurrPageHostGPU   uint64
	PageSize          uint64
	RespondToTop      bool
}

// Meta returns the meta data associated with the message.
func (m *PageMigrationReqToDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns cloned PageMigrationReqToDriver with different ID
func (m *PageMigrationReqToDriver) Clone() sim.Msg {
	return m
}

func (m *PageMigrationReqToDriver) GenerateRsp() sim.Rsp {
	rsp := NewPageMigrationRspFromDriver(m.Dst, m.Src, m)

	return rsp
}

// NewPageMigrationReqToDriver creates a PageMigrationReqToDriver.
func NewPageMigrationReqToDriver(
	src, dst sim.RemotePort,
) *PageMigrationReqToDriver {
	cmd := new(PageMigrationReqToDriver)
	// Pre-edit code (commented per project convention):
	// cmd.Src = src
	//
	// sbin_claude: every other message constructor stamps a unique
	// ID here. Without one the message travels with ID "", and the
	// switching-network endpoint keys its flit-reassembly table by
	// message ID (noc/networking/switching/endpoint), so every such
	// message in flight collides on the same key and the link wedges.
	cmd.ID = sim.GetIDGenerator().Generate() // sbin_claude
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(PageMigrationReqToDriver{}).String()

	return cmd
}

// PageMigrationRspFromDriver is a rsp from driver to MMU marking completion of
// migration
type PageMigrationRspFromDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
	VAddr     []uint64
	RspToTop  bool

	OriginalReq sim.Msg
}

// Meta returns the meta data associated with the message.
func (m *PageMigrationRspFromDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns cloned PageMigrationRspFromDriver with different ID
func (m *PageMigrationRspFromDriver) Clone() sim.Msg {
	return m
}

func (m *PageMigrationRspFromDriver) GetRspTo() string {
	return m.OriginalReq.Meta().ID
}

// NewPageMigrationRspFromDriver creates a new PageMigrationRspFromDriver.
func NewPageMigrationRspFromDriver(
	src, dst sim.RemotePort,
	originalReq sim.Msg,
) *PageMigrationRspFromDriver {
	cmd := new(PageMigrationRspFromDriver)
	// Pre-edit code (commented per project convention):
	// cmd.Src = src
	//
	// sbin_claude: every other message constructor stamps a unique
	// ID here. Without one the message travels with ID "", and the
	// switching-network endpoint keys its flit-reassembly table by
	// message ID (noc/networking/switching/endpoint), so every such
	// message in flight collides on the same key and the link wedges.
	cmd.ID = sim.GetIDGenerator().Generate() // sbin_claude
	cmd.Src = src
	cmd.Dst = dst
	cmd.OriginalReq = originalReq
	cmd.TrafficClass = reflect.TypeOf(PageMigrationReqToDriver{}).String()

	return cmd
}

// PageFaultReq is a request from a GPU GMMU to the driver UVM manager asking
// to service a demand page fault for a managed page. The GMMU keeps the
// originating translation pending until it receives the PageFaultRsp.
type PageFaultReq struct {
	sim.MsgMeta

	PID      PID
	VAddr    uint64
	DeviceID uint64
	// IsWrite marks a fault raised by a write access. A write to a
	// remotely-mapped managed page must migrate immediately instead of being
	// committed to host memory. // sbin_codex
	IsWrite bool
	// WaitRequestID is the ID of the original TranslationReq being serviced.
	WaitRequestID string
}

// Meta returns the meta data associated with the message.
func (r *PageFaultReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the PageFaultReq with a different ID.
func (r *PageFaultReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()
	return &cloneMsg
}

// NewPageFaultReq creates a new PageFaultReq.
func NewPageFaultReq(src, dst sim.RemotePort) *PageFaultReq {
	cmd := new(PageFaultReq)
	// Pre-edit code (commented per project convention):
	// cmd.Src = src
	//
	// sbin_claude: every other message constructor stamps a unique
	// ID here. Without one the message travels with ID "", and the
	// switching-network endpoint keys its flit-reassembly table by
	// message ID (noc/networking/switching/endpoint), so every such
	// message in flight collides on the same key and the link wedges.
	cmd.ID = sim.GetIDGenerator().Generate() // sbin_claude
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(PageFaultReq{}).String()
	return cmd
}

// PageFaultRsp notifies a GMMU that a demand page fault has been serviced.
// The GMMU re-reads the page table and completes the pending translation.
type PageFaultRsp struct {
	sim.MsgMeta

	RespondTo string
	PID       PID
	VAddr     uint64
}

// Meta returns the meta data associated with the message.
func (r *PageFaultRsp) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the PageFaultRsp with a different ID.
func (r *PageFaultRsp) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()
	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (r *PageFaultRsp) GetRspTo() string {
	return r.RespondTo
}

// NewPageFaultRsp creates a new PageFaultRsp.
func NewPageFaultRsp(src, dst sim.RemotePort, respondTo string) *PageFaultRsp {
	cmd := new(PageFaultRsp)
	// Pre-edit code (commented per project convention):
	// cmd.Src = src
	//
	// sbin_claude: every other message constructor stamps a unique
	// ID here. Without one the message travels with ID "", and the
	// switching-network endpoint keys its flit-reassembly table by
	// message ID (noc/networking/switching/endpoint), so every such
	// message in flight collides on the same key and the link wedges.
	cmd.ID = sim.GetIDGenerator().Generate() // sbin_claude
	cmd.Src = src
	cmd.Dst = dst
	cmd.RespondTo = respondTo
	cmd.TrafficClass = reflect.TypeOf(PageFaultRsp{}).String()
	return cmd
}

// AccessCounterNotifyReq is sent by a GPU GMMU to the driver when a 64KB
// remote-access counter reaches its threshold, requesting a CPU->GPU
// migration of that region. // sbin_codex
type AccessCounterNotifyReq struct {
	sim.MsgMeta

	PID        PID
	RegionBase uint64
	DeviceID   uint64
}

// Meta returns the meta data associated with the message.
func (r *AccessCounterNotifyReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the AccessCounterNotifyReq with a different ID.
func (r *AccessCounterNotifyReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()
	return &cloneMsg
}

// NewAccessCounterNotifyReq creates a new AccessCounterNotifyReq.
func NewAccessCounterNotifyReq(src, dst sim.RemotePort) *AccessCounterNotifyReq {
	cmd := new(AccessCounterNotifyReq)
	// Pre-edit code (commented per project convention):
	// cmd.Src = src
	//
	// sbin_claude: every other message constructor stamps a unique
	// ID here. Without one the message travels with ID "", and the
	// switching-network endpoint keys its flit-reassembly table by
	// message ID (noc/networking/switching/endpoint), so every such
	// message in flight collides on the same key and the link wedges.
	cmd.ID = sim.GetIDGenerator().Generate() // sbin_claude
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(AccessCounterNotifyReq{}).String()
	return cmd
}

// AccessCounterResetReq is sent by the driver to the PCIe accesscounter after
// a 64KB region migrates to the GPU, re-arming that region. // sbin_codex
type AccessCounterResetReq struct {
	sim.MsgMeta

	PID        PID
	RegionBase uint64
	DeviceID   uint64
	// ResetAll clears every counter of the GPU instead of one region. The
	// driver issues it at a kernel boundary so remote accesses never
	// accumulate across kernels. // sbin_codex
	ResetAll bool
}

// Meta returns the meta data associated with the message.
func (r *AccessCounterResetReq) Meta() *sim.MsgMeta {
	return &r.MsgMeta
}

// Clone returns a clone of the AccessCounterResetReq with a different ID.
func (r *AccessCounterResetReq) Clone() sim.Msg {
	cloneMsg := *r
	cloneMsg.ID = sim.GetIDGenerator().Generate()
	return &cloneMsg
}

// NewAccessCounterResetReq creates a new AccessCounterResetReq.
func NewAccessCounterResetReq(src, dst sim.RemotePort) *AccessCounterResetReq {
	cmd := new(AccessCounterResetReq)
	// Pre-edit code (commented per project convention):
	// cmd.Src = src
	//
	// sbin_claude: every other message constructor stamps a unique
	// ID here. Without one the message travels with ID "", and the
	// switching-network endpoint keys its flit-reassembly table by
	// message ID (noc/networking/switching/endpoint), so every such
	// message in flight collides on the same key and the link wedges.
	cmd.ID = sim.GetIDGenerator().Generate() // sbin_claude
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(AccessCounterResetReq{}).String()
	return cmd
}
