package runner

import (
	"flag"
	"log"
	"strconv"
	"strings"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
)

var timingFlag = flag.Bool("timing", false, "Run detailed timing simulation.")
var maxInstCount = flag.Uint64("max-inst", 0,
	"Terminate the simulation after the given number of instructions is retired.")
var progressIntervalFlag = flag.Uint64("progress-interval", 0,
	"Print the total number of retired instructions to stdout every N instructions (0 = disabled).") // sbin_codex
var parallelFlag = flag.Bool("parallel", false,
	"Run the simulation in parallel.")
var isaDebug = flag.Bool("debug-isa", false, "Generate the ISA debugging file.")
var archFlag = flag.String("arch", "gcn3", "GPU architecture: gcn3 or cdna3.")

// sbin_codex: added ideal-l1-tlb to the help text.
// var gpuTypeFlag = flag.String("gpu", "r9nano",
//
//	"GPU model for timing simulation: r9nano or mi300a.")
//
// Pre-edit code (commented per project convention):
// var gpuTypeFlag = flag.String("gpu", "r9nano",
//
//	"GPU model for timing simulation: r9nano, mi300a, ideal-l1tlb, or virtual-caching.") // sbin_codex
//
// Pre-edit code (commented per project convention):
// var gpuTypeFlag = flag.String("gpu", "r9nano",
//
//	"GPU model for timing simulation: r9nano, mi300a, ideal-l1tlb, virtual-caching, or utopia.") // sbin_claude_utopia
//
// Pre-edit code (commented per project convention):
// var gpuTypeFlag = flag.String("gpu", "r9nano",
//
//	"GPU model for timing simulation: r9nano, mi300a, ideal-l1tlb, "+
//		"virtual-caching, utopia, or avatar.") // sbin_claude_avatar
var gpuTypeFlag = flag.String("gpu", "r9nano",
	"GPU model for timing simulation: r9nano, mi300a, ideal-l1tlb, "+
		"virtual-caching, utopia, avatar, or hpt.") // sbin_claude_hpt

// sbin_claude_hpt: FS-HPT (PACT'24) hashed-page-table flags. The cost of one
// memory reference is the GMMU's existing per-level page-walking latency, so
// the access count is the only variable between the radix and hashed
// configurations.
var hptAccessesPerWalkFlag = flag.Int("hpt-accesses-per-walk", 1,
	"Memory references one hashed-page-table walk costs. 1 is ideal HPT "+
		"(no hash collision); larger values model collision chains. Only "+
		"meaningful with -gpu=hpt.")

// sbin_claude_avatar: Avatar (speculative translation with rapid
// validation) flags.
var avatarCompressRatioFlag = flag.Float64("avatar-compress-ratio", 0.8,
	"Fraction of frames whose sectors compress well enough to embed page "+
		"information (CAVA rapid validation). Only meaningful with -gpu=avatar.")

// Pre-edit v1 flag (commented per project convention). The whole
// speculative access used to be one flat countdown:
// var avatarValidationLatencyFlag = flag.Int("avatar-validation-latency", 200,
//
//	"Modeled speculative-fetch + CAVA validation latency in cycles.")
//
// sbin_claude_avatar v2: the sector fetch is now a real read through the
// L2/DRAM hierarchy; only the decompress-and-compare stays a fixed latency.
var avatarValidationLatencyFlag = flag.Int("avatar-validation-latency", 2,
	"Extra CAVA validation cycles (decompress + compare) after the "+
		"speculative sector fetch returns from the data hierarchy.")
var avatarModEntriesFlag = flag.Int("avatar-mod-entries", 32,
	"Entries per per-CU MOD (Mapping Offset Detection) table.")
var avatarConfidenceThresholdFlag = flag.Int("avatar-confidence-threshold", 2,
	"MOD confidence needed before speculating.")
var avatarFragFlag = flag.Bool("avatar-frag", true,
	"Model memory fragmentation with 2MB-region randomized physical "+
		"placement. Disabling it makes PPN-VPN globally constant and the MOD "+
		"near-perfect.")

// sbin_claude_utopia: Utopia (hybrid RestSeg/FlexSeg translation) flags.
var utopiaRestSegRatioFlag = flag.Float64("utopia-restseg-ratio", 0.125,
	"Fraction of GPU memory reserved as the RestSeg; the rest stays FlexSeg. "+
		"Only meaningful with -gpu=utopia.")
var utopiaRestSegSizeFlag = flag.Uint64("utopia-restseg-size", 0,
	"RestSeg size in bytes per GPU. Overrides -utopia-restseg-ratio when set.")
var utopiaRestSegAssocFlag = flag.Int("utopia-restseg-assoc", 16,
	"Number of ways per RestSeg set.")

// sbin_claude_utopia: block indexing knob. B consecutive pages share one set
// (and thus one 64B TAR line), trading conflict-absorbing ways for TAR-cache
// spatial locality. 1 = the paper's per-page indexing.
// Pre-edit code (commented per project convention):
// var utopiaRestSegBlockFlag = flag.Int("utopia-restseg-block", 1,
// 	"Consecutive pages that index into one RestSeg set (1 = per-page "+
// 		"indexing per the paper; must not exceed -utopia-restseg-assoc).")
// sbin_claude_utopia: default 16 (user decision 2026-08-26) — one whole set
// per block restores TAR-line spatial locality for sequential walks; pass 1
// to reproduce the paper's per-page indexing.
var utopiaRestSegBlockFlag = flag.Int("utopia-restseg-block", 16,
	"Consecutive pages that index into one RestSeg set (default 16 = one "+
		"whole set per block; 1 = per-page indexing per the paper; must "+
		"not exceed -utopia-restseg-assoc).")
// Pre-edit code (commented per project convention):
// var utopiaTARCacheBytesFlag = flag.Uint64("utopia-tar-cache-bytes", 2048,
//
//	"Capacity of the GMMU-side TAR metadata cache in bytes.")
//
// sbin_claude_utopia: default matches the baseline GMMU page-walk cache
// storage (128 entries x 4 cached levels x 8B = 4KB) for iso-capacity
// TAR-vs-PWC comparison.
var utopiaTARCacheBytesFlag = flag.Uint64("utopia-tar-cache-bytes", 4096,
	"Capacity of the GMMU-side TAR metadata cache in bytes "+
		"(default equals the baseline GMMU page-walk cache: 4KB).")
var utopiaSFCacheBytesFlag = flag.Uint64("utopia-sf-cache-bytes", 2048,
	"Capacity of the GMMU-side Set Filter metadata cache in bytes.")
var utopiaTARSFHitLatencyFlag = flag.Int("utopia-tarsf-hit-latency", 2,
	"TAR/SF cache hit latency in cycles.")
var utopiaTARSFMissLatencyFlag = flag.Int("utopia-tarsf-miss-latency", 100,
	"Modeled memory-fetch latency charged when TAR/SF metadata misses its cache.")

var verifyFlag = flag.Bool("verify", false, "Verify the emulation result.")
var memTracing = flag.Bool("trace-mem", false, "Generate memory trace")
var instCountReportFlag = flag.Bool("report-inst-count", false,
	"Report the number of instructions executed in each compute unit.")
var cacheLatencyReportFlag = flag.Bool("report-cache-latency", false,
	"Report the average cache latency.")
var cacheHitRateReportFlag = flag.Bool("report-cache-hit-rate", false,
	"Report the cache hit rate of each cache.")
var tlbHitRateReportFlag = flag.Bool("report-tlb-hit-rate", false,
	"Report the TLB hit rate of each TLB.")
var rdmaTransactionCountReportFlag = flag.Bool("report-rdma-transaction-count",
	false, "Report the number of transactions going through the RDMA engines.")
var dramTransactionCountReportFlag = flag.Bool("report-dram-transaction-count",
	false, "Report the number of transactions accessing the DRAMs.")
var gpuFlag = flag.String("gpus", "",
	"The GPUs to use, use a format like 1,2,3,4. By default, GPU 1 is used.")
var unifiedGPUFlag = flag.String("unified-gpus", "",
	`Run multi-GPU benchmark in a unified mode.
Use a format like 1,2,3,4. Cannot coexist with -gpus.`)
var useUnifiedMemoryFlag = flag.Bool("use-unified-memory", false,
	"Run benchmark with Unified Memory or not")

// sbin_codex: UVM (Unified Virtual Memory) demand-paging flags.
var uvmFlag = flag.Bool("uvm", false,
	"Enable UVM demand-paged managed memory. Managed allocations must use AllocateManaged.")
var idealUVMFlag = flag.Bool("uvm-ideal", false,
	"Run UVM with zero fault-handling and zero migration timing. Valid only with -uvm=true.")
var uvmFaultLatencyUSFlag = flag.Float64("uvm-fault-latency-us", 20,
	"Fixed host/driver page-fault handling latency in microseconds (UVM).")
var uvmAccessCounterFlag = flag.Bool("uvm-access-counter", true,
	"Let the GPU read host-resident managed memory remotely and migrate a 64KB "+
		"region once its remote-access counter reaches the threshold. When "+
		"false, a cold managed page is INVALID and the first access is a "+
		"demand fault.")

// Pre-edit default (commented per AGENTS.md convention): the threshold used to
// be 64. The specification fixes it at 8. // sbin_codex
var uvmAccessCounterThresholdFlag = flag.Uint64("uvm-access-counter-threshold", 8,
	"Access Counter threshold that triggers a 64KB CPU->GPU migration (UVM).")

// sbin_claude_uvm: with access counters on, the eager path publishes a REMOTE
// PTE for every managed page at allocation time, so a cold page never faults.
// This defers that publication to the first access.
var uvmLazyRemotePTEFlag = flag.Bool("uvm-lazy-remote-pte", false,
	"Publish the CPU-remote mapping of a managed page on its first GPU "+
		"access instead of at allocation time. A 64KB region's first read is "+
		"then a page fault whose only effect is to turn that region's PTEs "+
		"from INVALID to REMOTE; nothing migrates, and migration stays the "+
		"access counter's decision. A region whose first touch is a write is "+
		"migrated on demand instead, since a remote write is never performed. "+
		"Requires -uvm-access-counter=true.")
var uvmDisablePrefetchFlag = flag.Bool("uvm-disable-prefetch", false,
	"Restrict every UVM fault service to its own 64KB leaf (no TBN expansion).")
var uvmDisableEvictionFlag = flag.Bool("uvm-disable-eviction", false,
	"Disable capacity-driven UVM eviction.")
var uvmGPUCapacityFlag = flag.Uint64("uvm-gpu-memory-capacity", 0,
	"GPU bytes available to UVM managed memory. 0 uses the whole GPU memory.")
var uvmGPUCapacityRatioFlag = flag.Float64("uvm-gpu-memory-capacity-ratio", 0,
	"Fraction of GPU memory available to UVM managed memory. "+
		"Ignored when -uvm-gpu-memory-capacity is set.")
var uvmOversubRatioFlag = flag.Float64("uvm-oversubscription-ratio", 0,
	"Total AllocateManaged bytes divided by the UVM GPU capacity. 1.5 gives "+
		"every benchmark 150% oversubscription regardless of its footprint. "+
		"Unlike -uvm-gpu-memory-capacity-ratio this is relative to what the "+
		"benchmark allocated, not to the GPU's physical memory. "+
		"Overrides -uvm-gpu-memory-capacity and its ratio form.")

// sbin_claude: an unlimited value reproduces the pre-fix behaviour, where the
// GPU could park six figures of PCIe transactions in the root complex endpoint
// and starve the migration DMA that would have ended the remote traffic.
var uvmMaxOutstandingRemoteFlag = flag.Int("uvm-max-outstanding-remote", 256,
	"Maximum UVM remote accesses one GPU may have in flight over PCIe. "+
		"0 means unlimited.")
var uvmTBNExpandRatioFlag = flag.Float64("uvm-tbn-expand-ratio", 0.51,
	"Minimum GPU-resident page ratio inside a TBN node to migrate the whole node (UVM).")
var uvmTBNMaxFetchSizeFlag = flag.Uint64("uvm-tbn-max-fetch-size", 1<<21,
	"Maximum TBN neighborhood fetch size in bytes (UVM). Default 2MB.")
var reportAll = flag.Bool("report-all", false, "Report all metrics to .csv file.")
var filenameFlag = flag.String("metric-file-name", "metrics",
	"Modify the name of the output csv file.")
var magicMemoryCopy = flag.Bool("magic-memory-copy", false,
	"Copy data from CPU directly to global memory")
var bufferLevelTraceDirFlag = flag.String("buffer-level-trace-dir", "",
	"The directory to dump the buffer level traces.")
var bufferLevelTracePeriodFlag = flag.Float64("buffer-level-trace-period", 0.0,
	"The period to dump the buffer level trace.")
var simdBusyTimeTracerFlag = flag.Bool("report-busy-time", false, "Report SIMD Unit's busy time")
var reportCPIStackFlag = flag.Bool("report-cpi-stack", false, "Report CPI stack")

// sbin_codex: extended translation report controls.
var gmmuReportFlag = flag.Bool("report-gmmu", false,
	"Report GMMU/MMU translation and page-walk-cache statistics.")
var workingSetReportFlag = flag.Bool("report-working-set", false,
	"Report distinct pages observed by the L1 TLBs.")
var memoryFootprintReportFlag = flag.Bool("report-memory-footprint", false,
	"Report current, peak, and cumulative physical memory footprint.")
var pageMigrationReportFlag = flag.Bool("report-page-migration", false,
	"Report page-migration summaries and payload bytes.")
var l2TLBMPKIReportFlag = flag.Bool("report-l2-tlb-mpki", false,
	"Report L2 TLB misses per thousand retired instructions.")
var customPortForAkitaRTM = flag.Int("akitartm-port", 0,
	`Custom port to host AkitaRTM. A 4-digit or 5-digit port number is required. If 
this number is not given or a invalid number is given number, a random port 
will be used.`)
var disableAkitaRTM = flag.Bool("disable-rtm", false, "Disable the AkitaRTM monitoring portal")

var analyzerNameFlag = flag.String("analyzer-name", "",
	"The name of the analyzer to use.")

var analyzerPeriodFlag = flag.Float64("analyzer-period", 0.0,
	"The period to dump the analyzer results.")

var visTracing = flag.Bool("trace-vis", false,
	"Generate trace for visualization purposes.")
var visTracerDB = flag.String("trace-vis-db", "sqlite",
	"The database to store the visualization trace. Possible values are "+
		"sqlite, mysql, and csv.")
var visTracerDBFileName = flag.String("trace-vis-db-file", "",
	"The file name of the database to store the visualization trace. "+
		"Extension names are not required. "+
		"If not specified, a random file name will be used. "+
		"This flag does not work with Mysql db. When MySQL is used, "+
		"the database name is always randomly generated.")
var visTraceStartTime = flag.Float64("trace-vis-start", -1,
	"The starting time to collect visualization traces. A negative number "+
		"represents starting from the beginning.")
var visTraceEndTime = flag.Float64("trace-vis-end", -1,
	"The end time of collecting visualization traces. A negative number"+
		"means that the trace will be collected to the end of the simulation.")

// sbin_codex
func metricFileNameFlagIsSet() bool {
	isSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "metric-file-name" {
			isSet = true
		}
	})

	return isSet
}

// parseFlag applies the runner flag to runner object
func (r *Runner) parseFlag() *Runner {
	r.parseSimulationFlags()
	r.parseGPUFlag()

	// sbin_claude_utopia: needs r.GPUType and r.GPUIDs, so it runs after both
	// parse steps.
	r.validateUtopiaFlags()
	r.validateAvatarFlags() // sbin_claude_avatar
	r.validateHPTFlags()    // sbin_claude_hpt

	return r
}

func (r *Runner) parseSimulationFlags() {
	if *parallelFlag {
		r.Parallel = true
	}

	if *verifyFlag {
		r.Verify = true
	}

	if *timingFlag {
		r.Timing = true
	}

	if *useUnifiedMemoryFlag {
		r.UseUnifiedMemory = true
	}

	// sbin_claude_uvm: the UVM block lives in its own function so that adding
	// a knob to it does not push parseSimulationFlags over the funlen cap.
	r.parseUVMFlags()

	// sbin_claude_utopia: Utopia flags.
	r.UtopiaRestSegRatio = *utopiaRestSegRatioFlag
	r.UtopiaRestSegBytes = *utopiaRestSegSizeFlag
	r.UtopiaRestSegAssoc = *utopiaRestSegAssocFlag
	r.UtopiaRestSegBlock = *utopiaRestSegBlockFlag // sbin_claude_utopia
	r.UtopiaTARCacheBytes = *utopiaTARCacheBytesFlag
	r.UtopiaSFCacheBytes = *utopiaSFCacheBytesFlag
	r.UtopiaTARSFHitLatency = *utopiaTARSFHitLatencyFlag
	r.UtopiaTARSFMissLatency = *utopiaTARSFMissLatencyFlag

	// sbin_claude_avatar: Avatar flags.
	r.AvatarCompressRatio = *avatarCompressRatioFlag
	r.AvatarValidationLatency = *avatarValidationLatencyFlag
	r.AvatarModEntries = *avatarModEntriesFlag
	r.AvatarConfidenceThreshold = *avatarConfidenceThresholdFlag
	r.AvatarFrag = *avatarFragFlag

	// sbin_claude_hpt: HPT flags.
	r.HPTAccessesPerWalk = *hptAccessesPerWalkFlag

	r.ArchType = parseArchFlag()
	r.GPUType = parseGPUTypeFlag()
}

// validateAvatarFlags rejects invalid Avatar flag combinations (v1 scope).
// sbin_claude_avatar
func (r *Runner) validateAvatarFlags() {
	if r.GPUType != "avatar" {
		return
	}
	if !r.Timing {
		log.Panic("-gpu=avatar requires -timing")
	}
	if r.UVM {
		log.Panic("-gpu=avatar cannot be combined with -uvm yet")
	}
	if len(r.GPUIDs) > 1 {
		log.Panic("-gpu=avatar currently supports a single GPU")
	}
	if r.AvatarCompressRatio < 0 || r.AvatarCompressRatio > 1 {
		log.Panic("-avatar-compress-ratio must be within [0, 1]")
	}
	if r.AvatarValidationLatency <= 0 {
		log.Panic("-avatar-validation-latency must be positive")
	}
	if r.AvatarModEntries <= 0 {
		log.Panic("-avatar-mod-entries must be positive")
	}
	if r.AvatarConfidenceThreshold <= 0 {
		log.Panic("-avatar-confidence-threshold must be positive")
	}
}

// validateHPTFlags rejects invalid HPT flag combinations (v1 scope). The
// single-GPU cap is a scope choice, not a technical constraint: the hashed
// walk is a per-GMMU mode and owns no shared state. // sbin_claude_hpt
func (r *Runner) validateHPTFlags() {
	if r.GPUType != "hpt" {
		return
	}
	if !r.Timing {
		log.Panic("-gpu=hpt requires -timing")
	}
	if len(r.GPUIDs) > 1 {
		log.Panic("-gpu=hpt currently supports a single GPU")
	}
	if r.HPTAccessesPerWalk < 1 {
		log.Panic("-hpt-accesses-per-walk must be at least 1")
	}
}

// validateUtopiaFlags rejects invalid Utopia flag combinations (v1 scope).
// sbin_claude_utopia
func (r *Runner) validateUtopiaFlags() {
	if r.GPUType != "utopia" {
		return
	}
	if !r.Timing {
		log.Panic("-gpu=utopia requires -timing")
	}
	if r.UVM {
		log.Panic("-gpu=utopia cannot be combined with -uvm yet")
	}
	if len(r.GPUIDs) > 1 {
		log.Panic("-gpu=utopia currently supports a single GPU")
	}
	if r.UtopiaRestSegRatio <= 0 && r.UtopiaRestSegBytes == 0 {
		log.Panic("-utopia-restseg-ratio must be positive " +
			"(or set -utopia-restseg-size)")
	}
	if r.UtopiaRestSegRatio >= 1 {
		log.Panic("-utopia-restseg-ratio must be below 1")
	}
	if r.UtopiaRestSegAssoc <= 0 {
		log.Panic("-utopia-restseg-assoc must be positive")
	}
	// sbin_claude_utopia: a block wider than the associativity could never
	// fully reside in one set and would guarantee FlexSeg spills.
	if r.UtopiaRestSegBlock < 1 {
		log.Panic("-utopia-restseg-block must be at least 1")
	}
	if r.UtopiaRestSegBlock > r.UtopiaRestSegAssoc {
		log.Panic("-utopia-restseg-block must not exceed -utopia-restseg-assoc")
	}
}

// parseUVMFlags reads the UVM demand-paging flags into the runner and
// validates the combination. // sbin_codex, split out by sbin_claude_uvm
func (r *Runner) parseUVMFlags() {
	if *uvmFlag {
		r.UVM = true
	}

	if *idealUVMFlag {
		r.IdealUVM = true
	}

	r.UVMFaultLatencyUS = *uvmFaultLatencyUSFlag
	r.UVMAccessCounter = *uvmAccessCounterFlag // sbin_codex
	r.UVMACThreshold = *uvmAccessCounterThresholdFlag
	r.UVMLazyRemotePTE = *uvmLazyRemotePTEFlag // sbin_claude_uvm
	r.UVMExpandRatio = *uvmTBNExpandRatioFlag
	r.UVMmaxFetchSize = *uvmTBNMaxFetchSizeFlag
	r.UVMNoPrefetch = *uvmDisablePrefetchFlag          // sbin_codex
	r.UVMNoEviction = *uvmDisableEvictionFlag          // sbin_codex
	r.UVMGPUCapacityBytes = *uvmGPUCapacityFlag        // sbin_codex
	r.UVMGPUCapacityRatio = *uvmGPUCapacityRatioFlag   // sbin_codex
	r.UVMOversubRatio = *uvmOversubRatioFlag           // sbin_codex
	r.UVMMaxOutstanding = *uvmMaxOutstandingRemoteFlag // sbin_claude

	r.validateUVMFlags()
}

// validateUVMFlags rejects invalid UVM flag combinations per the UVM spec
// mode table. // sbin_codex
func (r *Runner) validateUVMFlags() {
	if r.IdealUVM && !r.UVM {
		log.Panic("-uvm-ideal requires -uvm=true")
	}
	if r.UVM && !r.Timing {
		log.Panic("-uvm requires -timing")
	}
	if r.UVM && r.UseUnifiedMemory {
		log.Panic("-uvm and -use-unified-memory cannot be combined")
	}
	if r.UVM && len(r.GPUIDs) > 1 {
		log.Panic("-uvm currently supports a single GPU")
	}
	if r.UVM && r.ArchType == arch.CDNA3 {
		log.Panic("-uvm currently supports GCN3 only")
	}
	if r.UVM && r.UVMACThreshold == 0 { // sbin_codex
		log.Panic("-uvm-access-counter-threshold must be at least 1")
	}
	if r.UVMLazyRemotePTE && !r.UVM { // sbin_claude_uvm
		log.Panic("-uvm-lazy-remote-pte requires -uvm=true")
	}
	if r.UVMLazyRemotePTE && !r.UVMAccessCounter { // sbin_claude_uvm
		// Without access counters a cold page is already INVALID and its
		// first-access fault migrates the region. There is nothing to defer.
		log.Panic("-uvm-lazy-remote-pte requires -uvm-access-counter=true")
	}
	if r.UVMOversubRatio < 0 { // sbin_codex
		log.Panic("-uvm-oversubscription-ratio must not be negative")
	}
	if !r.UVM && r.UVMOversubRatio > 0 { // sbin_codex
		log.Panic("-uvm-oversubscription-ratio requires -uvm")
	}
	if r.UVMMaxOutstanding < 0 { // sbin_claude
		log.Panic("-uvm-max-outstanding-remote must not be negative")
	}
}

func (r *Runner) parseGPUFlag() {
	if *gpuFlag == "" && *unifiedGPUFlag == "" {
		r.GPUIDs = []int{1}
		return
	}

	if *gpuFlag != "" && *unifiedGPUFlag != "" {
		panic("cannot use -gpus and -unified-gpus together")
	}

	var gpuIDs []int
	if *gpuFlag != "" {
		gpuIDs = r.gpuIDStringToList(*gpuFlag)
	} else if *unifiedGPUFlag != "" {
		gpuIDs = r.gpuIDStringToList(*unifiedGPUFlag)
	}

	r.GPUIDs = gpuIDs
}

func (r *Runner) gpuIDStringToList(gpuIDsString string) []int {
	gpuIDs := make([]int, 0)
	gpuIDTokens := strings.Split(gpuIDsString, ",")

	for _, t := range gpuIDTokens {
		gpuID, err := strconv.Atoi(t)
		if err != nil {
			panic(err)
		}
		gpuIDs = append(gpuIDs, gpuID)
	}

	return gpuIDs
}

func parseArchFlag() arch.Type {
	switch strings.ToLower(*archFlag) {
	case "cdna3", "gfx942":
		return arch.CDNA3
	default:
		return arch.GCN3
	}
}

func parseGPUTypeFlag() string {
	return strings.ToLower(*gpuTypeFlag)
}
