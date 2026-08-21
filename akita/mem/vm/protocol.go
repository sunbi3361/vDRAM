// Package vm provides the models for address translations
package vm

import (
	"reflect"

	"github.com/sarchlab/akita/v4/sim"
)

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
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(AccessCounterNotifyReq{}).String()
	return cmd
}

// AccessCounterResetReq is sent by the driver to a GPU GMMU after a
// 64KB region migrates to the GPU, resetting its remote-access counter for
// the new residency epoch. // sbin_codex
type AccessCounterResetReq struct {
	sim.MsgMeta

	PID        PID
	RegionBase uint64
	DeviceID   uint64
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
	cmd.Src = src
	cmd.Dst = dst
	cmd.TrafficClass = reflect.TypeOf(AccessCounterResetReq{}).String()
	return cmd
}
