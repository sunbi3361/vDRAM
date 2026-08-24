package runner

import (
	"flag"
	"strconv"
	"strings"
	"time"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
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
var gpuTypeFlag = flag.String("gpu", "r9nano",
	"GPU model for timing simulation: r9nano, mi300a, ideal-l1tlb, or virtual-caching.") // sbin_codex

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

// sbin_codex: UVM managed-memory configuration flags (uvm-manager.md §26).
// Defaults mirror driver.DefaultUVMConfig; keep them in sync.
var uvmFlag = flag.Bool("uvm", false, "Enable UVM managed memory.")
var uvmIdealFlag = flag.Bool("uvm-ideal", false,
	"Canonical zero-latency UVM experiment mode (requires -uvm).")
var uvmAccessCounterFlag = flag.Bool("uvm-access-counter", false,
	"Enable UVM access counters.")
var uvmFaultHandlingLatencyFlag = flag.Duration(
	"uvm-fault-handling-latency", 20*time.Microsecond,
	"UVM fault handling latency (must be non-negative).")
var uvmAccessCounterThresholdFlag = flag.Int(
	"uvm-access-counter-threshold", 8,
	"UVM access counter threshold before migration (must be > 0).")
var uvmVABlockSizeFlag = flag.Uint64("uvm-vablock-size", 2*mem.MB,
	"UVM VA block size (must be exactly 2MB).")
var uvmTBNMinNodeSizeFlag = flag.Uint64("uvm-tbn-min-node-size", 64*mem.KB,
	"UVM TBN minimum node size (must be exactly 64KB).")
var uvmGPUMemoryCapacityFlag = flag.Uint64("uvm-gpu-memory-capacity", 0,
	"UVM GPU memory capacity in bytes (0 = full GPU DRAM; must be 4KB-aligned and >= 64KB).")
var uvmPrefetcherFlag = flag.String("uvm-prefetcher", "tbn",
	"UVM prefetcher (must be exactly tbn).")

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

// sbin_codex
func uvmGPUMemoryCapacityFlagIsSet() bool {
	isSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "uvm-gpu-memory-capacity" {
			isSet = true
		}
	})

	return isSet
}

// parseUVMConfig builds the driver UVMConfig from the CLI flags and fails
// fast (panic with a descriptive message) on any invalid combination or
// domain. The capacity-vs-DRAM check happens later in the driver builder,
// where the actual GPU memory size is known. sbin_codex
func parseUVMConfig() driver.UVMConfig {
	cfg := driver.DefaultUVMConfig()
	cfg.Enabled = *uvmFlag
	cfg.Ideal = *uvmIdealFlag
	cfg.AccessCounter = *uvmAccessCounterFlag
	cfg.FaultHandlingLatency = *uvmFaultHandlingLatencyFlag
	cfg.AccessCounterThreshold = *uvmAccessCounterThresholdFlag
	cfg.VABlockSize = *uvmVABlockSizeFlag
	cfg.TBNMinNodeSize = *uvmTBNMinNodeSizeFlag
	cfg.GPUMemoryCapacity = *uvmGPUMemoryCapacityFlag
	cfg.CapacitySet = uvmGPUMemoryCapacityFlagIsSet()
	cfg.Prefetcher = *uvmPrefetcherFlag

	if err := cfg.Validate(); err != nil {
		panic(err)
	}

	return cfg
}

// parseFlag applies the runner flag to runner object
func (r *Runner) parseFlag() *Runner {
	r.parseSimulationFlags()
	r.parseGPUFlag()
	r.uvmConfig = parseUVMConfig() // sbin_codex: validated UVM config (fail-fast).

	// sbin_codex: -use-unified-memory and -uvm are mutually exclusive modes.
	// Reject the combination here, at flag-parse time, before any allocation.
	if r.UseUnifiedMemory && r.uvmConfig.Enabled {
		panic("cannot use -use-unified-memory and -uvm together")
	}

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

	r.ArchType = parseArchFlag()
	r.GPUType = parseGPUTypeFlag()
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
