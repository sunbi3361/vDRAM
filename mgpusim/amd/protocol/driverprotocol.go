package protocol

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
	"github.com/sarchlab/mgpusim/v4/amd/kernels"
)

// FlushReq requests the GPU to flush all the cache to the main memory
type FlushReq struct {
	sim.MsgMeta
}

// Meta returns the meta data associated with the message.
func (m *FlushReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the FlushReq with different ID.
func (m *FlushReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewFlushReq Creates a new flush command, setting the request send time
// with time and the source and destination.
func NewFlushReq(src, dst sim.Port) *FlushReq {
	cmd := new(FlushReq)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// A LaunchKernelReq is a request that asks a GPU to launch a kernel
type LaunchKernelReq struct {
	sim.MsgMeta

	PID vm.PID

	Packet        *kernels.HsaKernelDispatchPacket
	PacketAddress uint64
	CodeObject    *insts.KernelCodeObject
	WGFilter      kernels.WGFilterFunc
}

// Meta returns the meta data associated with the message.
func (m *LaunchKernelReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the LaunchKernelReq with different ID.
func (m *LaunchKernelReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewLaunchKernelReq returns a new LaunchKernelReq
func NewLaunchKernelReq(
	src, dst sim.Port,
) *LaunchKernelReq {
	r := new(LaunchKernelReq)
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = src.AsRemote()
	r.Dst = dst.AsRemote()
	return r
}

// LaunchKernelRsp is the response that is send by the GPU to the driver when
// the kernel completes execution.
type LaunchKernelRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (m *LaunchKernelRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the LaunchKernelRsp with different ID.
func (m *LaunchKernelRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewLaunchKernelRsp returns a new LaunchKernelRsp.
func NewLaunchKernelRsp(
	src, dst sim.RemotePort,
	rspTo string,
) *LaunchKernelRsp {
	r := new(LaunchKernelRsp)
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = src
	r.Dst = dst

	r.RspTo = rspTo

	return r
}

// A MemCopyH2DReq is a request that asks the DMAEngine to copy memory
// from the host to the device
type MemCopyH2DReq struct {
	sim.MsgMeta
	SrcBuffer  []byte
	DstAddress uint64
}

// Meta returns the meta data associated with the message.
func (m *MemCopyH2DReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the MemCopyH2DReq with different ID.
func (m *MemCopyH2DReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewMemCopyH2DReq created a new MemCopyH2DReq
func NewMemCopyH2DReq(
	src, dst sim.Port,
	srcBuffer []byte,
	dstAddress uint64,
) *MemCopyH2DReq {
	req := new(MemCopyH2DReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.MsgMeta.TrafficBytes = len(srcBuffer)
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	req.SrcBuffer = srcBuffer
	req.DstAddress = dstAddress
	return req
}

// A MemCopyD2HReq is a request that asks the DMAEngine to copy memory
// from the host to the device
type MemCopyD2HReq struct {
	sim.MsgMeta
	SrcAddress uint64
	DstBuffer  []byte
}

// Meta returns the meta data associated with the message.
func (m *MemCopyD2HReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the MemCopyD2HReq with different ID.
func (m *MemCopyD2HReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewMemCopyD2HReq created a new MemCopyD2HReq
func NewMemCopyD2HReq(
	src, dst sim.Port,
	srcAddress uint64,
	dstBuffer []byte,
) *MemCopyD2HReq {
	req := new(MemCopyD2HReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.MsgMeta.TrafficBytes = len(dstBuffer)
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	req.SrcAddress = srcAddress
	req.DstBuffer = dstBuffer
	return req
}

// ShootDownCommand requests the GPU to perform a TLB shootdown and invalidate
// the corresponding PTE's
type ShootDownCommand struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec

	VAddr []uint64
	PID   vm.PID
}

// Meta returns the meta data associated with the message.
func (m *ShootDownCommand) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the ShootDownCommand with different ID.
func (m *ShootDownCommand) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewShootdownCommand tells the CP to drain all CU and invalidate PTE's in TLB and Page Tables
func NewShootdownCommand(
	src, dst sim.Port,
	vAddr []uint64,
	pID vm.PID,
) *ShootDownCommand {
	cmd := new(ShootDownCommand)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	cmd.VAddr = vAddr
	cmd.PID = pID
	return cmd
}

// ShootDownCompleteRsp defines a respond
type ShootDownCompleteRsp struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *ShootDownCompleteRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the ShootDownCompleteRsp with different ID.
func (m *ShootDownCompleteRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewShootdownCompleteRsp creates a new respond
func NewShootdownCompleteRsp(
	src, dst sim.Port,
) *ShootDownCompleteRsp {
	cmd := new(ShootDownCompleteRsp)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// RDMADrainCmdFromDriver is driver asking CP to drain local RDMA
type RDMADrainCmdFromDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *RDMADrainCmdFromDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the RDMADrainCmdFromDriver with different ID.
func (m *RDMADrainCmdFromDriver) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewRDMADrainCmdFromDriver creates a new RDMADrainCmdFromDriver
func NewRDMADrainCmdFromDriver(
	src, dst sim.Port,
) *RDMADrainCmdFromDriver {
	cmd := new(RDMADrainCmdFromDriver)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// RDMADrainRspToDriver is  a rsp to driver indicating completion of RDMA drain
type RDMADrainRspToDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *RDMADrainRspToDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the RDMADrainRspToDriver with different ID.
func (m *RDMADrainRspToDriver) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewRDMADrainRspToDriver creates a new RDMADrainRspToDriver
func NewRDMADrainRspToDriver(
	src, dst sim.Port,
) *RDMADrainRspToDriver {
	cmd := new(RDMADrainRspToDriver)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// RDMARestartCmdFromDriver is  a cmd to unpause the RDMA
type RDMARestartCmdFromDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *RDMARestartCmdFromDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the RDMARestartCmdFromDriver with different ID.
func (m *RDMARestartCmdFromDriver) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewRDMARestartCmdFromDriver creates a new RDMARestartCmdFromDriver
func NewRDMARestartCmdFromDriver(
	src, dst sim.Port,
) *RDMARestartCmdFromDriver {
	cmd := new(RDMARestartCmdFromDriver)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// GPURestartReq is  a req to GPU to start the pipeline and unpause all paused components
type GPURestartReq struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *GPURestartReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the GPURestartReq with different ID.
func (m *GPURestartReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewGPURestartReq creates a GPURestart request
func NewGPURestartReq(
	src, dst sim.Port,
) *GPURestartReq {
	cmd := new(GPURestartReq)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// GPURestartRsp is  a rsp indicating the restart is complete
type GPURestartRsp struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *GPURestartRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the GPURestartRsp with different ID.
func (m *GPURestartRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewGPURestartRsp creates a GPURestart respond
func NewGPURestartRsp(
	src, dst sim.Port,
) *GPURestartRsp {
	cmd := new(GPURestartRsp)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// PageMigrationReqToCP is a request to CP to start the page migration process
type PageMigrationReqToCP struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec

	ToReadFromPhysicalAddress uint64
	ToWriteToPhysicalAddress  uint64
	DestinationPMCPort        sim.Port
	PageSize                  uint64
}

// Meta returns the meta data associated with the message.
func (m *PageMigrationReqToCP) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the PageMigrationReqToCP with different ID.
func (m *PageMigrationReqToCP) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewPageMigrationReqToCP creates a PageMigrationReqToCP
func NewPageMigrationReqToCP(
	src, dst sim.Port,
) *PageMigrationReqToCP {
	cmd := new(PageMigrationReqToCP)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// PageMigrationRspToDriver is a rsp to driver indicating completion of Page Migration requests
type PageMigrationRspToDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *PageMigrationRspToDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the PageMigrationRspToDriver with different ID.
func (m *PageMigrationRspToDriver) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewPageMigrationRspToDriver creates a PageMigrationRspToCP
func NewPageMigrationRspToDriver(
	src, dst sim.Port,
) *PageMigrationRspToDriver {
	cmd := new(PageMigrationRspToDriver)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// RDMARestartRspToDriver defines a respond
type RDMARestartRspToDriver struct {
	sim.MsgMeta

	StartTime sim.VTimeInSec
	EndTime   sim.VTimeInSec
}

// Meta returns the meta data associated with the message.
func (m *RDMARestartRspToDriver) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the RDMARestartRspToDriver with different ID.
func (m *RDMARestartRspToDriver) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewRDMARestartRspToDriver creates a RDMARestartRspToDriver
func NewRDMARestartRspToDriver(
	src, dst sim.Port,
) *RDMARestartRspToDriver {
	cmd := new(RDMARestartRspToDriver)
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// sbin_codex: UVM driver/CP/DMA envelopes (todo 2 of plan
// mgpusim-uvm-manager, uvm-manager.md §24). These are the typed message
// envelopes exchanged between the UVM driver, the CP, and the DMA engine.
// They carry the typed UVM contracts from akita/mem/vm; no untyped payloads.

// MigrationDirection describes the direction of a UVM migration.
type MigrationDirection uint8

const (
	// MigrationCPUToGPU migrates a region from CPU backing to GPU HBM.
	MigrationCPUToGPU MigrationDirection = iota
	// MigrationGPUToCPU migrates a region from GPU HBM back to CPU backing.
	MigrationGPUToCPU
)

// PageFaultReq is sent from the GPU CP to the UVM driver when the GMMU detects
// a page fault on a managed virtual address (uvm-manager.md §8.2).
type PageFaultReq struct {
	sim.MsgMeta

	PID               vm.PID
	GPU               int
	VAddr             uint64
	AccessType        vm.AccessKind
	SourceCU          int
	FaultPendingToken vm.FaultPendingToken
}

// Meta returns the meta data associated with the message.
func (m *PageFaultReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the PageFaultReq with different ID.
func (m *PageFaultReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a PageFaultRsp addressed back to the requester.
func (m *PageFaultReq) GenerateRsp() sim.Rsp {
	rsp := PageFaultRspBuilder{}.
		WithSrc(m.Dst).
		WithDst(m.Src).
		WithRspTo(m.ID).
		WithFaultPendingToken(m.FaultPendingToken).
		Build()

	return rsp
}

// NewPageFaultReq creates a new PageFaultReq.
func NewPageFaultReq(src, dst sim.Port) *PageFaultReq {
	req := new(PageFaultReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	return req
}

// PageFaultReqBuilder can build page fault requests.
type PageFaultReqBuilder struct {
	src, dst           sim.RemotePort
	pid                vm.PID
	gpu                int
	vAddr              uint64
	accessType         vm.AccessKind
	sourceCU           int
	faultPendingToken  vm.FaultPendingToken
}

// WithSrc sets the source of the request to build.
func (b PageFaultReqBuilder) WithSrc(src sim.RemotePort) PageFaultReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b PageFaultReqBuilder) WithDst(dst sim.RemotePort) PageFaultReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the process ID of the request to build.
func (b PageFaultReqBuilder) WithPID(pid vm.PID) PageFaultReqBuilder {
	b.pid = pid
	return b
}

// WithGPU sets the GPU ID of the request to build.
func (b PageFaultReqBuilder) WithGPU(gpu int) PageFaultReqBuilder {
	b.gpu = gpu
	return b
}

// WithVAddr sets the virtual address of the request to build.
func (b PageFaultReqBuilder) WithVAddr(vAddr uint64) PageFaultReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithAccessType sets the access kind of the request to build.
func (b PageFaultReqBuilder) WithAccessType(accessType vm.AccessKind) PageFaultReqBuilder {
	b.accessType = accessType
	return b
}

// WithSourceCU sets the source CU of the request to build.
func (b PageFaultReqBuilder) WithSourceCU(sourceCU int) PageFaultReqBuilder {
	b.sourceCU = sourceCU
	return b
}

// WithFaultPendingToken sets the fault-pending token of the request to build.
func (b PageFaultReqBuilder) WithFaultPendingToken(
	token vm.FaultPendingToken,
) PageFaultReqBuilder {
	b.faultPendingToken = token
	return b
}

// Build creates a new PageFaultReq
func (b PageFaultReqBuilder) Build() *PageFaultReq {
	r := &PageFaultReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.GPU = b.gpu
	r.VAddr = b.vAddr
	r.AccessType = b.accessType
	r.SourceCU = b.sourceCU
	r.FaultPendingToken = b.faultPendingToken
	return r
}

// PageFaultRsp is the response to a PageFaultReq.
type PageFaultRsp struct {
	sim.MsgMeta

	RspTo             string
	FaultPendingToken vm.FaultPendingToken
}

// Meta returns the meta data associated with the message.
func (m *PageFaultRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the PageFaultRsp with different ID.
func (m *PageFaultRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (m *PageFaultRsp) GetRspTo() string {
	return m.RspTo
}

// NewPageFaultRsp creates a new PageFaultRsp.
func NewPageFaultRsp(src, dst sim.Port, rspTo string) *PageFaultRsp {
	rsp := new(PageFaultRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src.AsRemote()
	rsp.Dst = dst.AsRemote()
	rsp.RspTo = rspTo
	return rsp
}

// PageFaultRspBuilder can build page fault responses.
type PageFaultRspBuilder struct {
	src, dst           sim.RemotePort
	rspTo              string
	faultPendingToken  vm.FaultPendingToken
}

// WithSrc sets the source of the response to build.
func (b PageFaultRspBuilder) WithSrc(src sim.RemotePort) PageFaultRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build.
func (b PageFaultRspBuilder) WithDst(dst sim.RemotePort) PageFaultRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID that the response replies to.
func (b PageFaultRspBuilder) WithRspTo(rspTo string) PageFaultRspBuilder {
	b.rspTo = rspTo
	return b
}

// WithFaultPendingToken sets the fault-pending token of the response to build.
func (b PageFaultRspBuilder) WithFaultPendingToken(
	token vm.FaultPendingToken,
) PageFaultRspBuilder {
	b.faultPendingToken = token
	return b
}

// Build creates a new PageFaultRsp
func (b PageFaultRspBuilder) Build() *PageFaultRsp {
	r := &PageFaultRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	r.FaultPendingToken = b.faultPendingToken
	return r
}

// AccessCounterNotification reports a remote-access counter observation to the
// UVM driver.
type AccessCounterNotification struct {
	sim.MsgMeta

	PID         vm.PID
	GPU         int
	VAddr       uint64
	AccessKind  vm.AccessKind
	AccessCount uint64
}

// Meta returns the meta data associated with the message.
func (m *AccessCounterNotification) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the AccessCounterNotification with different ID.
func (m *AccessCounterNotification) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// NewAccessCounterNotification creates a new AccessCounterNotification.
func NewAccessCounterNotification(src, dst sim.Port) *AccessCounterNotification {
	cmd := new(AccessCounterNotification)
	cmd.ID = sim.GetIDGenerator().Generate()
	cmd.Src = src.AsRemote()
	cmd.Dst = dst.AsRemote()
	return cmd
}

// AccessCounterNotificationBuilder can build access counter notifications.
type AccessCounterNotificationBuilder struct {
	src, dst     sim.RemotePort
	pid          vm.PID
	gpu          int
	vAddr        uint64
	accessKind   vm.AccessKind
	accessCount  uint64
}

// WithSrc sets the source of the notification to build.
func (b AccessCounterNotificationBuilder) WithSrc(src sim.RemotePort) AccessCounterNotificationBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the notification to build.
func (b AccessCounterNotificationBuilder) WithDst(dst sim.RemotePort) AccessCounterNotificationBuilder {
	b.dst = dst
	return b
}

// WithPID sets the process ID of the notification to build.
func (b AccessCounterNotificationBuilder) WithPID(pid vm.PID) AccessCounterNotificationBuilder {
	b.pid = pid
	return b
}

// WithGPU sets the GPU ID of the notification to build.
func (b AccessCounterNotificationBuilder) WithGPU(gpu int) AccessCounterNotificationBuilder {
	b.gpu = gpu
	return b
}

// WithVAddr sets the virtual address of the notification to build.
func (b AccessCounterNotificationBuilder) WithVAddr(vAddr uint64) AccessCounterNotificationBuilder {
	b.vAddr = vAddr
	return b
}

// WithAccessKind sets the access kind of the notification to build.
func (b AccessCounterNotificationBuilder) WithAccessKind(
	accessKind vm.AccessKind,
) AccessCounterNotificationBuilder {
	b.accessKind = accessKind
	return b
}

// WithAccessCount sets the observed access count of the notification to build.
func (b AccessCounterNotificationBuilder) WithAccessCount(
	count uint64,
) AccessCounterNotificationBuilder {
	b.accessCount = count
	return b
}

// Build creates a new AccessCounterNotification
func (b AccessCounterNotificationBuilder) Build() *AccessCounterNotification {
	r := &AccessCounterNotification{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.GPU = b.gpu
	r.VAddr = b.vAddr
	r.AccessKind = b.accessKind
	r.AccessCount = b.accessCount
	return r
}

// MigrationReq asks the CP/DMA engine to migrate a managed virtual range
// between CPU backing and GPU HBM.
type MigrationReq struct {
	sim.MsgMeta

	PID       vm.PID
	GPU       int
	VAddr     uint64
	Size      uint64
	Direction MigrationDirection
}

// Meta returns the meta data associated with the message.
func (m *MigrationReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the MigrationReq with different ID.
func (m *MigrationReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a MigrationRsp addressed back to the requester.
func (m *MigrationReq) GenerateRsp() sim.Rsp {
	rsp := MigrationRspBuilder{}.
		WithSrc(m.Dst).
		WithDst(m.Src).
		WithRspTo(m.ID).
		Build()

	return rsp
}

// NewMigrationReq creates a new MigrationReq.
func NewMigrationReq(src, dst sim.Port) *MigrationReq {
	req := new(MigrationReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	return req
}

// MigrationReqBuilder can build migration requests.
type MigrationReqBuilder struct {
	src, dst   sim.RemotePort
	pid        vm.PID
	gpu        int
	vAddr      uint64
	size       uint64
	direction  MigrationDirection
}

// WithSrc sets the source of the request to build.
func (b MigrationReqBuilder) WithSrc(src sim.RemotePort) MigrationReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b MigrationReqBuilder) WithDst(dst sim.RemotePort) MigrationReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the process ID of the request to build.
func (b MigrationReqBuilder) WithPID(pid vm.PID) MigrationReqBuilder {
	b.pid = pid
	return b
}

// WithGPU sets the GPU ID of the request to build.
func (b MigrationReqBuilder) WithGPU(gpu int) MigrationReqBuilder {
	b.gpu = gpu
	return b
}

// WithVAddr sets the virtual base address of the request to build.
func (b MigrationReqBuilder) WithVAddr(vAddr uint64) MigrationReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithSize sets the size of the range to migrate.
func (b MigrationReqBuilder) WithSize(size uint64) MigrationReqBuilder {
	b.size = size
	return b
}

// WithDirection sets the migration direction of the request to build.
func (b MigrationReqBuilder) WithDirection(
	direction MigrationDirection,
) MigrationReqBuilder {
	b.direction = direction
	return b
}

// Build creates a new MigrationReq
func (b MigrationReqBuilder) Build() *MigrationReq {
	r := &MigrationReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.GPU = b.gpu
	r.VAddr = b.vAddr
	r.Size = b.size
	r.Direction = b.direction
	return r
}

// MigrationRsp is the response to a MigrationReq.
type MigrationRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (m *MigrationRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the MigrationRsp with different ID.
func (m *MigrationRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (m *MigrationRsp) GetRspTo() string {
	return m.RspTo
}

// NewMigrationRsp creates a new MigrationRsp.
func NewMigrationRsp(src, dst sim.Port, rspTo string) *MigrationRsp {
	rsp := new(MigrationRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src.AsRemote()
	rsp.Dst = dst.AsRemote()
	rsp.RspTo = rspTo
	return rsp
}

// MigrationRspBuilder can build migration responses.
type MigrationRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the response to build.
func (b MigrationRspBuilder) WithSrc(src sim.RemotePort) MigrationRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build.
func (b MigrationRspBuilder) WithDst(dst sim.RemotePort) MigrationRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID that the response replies to.
func (b MigrationRspBuilder) WithRspTo(rspTo string) MigrationRspBuilder {
	b.rspTo = rspTo
	return b
}

// Build creates a new MigrationRsp
func (b MigrationRspBuilder) Build() *MigrationRsp {
	r := &MigrationRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	return r
}

// UVMTLBInvalidateReq asks the CP to route a 64 KB range TLB invalidation to
// the GMMU, which broadcasts it to the shared L2 TLB and every private L1 TLB
// (uvm-manager.md §21.1).
type UVMTLBInvalidateReq struct {
	sim.MsgMeta

	PID     vm.PID
	StartVA uint64
	Size    uint64
}

// Meta returns the meta data associated with the message.
func (m *UVMTLBInvalidateReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the UVMTLBInvalidateReq with different ID.
func (m *UVMTLBInvalidateReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a UVMTLBInvalidateRsp addressed back to the requester.
func (m *UVMTLBInvalidateReq) GenerateRsp() sim.Rsp {
	rsp := UVMTLBInvalidateRspBuilder{}.
		WithSrc(m.Dst).
		WithDst(m.Src).
		WithRspTo(m.ID).
		Build()

	return rsp
}

// NewUVMTLBInvalidateReq creates a new UVMTLBInvalidateReq.
func NewUVMTLBInvalidateReq(src, dst sim.Port) *UVMTLBInvalidateReq {
	req := new(UVMTLBInvalidateReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	return req
}

// UVMTLBInvalidateReqBuilder can build UVM TLB invalidation requests.
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

// WithPID sets the PID/ASID of the request to build.
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
	return r
}

// UVMTLBInvalidateRsp is the aggregated completion response for a
// UVMTLBInvalidateReq.
type UVMTLBInvalidateRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (m *UVMTLBInvalidateRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the UVMTLBInvalidateRsp with different ID.
func (m *UVMTLBInvalidateRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (m *UVMTLBInvalidateRsp) GetRspTo() string {
	return m.RspTo
}

// NewUVMTLBInvalidateRsp creates a new UVMTLBInvalidateRsp.
func NewUVMTLBInvalidateRsp(src, dst sim.Port, rspTo string) *UVMTLBInvalidateRsp {
	rsp := new(UVMTLBInvalidateRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src.AsRemote()
	rsp.Dst = dst.AsRemote()
	rsp.RspTo = rspTo
	return rsp
}

// UVMTLBInvalidateRspBuilder can build UVM TLB invalidation responses.
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
	return r
}

// UVMCacheRangeFlushReq asks the CP to route a range cache writeback and/or
// invalidate for a managed virtual range (uvm-manager.md §21.2).
type UVMCacheRangeFlushReq struct {
	sim.MsgMeta

	Operation     cache.UVMCacheRangeFlushOp
	PID           vm.PID
	VABase        uint64
	ValidPageMask uint64
	PhysicalRuns  []cache.PhysicalRun
}

// Meta returns the meta data associated with the message.
func (m *UVMCacheRangeFlushReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the UVMCacheRangeFlushReq with different ID.
func (m *UVMCacheRangeFlushReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a UVMCacheRangeFlushRsp addressed back to the requester.
func (m *UVMCacheRangeFlushReq) GenerateRsp() sim.Rsp {
	rsp := UVMCacheRangeFlushRspBuilder{}.
		WithSrc(m.Dst).
		WithDst(m.Src).
		WithRspTo(m.ID).
		Build()

	return rsp
}

// NewUVMCacheRangeFlushReq creates a new UVMCacheRangeFlushReq.
func NewUVMCacheRangeFlushReq(src, dst sim.Port) *UVMCacheRangeFlushReq {
	req := new(UVMCacheRangeFlushReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	return req
}

// UVMCacheRangeFlushReqBuilder can build UVM cache range flush requests.
type UVMCacheRangeFlushReqBuilder struct {
	src, dst      sim.RemotePort
	operation     cache.UVMCacheRangeFlushOp
	pid           vm.PID
	vaBase        uint64
	validPageMask uint64
	physicalRuns  []cache.PhysicalRun
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
	operation cache.UVMCacheRangeFlushOp,
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
	runs []cache.PhysicalRun,
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
	return r
}

// UVMCacheRangeFlushRsp is the completion response for a UVMCacheRangeFlushReq.
type UVMCacheRangeFlushRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (m *UVMCacheRangeFlushRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the UVMCacheRangeFlushRsp with different ID.
func (m *UVMCacheRangeFlushRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (m *UVMCacheRangeFlushRsp) GetRspTo() string {
	return m.RspTo
}

// NewUVMCacheRangeFlushRsp creates a new UVMCacheRangeFlushRsp.
func NewUVMCacheRangeFlushRsp(src, dst sim.Port, rspTo string) *UVMCacheRangeFlushRsp {
	rsp := new(UVMCacheRangeFlushRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src.AsRemote()
	rsp.Dst = dst.AsRemote()
	rsp.RspTo = rspTo
	return rsp
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
	return r
}

// UVMFaultReplayReq tells the GMMU to replay the stalled memory requests for a
// serviced managed range (uvm-manager.md §22). The GMMU owns the replay queue;
// the driver returns the replay token so the GMMU can match the replay.
type UVMFaultReplayReq struct {
	sim.MsgMeta

	PID         vm.PID
	GPU         int
	StartVA     uint64
	Size        uint64
	ReplayToken vm.ReplayToken
}

// Meta returns the meta data associated with the message.
func (m *UVMFaultReplayReq) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the UVMFaultReplayReq with different ID.
func (m *UVMFaultReplayReq) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GenerateRsp creates a UVMFaultReplayRsp addressed back to the requester.
func (m *UVMFaultReplayReq) GenerateRsp() sim.Rsp {
	rsp := UVMFaultReplayRspBuilder{}.
		WithSrc(m.Dst).
		WithDst(m.Src).
		WithRspTo(m.ID).
		Build()

	return rsp
}

// NewUVMFaultReplayReq creates a new UVMFaultReplayReq.
func NewUVMFaultReplayReq(src, dst sim.Port) *UVMFaultReplayReq {
	req := new(UVMFaultReplayReq)
	req.ID = sim.GetIDGenerator().Generate()
	req.Src = src.AsRemote()
	req.Dst = dst.AsRemote()
	return req
}

// UVMFaultReplayReqBuilder can build fault replay requests.
type UVMFaultReplayReqBuilder struct {
	src, dst    sim.RemotePort
	pid         vm.PID
	gpu         int
	startVA     uint64
	size        uint64
	replayToken vm.ReplayToken
}

// WithSrc sets the source of the request to build.
func (b UVMFaultReplayReqBuilder) WithSrc(src sim.RemotePort) UVMFaultReplayReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b UVMFaultReplayReqBuilder) WithDst(dst sim.RemotePort) UVMFaultReplayReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the process ID of the request to build.
func (b UVMFaultReplayReqBuilder) WithPID(pid vm.PID) UVMFaultReplayReqBuilder {
	b.pid = pid
	return b
}

// WithGPU sets the GPU ID of the request to build.
func (b UVMFaultReplayReqBuilder) WithGPU(gpu int) UVMFaultReplayReqBuilder {
	b.gpu = gpu
	return b
}

// WithStartVA sets the start of the serviced virtual range.
func (b UVMFaultReplayReqBuilder) WithStartVA(startVA uint64) UVMFaultReplayReqBuilder {
	b.startVA = startVA
	return b
}

// WithSize sets the size of the serviced virtual range.
func (b UVMFaultReplayReqBuilder) WithSize(size uint64) UVMFaultReplayReqBuilder {
	b.size = size
	return b
}

// WithReplayToken sets the replay token of the request to build.
func (b UVMFaultReplayReqBuilder) WithReplayToken(
	token vm.ReplayToken,
) UVMFaultReplayReqBuilder {
	b.replayToken = token
	return b
}

// Build creates a new UVMFaultReplayReq
func (b UVMFaultReplayReqBuilder) Build() *UVMFaultReplayReq {
	r := &UVMFaultReplayReq{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.PID = b.pid
	r.GPU = b.gpu
	r.StartVA = b.startVA
	r.Size = b.size
	r.ReplayToken = b.replayToken
	return r
}

// UVMFaultReplayRsp is the completion response for a UVMFaultReplayReq.
type UVMFaultReplayRsp struct {
	sim.MsgMeta

	RspTo string
}

// Meta returns the meta data associated with the message.
func (m *UVMFaultReplayRsp) Meta() *sim.MsgMeta {
	return &m.MsgMeta
}

// Clone returns a clone of the UVMFaultReplayRsp with different ID.
func (m *UVMFaultReplayRsp) Clone() sim.Msg {
	cloneMsg := *m
	cloneMsg.ID = sim.GetIDGenerator().Generate()

	return &cloneMsg
}

// GetRspTo returns the request ID that the response replies to.
func (m *UVMFaultReplayRsp) GetRspTo() string {
	return m.RspTo
}

// NewUVMFaultReplayRsp creates a new UVMFaultReplayRsp.
func NewUVMFaultReplayRsp(src, dst sim.Port, rspTo string) *UVMFaultReplayRsp {
	rsp := new(UVMFaultReplayRsp)
	rsp.ID = sim.GetIDGenerator().Generate()
	rsp.Src = src.AsRemote()
	rsp.Dst = dst.AsRemote()
	rsp.RspTo = rspTo
	return rsp
}

// UVMFaultReplayRspBuilder can build fault replay responses.
type UVMFaultReplayRspBuilder struct {
	src, dst sim.RemotePort
	rspTo    string
}

// WithSrc sets the source of the response to build.
func (b UVMFaultReplayRspBuilder) WithSrc(src sim.RemotePort) UVMFaultReplayRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the response to build.
func (b UVMFaultReplayRspBuilder) WithDst(dst sim.RemotePort) UVMFaultReplayRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets the request ID that the response replies to.
func (b UVMFaultReplayRspBuilder) WithRspTo(rspTo string) UVMFaultReplayRspBuilder {
	b.rspTo = rspTo
	return b
}

// Build creates a new UVMFaultReplayRsp
func (b UVMFaultReplayRspBuilder) Build() *UVMFaultReplayRsp {
	r := &UVMFaultReplayRsp{}
	r.ID = sim.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.RspTo = b.rspTo
	return r
}
