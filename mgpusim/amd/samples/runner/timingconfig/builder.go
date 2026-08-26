// Package timingconfig contains the configuration for the timing simulation.
package timingconfig

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/mmu"
	"github.com/sarchlab/akita/v4/noc/networking/pcie"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection" // sbin_codex: zero-latency UVM fault channel in ideal mode.
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	avatargpu "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/avatar" // sbin_claude_avatar: Avatar GPU builder
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/gpubuilder"
	ideall1tlb "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/ideal-l1tlb" // sbin_codex: ideal-L1-TLB GPU builder
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/r9nano"
	utopiagpu "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/utopia"               // sbin_claude_utopia: Utopia GPU builder
	virtualcaching "github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig/virtual-caching" // sbin_codex: virtual-caching GPU builder
	avatarmeta "github.com/sarchlab/mgpusim/v4/amd/timing/avatar/meta"                              // sbin_claude_avatar: shared Avatar state
	"github.com/sarchlab/mgpusim/v4/amd/timing/utopia/restseg"                                      // sbin_claude_utopia: shared RestSeg state
)

// Builder builds a hardware platform for timing simulation.
type Builder struct {
	simulation *simulation.Simulation

	numGPUs            int
	numCUPerSA         int
	numSAPerGPU        int
	cpuMemSize         uint64
	gpuMemSize         uint64
	log2PageSize       uint64
	useMagicMemoryCopy bool
	gpuType            string
	switchLatency      int // PCIe/interconnect switch latency in cycles
	d2hCycles          int
	h2dCycles          int

	platform          *sim.Domain
	globalStorage     *mem.Storage
	rdmaAddressMapper *mem.BankedAddressPortMapper
	cpuRemoteTop      sim.Port       // sbin_codex
	cpuPageTable      vm.PageTable   // sbin_codex: authoritative CPU-side page table.
	gpuPageTables     []vm.PageTable // sbin_codex: one isolated page table per GPU GMMU.

	// sbin_codex: UVM demand-paging configuration.
	uvmEnabled              bool
	uvmIdeal                bool
	uvmFaultUS              float64
	uvmAccessCounter        bool
	uvmACThresh             uint64
	uvmTBNExpand            float64
	uvmTBNMaxSize           uint64
	uvmNoPrefetch           bool
	uvmNoEviction           bool
	uvmGPUCapacity          uint64
	uvmCapacityRatio        float64
	uvmOversubRatio         float64
	uvmMaxOutstandingRemote int // sbin_claude

	// sbin_claude_utopia: Utopia hybrid RestSeg/FlexSeg configuration.
	utopiaCfg      UtopiaPlatformConfig
	utopiaRegistry *restseg.Registry

	// sbin_claude_avatar: Avatar speculative-translation configuration.
	avatarCfg      AvatarPlatformConfig
	avatarRegistry *avatarmeta.Registry

	// sbin_claude_hpt: hashed-page-table (FS-HPT) walk configuration.
	hptCfg HPTPlatformConfig

	// sbin_claude_softwalker: SoftWalker software page-walk configuration.
	// swCfgSet records whether WithSoftWalker ran; without it (direct
	// selector use in tests) the paper defaults apply.
	swCfg    SoftWalkerPlatformConfig
	swCfgSet bool
	// sbin_claude_softwalker: baseline sweep knobs (paper Figure 5
	// replication); 0 keeps the r9nano defaults.
	gmmuMaxInflight int
	l2TLBNumMSHR    int
	// sbin_claude_latpc: LATPC (MICRO'25) translation-path configuration.
	latpcCfg LATPCPlatformConfig
}

// uvmCapacity resolves the GPU capacity UVM managed memory may occupy. An
// explicit byte count wins over a ratio of the GPU DRAM; with neither, the
// whole GPU memory is available and no oversubscription occurs. // sbin_codex
func (b *Builder) uvmCapacity() uint64 {
	if b.uvmGPUCapacity > 0 {
		return b.uvmGPUCapacity
	}

	if b.uvmCapacityRatio > 0 {
		return uint64(float64(b.gpuMemSize) * b.uvmCapacityRatio)
	}

	return b.gpuMemSize
}

// MakeBuilder creates a new Builder with default parameters.
func MakeBuilder() Builder {
	return Builder{
		numGPUs:            1,
		numCUPerSA:         4,
		numSAPerGPU:        16,
		cpuMemSize:         4 * mem.GB,
		gpuMemSize:         4 * mem.GB,
		log2PageSize:       12,
		useMagicMemoryCopy: false,
		gpuType:            "r9nano",
		switchLatency:      140, // default PCIe Gen4
		d2hCycles:          300,
		h2dCycles:          500,
	}
}

// WithSimulation sets the simulation to use.
func (b Builder) WithSimulation(sim *simulation.Simulation) Builder {
	b.simulation = sim
	return b
}

// WithNumGPUs sets the number of GPUs to simulate.
func (b Builder) WithNumGPUs(numGPUs int) Builder {
	b.numGPUs = numGPUs
	return b
}

// WithMagicMemoryCopy sets whether to use the magic memory copy middleware.
func (b Builder) WithMagicMemoryCopy() Builder {
	b.useMagicMemoryCopy = true
	return b
}

// Pre-edit comment (commented per project convention):
// WithGPUType sets the GPU type for timing simulation (r9nano, mi300a,
// ideal-l1tlb, virtual-caching, utopia, avatar, or hpt). // sbin_claude_hpt
//
// WithGPUType sets the GPU type for timing simulation (r9nano, mi300a,
// ideal-l1tlb, virtual-caching, utopia, avatar, hpt, or softwalker).
// sbin_claude_softwalker
func (b Builder) WithGPUType(gpuType string) Builder {
	b.gpuType = gpuType
	return b
}

// WithUVM enables UVM demand-paged managed memory on the timing platform.
func (b Builder) WithUVM(config UVMPlatformConfig) Builder {
	b.uvmEnabled = config.Enabled
	b.uvmIdeal = config.Ideal
	b.uvmFaultUS = config.FaultLatencyUS
	b.uvmAccessCounter = config.AccessCounterEnabled
	b.uvmACThresh = config.AccessCounterThreshold
	b.uvmTBNExpand = config.TBNExpandRatio
	b.uvmTBNMaxSize = config.TBNMaxFetchSize
	b.uvmNoPrefetch = config.PrefetchDisabled
	b.uvmNoEviction = config.EvictionDisabled
	b.uvmGPUCapacity = config.GPUCapacityBytes
	b.uvmCapacityRatio = config.GPUCapacityRatio
	b.uvmOversubRatio = config.OversubscriptionRatio
	b.uvmMaxOutstandingRemote = config.MaxOutstandingRemote // sbin_claude

	return b
}

// WithUtopia sets the Utopia hybrid RestSeg/FlexSeg configuration. It only
// takes effect when the GPU type is "utopia". // sbin_claude_utopia
func (b Builder) WithUtopia(config UtopiaPlatformConfig) Builder {
	b.utopiaCfg = config
	return b
}

// WithAvatar sets the Avatar speculative-translation configuration. It only
// takes effect when the GPU type is "avatar". // sbin_claude_avatar
func (b Builder) WithAvatar(config AvatarPlatformConfig) Builder {
	b.avatarCfg = config
	return b
}

// WithHPT sets the hashed-page-table configuration. It only takes effect when
// the GPU type is "hpt". // sbin_claude_hpt
func (b Builder) WithHPT(config HPTPlatformConfig) Builder {
	b.hptCfg = config
	return b
}

// WithLATPC sets the LATPC configuration. It only takes effect when the GPU
// type is "latpc", except L1TLBMSHREntries, which also applies to the plain
// r9nano and hpt configurations (the sizing knob must hit the baseline and
// LATPC symmetrically). // sbin_claude_latpc
func (b Builder) WithLATPC(config LATPCPlatformConfig) Builder {
	b.latpcCfg = config
	return b
}

// LATPCPlatformConfig carries the LATPC knobs from the runner into the
// platform builder (refs/latpc-plan.md 2.6). // sbin_claude_latpc
type LATPCPlatformConfig struct {
	// L4RowHitLatency is the cycles one batched member's L4 PTE load costs
	// in the GMMU (a DRAM row-buffer hit). 0 keeps the GMMU default (20).
	L4RowHitLatency int
	// L1TLBMSHREntries overrides the per-CU L1V TLB MSHR entry count for
	// r9nano-based configurations. 0 keeps the default (64).
	L1TLBMSHREntries int
}

// latpcEnabled reports whether the platform builds the LATPC GPU type.
// sbin_claude_latpc
func (b *Builder) latpcEnabled() bool {
	return b.gpuType == "latpc"
}

// HPTPlatformConfig carries the FS-HPT knobs from the runner into the
// platform builder (hpt-plan.md 1.3). // sbin_claude_hpt
type HPTPlatformConfig struct {
	// AccessesPerWalk is how many memory references one hashed-page-table
	// walk costs. Ideal HPT (no hash collision) is 1; the cost of a single
	// access is the GMMU's existing per-level page-walking latency, so this
	// is the only variable between the radix and hashed configurations.
	AccessesPerWalk int
}

// hptEnabled reports whether the platform builds the HPT GPU type.
// sbin_claude_hpt
func (b *Builder) hptEnabled() bool {
	return b.gpuType == "hpt"
}

// hptAccessesPerWalk resolves the per-walk access count (default 1, the
// ideal-HPT assumption and the runner flag default). // sbin_claude_hpt
func (b *Builder) hptAccessesPerWalk() int {
	if b.hptCfg.AccessesPerWalk > 0 {
		return b.hptCfg.AccessesPerWalk
	}

	return 1
}

// validateHPTConfig rejects unsupported HPT combinations (v1 scope). The
// single-GPU cap is a scope choice, not a technical constraint: the walk mode
// is per-GMMU and owns no shared state. // sbin_claude_hpt
func (b *Builder) validateHPTConfig() {
	if !b.hptEnabled() {
		return
	}
	if b.numGPUs > 1 {
		panic("-gpu=hpt currently supports a single GPU")
	}
}

// WithSoftWalker sets the SoftWalker configuration. It only takes effect
// when the GPU type is "softwalker". All fields are used verbatim - the
// runner's flag defaults own the paper values. sbin_claude_softwalker
func (b Builder) WithSoftWalker(config SoftWalkerPlatformConfig) Builder {
	b.swCfg = config
	b.swCfgSet = true
	return b
}

// WithGMMUMaxInflight overrides the baseline GMMU's in-flight walk cap (the
// hardware PTW count analog, paper Figure 5). 0 keeps the default.
// sbin_claude_softwalker
func (b Builder) WithGMMUMaxInflight(n int) Builder {
	b.gmmuMaxInflight = n
	return b
}

// WithL2TLBNumMSHR overrides the shared L2 TLB's dedicated MSHR entry
// count. 0 keeps the default. sbin_claude_softwalker
func (b Builder) WithL2TLBNumMSHR(n int) Builder {
	b.l2TLBNumMSHR = n
	return b
}

// SoftWalkerPlatformConfig carries the SoftWalker knobs from the runner into
// the platform builder (softwalker-plan.md 1.3-1.4). // sbin_claude_softwalker
type SoftWalkerPlatformConfig struct {
	// SlotsPerCU is the SoftPWB depth per CU (32 in the paper).
	SlotsPerCU int
	// CommCycles is the one-way L2TLB<->CU communication latency, charged
	// twice per walk. The paper models it as the L2 TLB access latency.
	CommCycles int
	// SetupCycles is the PW Warp's per-walk setup cost.
	SetupCycles int
	// PerLevelCycles is the non-memory instruction cost per traversed
	// page-table level.
	PerLevelCycles int
	// InTLBMSHRMax caps the L2 TLB ways repurposed as In-TLB MSHR slots.
	// 0 disables In-TLB MSHR (the "SW w/o In-TLB MSHR" ablation).
	InTLBMSHRMax int
}

// softWalkerEnabled reports whether the platform builds the SoftWalker GPU
// type. sbin_claude_softwalker
func (b *Builder) softWalkerEnabled() bool {
	return b.gpuType == "softwalker"
}

// softWalkerSettings resolves the r9nano settings. Without WithSoftWalker
// (direct selector use in tests) the paper defaults apply: 32 slots per CU,
// comm = the L2 TLB latency (10), setup 20, 8 cycles per level, and every L2
// TLB way available as an In-TLB MSHR slot. sbin_claude_softwalker
func (b *Builder) softWalkerSettings() r9nano.SoftWalkerSettings {
	if !b.swCfgSet {
		return r9nano.SoftWalkerSettings{
			Enabled:        true,
			SlotsPerCU:     32,
			CommCycles:     10,
			SetupCycles:    20,
			PerLevelCycles: 8,
			InTLBMSHRMax:   512,
		}
	}

	return r9nano.SoftWalkerSettings{
		Enabled:        true,
		SlotsPerCU:     b.swCfg.SlotsPerCU,
		CommCycles:     b.swCfg.CommCycles,
		SetupCycles:    b.swCfg.SetupCycles,
		PerLevelCycles: b.swCfg.PerLevelCycles,
		InTLBMSHRMax:   b.swCfg.InTLBMSHRMax,
	}
}

// validateSoftWalkerConfig rejects unsupported SoftWalker combinations. The
// single-GPU cap matches hpt/utopia/avatar; UVM is rejected by scope choice
// (softwalker-plan.md: non-UVM only - the paper's FFB fault path is not
// modeled). sbin_claude_softwalker
func (b *Builder) validateSoftWalkerConfig() {
	if !b.softWalkerEnabled() {
		return
	}
	if b.numGPUs > 1 {
		panic("-gpu=softwalker currently supports a single GPU")
	}
	if b.uvmEnabled {
		panic("-gpu=softwalker does not support -uvm (non-UVM scope)")
	}
}

// validateLATPCConfig rejects unsupported LATPC combinations. UVM is a hard
// constraint, not scope: the GMMU's batch drain answers members straight
// from the page table and would bypass the managed-page fault gating
// (refs/latpc-plan.md 1.4). // sbin_claude_latpc
func (b *Builder) validateLATPCConfig() {
	if !b.latpcEnabled() {
		return
	}
	if b.numGPUs > 1 {
		panic("-gpu=latpc currently supports a single GPU")
	}
	if b.uvmEnabled {
		panic("-gpu=latpc does not support -uvm (non-UVM only)")
	}
}

// AvatarPlatformConfig carries the Avatar knobs from the runner into the
// platform builder (refs/avatar.md, avatar-plan.md). // sbin_claude_avatar
type AvatarPlatformConfig struct {
	// CompressRatio is the fraction of frames whose sectors compress well
	// enough to embed page information (deterministic per-frame draw).
	CompressRatio float64
	// ValidationLatency is the modeled speculative-fetch + CAVA check
	// latency in cycles.
	ValidationLatency int
	// ModEntries is the per-CU MOD table size; ConfidenceThreshold is the
	// confidence needed to speculate.
	ModEntries          int
	ConfidenceThreshold int
	// FragDisabled turns the 2MB-region randomized physical placement off
	// (the stock sequential pool then makes PPN-VPN globally constant).
	FragDisabled bool
}

// avatarSeed makes the compressibility draw and the region selection
// deterministic and identical across configurations. // sbin_claude_avatar
const avatarSeed = 0x5b1a7a7a

// avatarCompressRatio resolves the compression ratio (default 0.8, the
// runner flag default; refs/avatar.md 5.5). // sbin_claude_avatar
func (b *Builder) avatarCompressRatio() float64 {
	if b.avatarCfg.CompressRatio > 0 {
		return b.avatarCfg.CompressRatio
	}

	return 0.8
}

// avatarEnabled reports whether the platform builds the Avatar GPU type.
// sbin_claude_avatar
func (b *Builder) avatarEnabled() bool {
	return b.gpuType == "avatar"
}

// validateAvatarConfig rejects unsupported Avatar combinations (v1 scope).
// sbin_claude_avatar
func (b *Builder) validateAvatarConfig() {
	if !b.avatarEnabled() {
		return
	}
	if b.numGPUs > 1 {
		panic("-gpu=avatar currently supports a single GPU")
	}
	if b.uvmEnabled {
		panic("-gpu=avatar cannot be combined with -uvm yet")
	}
}

// makeAvatarRegistry constructs the shared authoritative Avatar state.
// sbin_claude_avatar
func (b *Builder) makeAvatarRegistry() *avatarmeta.Registry {
	return avatarmeta.NewRegistry(
		b.log2PageSize, b.avatarCompressRatio(), avatarSeed)
}

// UtopiaPlatformConfig carries the Utopia knobs from the runner into the
// platform builder. // sbin_claude_utopia
type UtopiaPlatformConfig struct {
	// RestSegRatio is the fraction of each GPU's memory reserved as RestSeg
	// (the FlexSeg keeps the remainder). Ignored when RestSegBytes is set.
	RestSegRatio float64
	// RestSegBytes is the explicit RestSeg size per GPU; overrides the ratio.
	RestSegBytes uint64
	// Associativity is the number of ways per RestSeg set.
	Associativity int
	// BlockPages is the number of consecutive pages that index into one
	// RestSeg set (1 = the paper's per-page indexing; B > 1 trades way-level
	// conflict absorption for TAR-line spatial locality). sbin_claude_utopia
	BlockPages int
	// TARCacheBytes and SFCacheBytes size the GMMU-side metadata caches.
	TARCacheBytes uint64
	SFCacheBytes  uint64
	// HitLatency is the TAR/SF cache hit latency; MissLatency is the modeled
	// memory fetch charged on a metadata cache miss.
	HitLatency  int
	MissLatency int
}

// utopiaRestSegBytes resolves the per-GPU RestSeg size. An explicit byte
// count wins over the ratio; with neither, 1/8 of the GPU memory is used.
// sbin_claude_utopia
func (b *Builder) utopiaRestSegBytes() uint64 {
	if b.utopiaCfg.RestSegBytes > 0 {
		return b.utopiaCfg.RestSegBytes
	}

	ratio := b.utopiaCfg.RestSegRatio
	if ratio <= 0 {
		ratio = 0.125
	}

	return uint64(float64(b.gpuMemSize) * ratio)
}

// utopiaAssociativity resolves the RestSeg way count (default 16).
// sbin_claude_utopia
func (b *Builder) utopiaAssociativity() int {
	if b.utopiaCfg.Associativity > 0 {
		return b.utopiaCfg.Associativity
	}

	return 16
}

// utopiaBlockPages resolves the RestSeg block size (default 1 = per-page).
// sbin_claude_utopia
func (b *Builder) utopiaBlockPages() int {
	if b.utopiaCfg.BlockPages > 0 {
		return b.utopiaCfg.BlockPages
	}

	return 1
}

// utopiaEnabled reports whether the platform builds the Utopia GPU type.
// sbin_claude_utopia
func (b *Builder) utopiaEnabled() bool {
	return b.gpuType == "utopia"
}

// validateUtopiaConfig rejects unsupported Utopia combinations (v1 scope).
// sbin_claude_utopia
func (b *Builder) validateUtopiaConfig() {
	if !b.utopiaEnabled() {
		return
	}
	if b.numGPUs > 1 {
		panic("-gpu=utopia currently supports a single GPU")
	}
	if b.uvmEnabled {
		panic("-gpu=utopia cannot be combined with -uvm yet")
	}
	if b.utopiaRestSegBytes() >= b.gpuMemSize {
		panic("utopia: RestSeg must be smaller than the GPU memory")
	}
}

// UVMPlatformConfig carries the UVM knobs from the runner into the platform
// builder. // sbin_codex
type UVMPlatformConfig struct {
	Enabled                bool
	Ideal                  bool
	FaultLatencyUS         float64
	AccessCounterEnabled   bool
	AccessCounterThreshold uint64
	TBNExpandRatio         float64
	TBNMaxFetchSize        uint64
	PrefetchDisabled       bool
	EvictionDisabled       bool
	GPUCapacityBytes       uint64
	GPUCapacityRatio       float64
	OversubscriptionRatio  float64
	// MaxOutstandingRemote caps the GPU's in-flight PCIe remote accesses.
	// Zero means unlimited. // sbin_claude
	MaxOutstandingRemote int
}

// Build builds the hardware platform.
func (b Builder) Build() *sim.Domain {
	b.adjustConfigForGPUType()
	b.cpuGPUMemSizeMustEqual()
	// sbin_claude_utopia: the registry is shared between the driver (policy,
	// authoritative TAR/SF) and every GPU's UTU (timed RestSeg walk).
	b.validateUtopiaConfig()
	if b.utopiaEnabled() {
		b.utopiaRegistry = restseg.NewRegistry()
	}
	// sbin_claude_avatar: the registry is shared between the driver
	// (metadata bookkeeping, region placement) and the GPU's ASU (CAVA).
	b.validateAvatarConfig()
	if b.avatarEnabled() {
		b.avatarRegistry = b.makeAvatarRegistry()
	}
	// sbin_claude_hpt: the hashed walk needs no shared state, only validation.
	b.validateHPTConfig()
	// sbin_claude_softwalker: the software walk needs no shared state either.
	b.validateSoftWalkerConfig()
	// sbin_claude_latpc: LATPC needs no shared state either, only validation.
	b.validateLATPCConfig()

	b.platform = &sim.Domain{}

	b.globalStorage = mem.NewStorage(
		uint64(b.numGPUs)*b.gpuMemSize + b.cpuMemSize)

	b.createPageTables()                   // sbin_codex: construct distinct CPU/GPU page-table instances.
	mmuComp := b.createMMU(b.cpuPageTable) // sbin_codex: CPU MMU uses only the CPU table.
	gpuDriver := b.buildGPUDriver(
		b.cpuPageTable, b.gpuPageTables) // sbin_codex: driver owns CPU/GPU table synchronization.
	b.buildCPURemoteMemory(gpuDriver) // sbin_codex
	b.createRDMAAddressMapper()       // sbin_codex: CPU endpoint exists before mapper construction.

	gpuBuilder := b.createGPUBuilder(mmuComp)
	pcieConnector, rootComplexID := b.createConnection(gpuDriver, mmuComp)

	mmuComp.MigrationServiceProvider = gpuDriver.GetPortByName("MMU").AsRemote()

	b.createRDMAAddrTable()
	pmcAddressTable := b.createPMCPageTable()

	b.createGPUs(
		rootComplexID, pcieConnector,
		gpuBuilder, gpuDriver,
		pmcAddressTable)

	pcieConnector.EstablishRoute()

	return b.platform
}

func (b *Builder) cpuGPUMemSizeMustEqual() {
	if b.cpuMemSize != b.gpuMemSize {
		panic("currently only support cpuMemSize == gpuMemSize")
	}
}

func (b *Builder) adjustConfigForGPUType() {
	switch b.gpuType {
	default:
		// Keep defaults for r9nano
	}
}

// createPageTables allocates one CPU page table and one page table per GPU.
// No CPU MMU or GPU GMMU shares the same table instance. // sbin_codex
func (b *Builder) createPageTables() {
	b.cpuPageTable = vm.NewPageTable(b.log2PageSize)
	b.gpuPageTables = make([]vm.PageTable, b.numGPUs)
	for i := range b.gpuPageTables {
		b.gpuPageTables[i] = vm.NewPageTable(b.log2PageSize)
	}
}

func (b *Builder) createMMU(pageTable vm.PageTable) *mmu.Comp { // sbin_codex: accept the CPU table explicitly.
	mmuBuilder := mmu.MakeBuilder().
		WithEngine(b.simulation.GetEngine()).
		WithFreq(1 * sim.GHz).
		WithPageWalkingLatency(100).
		WithLog2PageSize(b.log2PageSize).
		WithPageTable(pageTable)

	mmuComponent := mmuBuilder.Build("MMU")

	b.simulation.RegisterComponent(mmuComponent)

	return mmuComponent
}

func (b *Builder) buildGPUDriver(
	cpuPageTable vm.PageTable,
	gpuPageTables []vm.PageTable, // sbin_codex: supply all page tables to the driver.
) *driver.Driver {
	gpuDriverBuilder := driver.MakeBuilder()

	if b.useMagicMemoryCopy {
		gpuDriverBuilder = gpuDriverBuilder.WithMagicMemoryCopyMiddleware()
	}

	gpuDriverBuilder = gpuDriverBuilder.
		WithEngine(b.simulation.GetEngine()).
		WithPageTable(cpuPageTable).
		WithGPUPageTables(gpuPageTables). // sbin_codex: driver registers GPU tables with its allocator.
		WithLog2PageSize(b.log2PageSize).
		WithGlobalStorage(b.globalStorage).
		WithD2HCycles(b.d2hCycles).
		WithH2DCycles(b.h2dCycles)

	// sbin_codex: UVM demand-paging configuration.
	if b.uvmEnabled {
		gpuDriverBuilder = gpuDriverBuilder.WithUVM(driver.UVMConfig{
			Enabled:                true,
			Ideal:                  b.uvmIdeal,
			FaultLatencyUS:         b.uvmFaultUS,
			AccessCounterEnabled:   b.uvmAccessCounter, // sbin_codex
			AccessCounterThreshold: b.uvmACThresh,
			TBNExpandRatio:         b.uvmTBNExpand,
			TBNMaxFetchSize:        b.uvmTBNMaxSize,
			PrefetchDisabled:       b.uvmNoPrefetch,   // sbin_codex
			EvictionDisabled:       b.uvmNoEviction,   // sbin_codex
			GPUCapacityBytes:       b.uvmCapacity(),   // sbin_codex
			OversubscriptionRatio:  b.uvmOversubRatio, // sbin_codex
		})
	}

	// sbin_claude_avatar: the driver installs/invalidates embedded page
	// metadata and places frames at 2MB-region granularity.
	if b.avatarEnabled() {
		gpuDriverBuilder = gpuDriverBuilder.WithAvatar(driver.AvatarConfig{
			Enabled:     true,
			Registry:    b.avatarRegistry,
			FragEnabled: !b.avatarCfg.FragDisabled,
		})
	}

	// sbin_claude_utopia: the driver reserves the RestSeg per GPU and places
	// pages RestSeg-first through the shared registry.
	if b.utopiaEnabled() {
		// Pre-edit code (commented per project convention):
		// gpuDriverBuilder = gpuDriverBuilder.WithUtopia(driver.UtopiaConfig{
		// 	Enabled:       true,
		// 	Registry:      b.utopiaRegistry,
		// 	RestSegBytes:  b.utopiaRestSegBytes(),
		// 	Associativity: b.utopiaAssociativity(),
		// })
		gpuDriverBuilder = gpuDriverBuilder.WithUtopia(driver.UtopiaConfig{
			Enabled:       true,
			Registry:      b.utopiaRegistry,
			RestSegBytes:  b.utopiaRestSegBytes(),
			Associativity: b.utopiaAssociativity(),
			BlockPages:    b.utopiaBlockPages(), // sbin_claude_utopia
		})
	}

	gpuDriver := gpuDriverBuilder.Build("Driver")

	b.simulation.RegisterComponent(gpuDriver)

	return gpuDriver
}

func (b *Builder) createGPUBuilder(
	mmuComponent *mmu.Comp,
) gpubuilder.GPUBuilder {
	// b.createRDMAAddressMapper() // sbin_codex: pre-edit construction occurred before CPU endpoint wiring.

	switch b.gpuType {
	case "ideal-l1tlb": // sbin_codex: ideal-L1TLB GPU config (todo 7).
		return ideall1tlb.MakeBuilder().
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	case "virtual-caching": // sbin_codex: virtual-caching GPU config.
		return virtualcaching.MakeBuilder().
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	case "avatar": // sbin_claude_avatar: speculative-translation GPU config.
		// Build() creates the registry before the driver; direct selector
		// use (tests) creates it here so the topology never sees nil.
		if b.avatarRegistry == nil {
			b.avatarRegistry = b.makeAvatarRegistry()
		}
		return avatargpu.MakeBuilder().
			WithAvatarSettings(r9nano.AvatarSettings{
				Registry:            b.avatarRegistry,
				ValidationLatency:   b.avatarCfg.ValidationLatency,
				ModNumEntries:       b.avatarCfg.ModEntries,
				ConfidenceThreshold: b.avatarCfg.ConfidenceThreshold,
			}).
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	case "hpt": // sbin_claude_hpt: hashed-page-table (FS-HPT) GPU config.
		// HPT swaps neither a component factory nor a topology, only a GMMU
		// walk mode, so the r9nano builder is returned directly instead of
		// through a wrapper package.
		return r9nano.MakeBuilder().
			WithHPTSettings(r9nano.HPTSettings{
				Enabled:         true,
				AccessesPerWalk: b.hptAccessesPerWalk(),
			}).
			WithL1TLBMSHREntries(b.latpcCfg.L1TLBMSHREntries). // sbin_claude_latpc: sizing knob.
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	case "softwalker": // sbin_claude_softwalker: software page-walk GPU config.
		return b.makeSoftWalkerGPUBuilder(mmuComponent)
	case "latpc": // sbin_claude_latpc: LATPC (MICRO'25) GPU config.
		return b.makeLATPCGPUBuilder(mmuComponent)
	case "utopia": // sbin_claude_utopia: hybrid RestSeg/FlexSeg GPU config.
		// Build() creates the registry before the driver; direct selector use
		// (tests) creates it here so the topology constructor never sees nil.
		if b.utopiaRegistry == nil {
			b.utopiaRegistry = restseg.NewRegistry()
		}
		return utopiagpu.MakeBuilder().
			WithUtopiaSettings(r9nano.UtopiaSettings{
				Registry:      b.utopiaRegistry,
				TARCacheBytes: b.utopiaCfg.TARCacheBytes,
				SFCacheBytes:  b.utopiaCfg.SFCacheBytes,
				HitLatency:    b.utopiaCfg.HitLatency,
				MissLatency:   b.utopiaCfg.MissLatency,
			}).
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	default:
		return r9nano.MakeBuilder().
			WithGMMUMaxInflight(b.gmmuMaxInflight).
			WithL2TLBNumMSHR(b.l2TLBNumMSHR).
			WithL1TLBMSHREntries(b.latpcCfg.L1TLBMSHREntries).
			WithSimulation(b.simulation).
			WithMMU(mmuComponent).
			WithLog2PageSize(b.log2PageSize).
			WithGlobalStorage(b.globalStorage)
	}
}

// makeSoftWalkerGPUBuilder returns the r9nano builder in SoftWalker mode.
// Like HPT, SoftWalker changes only a GMMU walk mode plus an L2 TLB
// capability, so no wrapper package exists. sbin_claude_softwalker
func (b *Builder) makeSoftWalkerGPUBuilder(
	mmuComponent *mmu.Comp,
) gpubuilder.GPUBuilder {
	return r9nano.MakeBuilder().
		WithSoftWalkerSettings(b.softWalkerSettings()).
		WithL2TLBNumMSHR(b.l2TLBNumMSHR).
		WithSimulation(b.simulation).
		WithMMU(mmuComponent).
		WithLog2PageSize(b.log2PageSize).
		WithGlobalStorage(b.globalStorage)
}

// makeLATPCGPUBuilder returns the -gpu=latpc GPU builder. Like HPT, LATPC
// swaps neither a component factory nor a topology - the Regularity
// Detector, the LATC MSHR, and LATP batching are all builder values on the
// baseline path - so the r9nano builder is returned directly instead of
// through a wrapper package. // sbin_claude_latpc
func (b *Builder) makeLATPCGPUBuilder(
	mmuComponent *mmu.Comp,
) gpubuilder.GPUBuilder {
	return r9nano.MakeBuilder().
		WithLATPCSettings(r9nano.LATPCSettings{
			Enabled:         true,
			L4RowHitLatency: b.latpcCfg.L4RowHitLatency,
		}).
		WithL1TLBMSHREntries(b.latpcCfg.L1TLBMSHREntries).
		WithSimulation(b.simulation).
		WithMMU(mmuComponent).
		WithLog2PageSize(b.log2PageSize).
		WithGlobalStorage(b.globalStorage)
}

func (b *Builder) createGPUs(
	rootComplexID int,
	pcieConnector *pcie.Connector,
	gpuBuilder gpubuilder.GPUBuilder,
	gpuDriver *driver.Driver,
	pmcAddressTable *mem.BankedAddressPortMapper,
) {
	lastSwitchID := rootComplexID
	for i := 1; i < b.numGPUs+1; i++ {
		if i%2 == 1 {
			lastSwitchID = pcieConnector.AddSwitch(rootComplexID)
		}

		b.createGPU(i, gpuBuilder, gpuDriver, pmcAddressTable,
			pcieConnector, lastSwitchID)
	}
}

func (b *Builder) createPMCPageTable() *mem.BankedAddressPortMapper {
	pmcAddressTable := new(mem.BankedAddressPortMapper)
	pmcAddressTable.BankSize = b.gpuMemSize
	pmcAddressTable.LowModules = append(pmcAddressTable.LowModules, "")
	return pmcAddressTable
}

func (b *Builder) createRDMAAddrTable() *mem.BankedAddressPortMapper {
	rdmaAddressTable := new(mem.BankedAddressPortMapper)
	rdmaAddressTable.BankSize = b.gpuMemSize
	rdmaAddressTable.LowModules = append(rdmaAddressTable.LowModules, "")
	return rdmaAddressTable
}

func (b *Builder) createConnection(
	gpuDriver *driver.Driver,
	mmuComponent *mmu.Comp,
) (*pcie.Connector, int) {
	// connection := sim.NewDirectConnection(engine)
	// connection := noc.NewFixedBandwidthConnection(32, engine, 1*sim.GHz)
	// connection.SrcBufferCapacity = 40960000
	pcieConnector := pcie.NewConnector().
		WithEngine(b.simulation.GetEngine()).
		WithVersion(4, 16).
		WithSwitchLatency(b.switchLatency)

	pcieConnector.CreateNetwork("PCIe")
	rootComplexPorts := []sim.Port{
		gpuDriver.GetPortByName("GPU"),
		gpuDriver.GetPortByName("MMU"),
		mmuComponent.GetPortByName("Migration"),
		mmuComponent.GetPortByName("Top"),
	}
	// Pre-edit code (commented per project convention). The driver UVM port
	// used to share the root complex endpoint with the data ports:
	//
	// // sbin_codex: in ideal mode the whole UVM control channel is carried by
	// // a zero-latency direct connection instead of PCIe (spec 1.2), so the
	// // driver UVM port must stay off the root complex.
	// if b.uvmEnabled && !b.uvmIdeal {
	// 	rootComplexPorts = append(
	// 		rootComplexPorts, gpuDriver.GetPortByName("UVM"))
	// }
	rootComplexPorts = append(rootComplexPorts, b.cpuRemoteTop) // sbin_codex
	rootComplexID := pcieConnector.AddRootComplex(rootComplexPorts)

	// sbin_claude: the UVM control plane gets its own endpoint, which is to
	// say its own virtual channel, on the same root complex switch.
	//
	// An endpoint serialises every port plugged into it through one flit
	// queue, and that queue is unbounded. With access counters on, the CPU
	// side answers the GPU's remote reads through b.cpuRemoteTop, and a
	// kernel that touches managed memory from thousands of work-items queues
	// six figures of data flits there. A migration's TLB invalidate or region
	// drain then sat behind all of them. Because the driver services one 64KB
	// region at a time and cannot start the next until that control message is
	// acknowledged, no region ever migrated, so the pages stayed remote and
	// produced yet more data flits: the run made no forward progress at all.
	//
	// Ideal mode keeps its zero-latency direct connection instead (spec 1.2).
	if b.uvmEnabled && !b.uvmIdeal {
		pcieConnector.PlugInDevice(rootComplexID,
			[]sim.Port{gpuDriver.GetPortByName("UVM")})
	}

	return pcieConnector, rootComplexID
}

func (b *Builder) createRDMAAddressMapper() {
	b.rdmaAddressMapper = new(mem.BankedAddressPortMapper)
	b.rdmaAddressMapper.BankSize = b.gpuMemSize
	// Pre-edit code (commented per AGENTS.md convention):
	// b.rdmaAddressMapper.LowModules = append(b.rdmaAddressMapper.LowModules,
	// 	sim.RemotePort("CPU"))
	b.rdmaAddressMapper.LowModules = append( // sbin_codex
		b.rdmaAddressMapper.LowModules, b.cpuRemoteTop.AsRemote())
}

func (b *Builder) createGPU(
	index int,
	gpuBuilder gpubuilder.GPUBuilder,
	gpuDriver *driver.Driver,
	pmcAddressTable *mem.BankedAddressPortMapper,
	pcieConnector *pcie.Connector,
	pcieSwitchID int,
) *sim.Domain {
	name := fmt.Sprintf("GPU[%d]", index)
	memAddrOffset := uint64(index) * b.gpuMemSize
	builder := gpuBuilder.
		WithGPUID(uint64(index)).
		WithMemAddrOffset(memAddrOffset).
		WithRDMAAddressMapper(b.rdmaAddressMapper).
		WithDriverPort(gpuDriver.GetPortByName("GPU")). // sbin_codex
		WithPageTable(b.gpuPageTables[index-1])         // sbin_codex: bind GPU N to its driver-managed table.
	if b.uvmEnabled { // sbin_codex: the GPU UVM endpoint answers to the driver.
		builder = builder.
			WithUVMServiceProvider(gpuDriver.GetPortByName("UVM").AsRemote()).
			WithAccessCounterThreshold(b.uvmACThresh).
			WithMaxOutstandingRemote(b.uvmMaxOutstandingRemote) // sbin_claude
	}
	gpu := builder.Build(name)

	gpuDriver.RegisterGPU(
		gpu.GetPortByName("CommandProcessor"),
		driver.DeviceProperties{
			CUCount:  b.numCUPerSA * b.numSAPerGPU,
			DRAMSize: b.gpuMemSize,
		},
	)

	if b.uvmEnabled { // sbin_codex: the CP is the GPU-side UVM endpoint.
		gpuDriver.RegisterUVMGPU(gpu.GetPortByName("UVM").AsRemote())
	}
	// gpu.CommandProcessor.Driver = gpuDriver.GetPortByName("GPU")

	b.configRDMAEngine(gpu)
	// b.configPMC(gpu, gpuDriver, pmcAddressTable)

	ports := gpu.Ports()

	// Pre-edit code (commented per project convention). The UVM port was
	// separated from the data ports only in ideal mode, and by name match:
	//
	// if b.uvmEnabled && b.uvmIdeal {
	// 	... build conn ...
	// 	filtered := make([]sim.Port, 0, len(ports))
	// 	for _, p := range ports {
	// 		if !strings.Contains(p.Name(), "UVM") {
	// 			filtered = append(filtered, p)
	// 		}
	// 	}
	// 	ports = filtered
	// }
	//
	// sbin_claude: the GPU end of the control plane needs the same separation
	// the root complex end does, for the same reason - the access counter's
	// remote egress leaves through the RDMA engine, whose port shares this
	// endpoint's single flit queue with the CP's UVM port. Page faults and
	// counter notifications were queued behind the remote traffic they were
	// meant to stop. So the UVM port now always leaves `ports`, and is either
	// direct-connected (ideal) or given its own endpoint (PCIe).
	var uvmEndpointPort sim.Port

	if b.uvmEnabled {
		uvmPort := gpu.GetPortByName("UVM")
		filtered := make([]sim.Port, 0, len(ports))

		for _, p := range ports {
			if p != uvmPort {
				filtered = append(filtered, p)
			}
		}

		ports = filtered

		if b.uvmIdeal { // sbin_codex: zero-latency UVM fault channel.
			// Route UVM fault messages between the GMMU and the driver over a
			// direct connection instead of PCIe, so ideal-uvm charges no
			// interconnect latency on the control plane.
			conn := directconnection.MakeBuilder().
				WithEngine(b.simulation.GetEngine()).
				WithFreq(1 * sim.GHz).
				Build(name + ".UVMConn")
			b.simulation.RegisterComponent(conn)
			conn.PlugIn(gpuDriver.GetPortByName("UVM"))
			conn.PlugIn(uvmPort)
		} else {
			uvmEndpointPort = uvmPort
		}
	}

	pcieConnector.PlugInDevice(pcieSwitchID, ports)

	if uvmEndpointPort != nil { // sbin_claude: own endpoint, own flit queue.
		pcieConnector.PlugInDevice(
			pcieSwitchID, []sim.Port{uvmEndpointPort})
	}

	// b.gpus = append(b.gpus, gpu)

	return gpu
}

func (b *Builder) configRDMAEngine(
	gpu *sim.Domain,
) {
	b.rdmaAddressMapper.LowModules = append(
		b.rdmaAddressMapper.LowModules,
		gpu.GetPortByName("RDMAData").AsRemote())
}

// func (b *Builder) configPMC(
// 	gpu *GPU,
// 	gpuDriver *driver.Driver,
// 	addrTable *mem.BankedAddressPortMapper,
// ) {
// 	gpu.PMC.RemotePMCAddressTable = addrTable
// 	addrTable.LowModules = append(
// 		addrTable.LowModules,
// 		gpu.PMC.GetPortByName("Remote").AsRemote())
// 	gpuDriver.RemotePMCPorts = append(
// 		gpuDriver.RemotePMCPorts, gpu.PMC.GetPortByName("Remote"))
// }
