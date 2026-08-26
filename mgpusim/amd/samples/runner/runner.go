// Package runner defines how default benchmark samples are executed.
package runner

import (
	"log"

	// Enable profiling
	_ "net/http/pprof"
	"sync"

	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/simulation"
	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/emusystem"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner/timingconfig"
	"github.com/sarchlab/mgpusim/v4/amd/sampling"
	"github.com/tebeka/atexit"
)

type verificationPreEnablingBenchmark interface {
	benchmarks.Benchmark

	EnableVerification()
}

// Runner is a class that helps running the benchmarks in the official samples.
type Runner struct {
	simulation *simulation.Simulation
	platform   *sim.Domain
	reporter   *reporter

	Timing           bool
	Verify           bool
	Parallel         bool
	UseUnifiedMemory bool
	ArchType         arch.Type
	GPUType          string

	// sbin_codex: UVM demand-paging configuration.
	UVM                 bool
	IdealUVM            bool
	UVMFaultLatencyUS   float64
	UVMAccessCounter    bool
	UVMACThreshold      uint64
	UVMExpandRatio      float64
	UVMmaxFetchSize     uint64
	UVMNoPrefetch       bool
	UVMNoEviction       bool
	UVMGPUCapacityBytes uint64
	UVMGPUCapacityRatio float64
	UVMOversubRatio     float64

	// sbin_claude_utopia: Utopia hybrid RestSeg/FlexSeg configuration.
	UtopiaRestSegRatio     float64
	UtopiaRestSegBytes     uint64
	UtopiaRestSegAssoc     int
	UtopiaTARCacheBytes    uint64
	UtopiaSFCacheBytes     uint64
	UtopiaTARSFHitLatency  int
	UtopiaTARSFMissLatency int

	// sbin_claude_avatar: Avatar speculative-translation configuration.
	AvatarCompressRatio       float64
	AvatarValidationLatency   int
	AvatarModEntries          int
	AvatarConfidenceThreshold int
	AvatarFrag                bool

	// sbin_claude_hpt: FS-HPT hashed-page-table configuration.
	HPTAccessesPerWalk int

	GPUIDs     []int
	benchmarks []benchmarks.Benchmark

	reported bool // sbin_codex
}

// Init initializes the platform simulate
func (r *Runner) Init() *Runner {
	r.parseFlag()

	log.SetFlags(log.Llongfile | log.Ldate | log.Ltime)

	r.initSimulation()

	if r.Timing {
		r.buildTimingPlatform()
	} else {
		r.buildEmuPlatform()
	}

	r.createUnifiedGPUs()

	return r
}

func (r *Runner) initSimulation() {
	builder := simulation.MakeBuilder()

	if *parallelFlag {
		builder = builder.WithParallelEngine()
	}

	if *disableAkitaRTM {
		builder = builder.WithoutMonitoring()
	} else if *customPortForAkitaRTM > 0 {
		builder = builder.WithMonitorPort(*customPortForAkitaRTM)
	}

	if *visTracing {
		builder = builder.WithVisTracingOnStart()
	}

	// sbin_codex
	if metricFileNameFlagIsSet() {
		builder = builder.WithOutputFileName(*filenameFlag)
	}

	r.simulation = builder.Build()
}

func (r *Runner) buildEmuPlatform() {
	b := emusystem.MakeBuilder().
		WithSimulation(r.simulation).
		WithNumGPUs(r.GPUIDs[len(r.GPUIDs)-1]).
		WithArchitecture(r.ArchType)

	if *isaDebug {
		b = b.WithDebugISA()
	}

	r.platform = b.Build()
}

func (r *Runner) buildTimingPlatform() {
	sampling.InitSampledEngine()

	b := timingconfig.MakeBuilder().
		WithSimulation(r.simulation).
		WithNumGPUs(r.GPUIDs[len(r.GPUIDs)-1]).
		WithGPUType(r.GPUType)

	if r.UVM {
		b = b.WithUVM(timingconfig.UVMPlatformConfig{ // sbin_codex
			Enabled:                r.UVM,
			Ideal:                  r.IdealUVM,
			FaultLatencyUS:         r.UVMFaultLatencyUS,
			AccessCounterEnabled:   r.UVMAccessCounter,
			AccessCounterThreshold: r.UVMACThreshold,
			TBNExpandRatio:         r.UVMExpandRatio,
			TBNMaxFetchSize:        r.UVMmaxFetchSize,
			PrefetchDisabled:       r.UVMNoPrefetch,
			EvictionDisabled:       r.UVMNoEviction,
			GPUCapacityBytes:       r.UVMGPUCapacityBytes,
			GPUCapacityRatio:       r.UVMGPUCapacityRatio,
			OversubscriptionRatio:  r.UVMOversubRatio,
		})
	}

	// sbin_claude_utopia: hand the Utopia knobs to the platform builder. They
	// only take effect with -gpu=utopia.
	if r.GPUType == "utopia" {
		b = b.WithUtopia(timingconfig.UtopiaPlatformConfig{
			RestSegRatio:  r.UtopiaRestSegRatio,
			RestSegBytes:  r.UtopiaRestSegBytes,
			Associativity: r.UtopiaRestSegAssoc,
			TARCacheBytes: r.UtopiaTARCacheBytes,
			SFCacheBytes:  r.UtopiaSFCacheBytes,
			HitLatency:    r.UtopiaTARSFHitLatency,
			MissLatency:   r.UtopiaTARSFMissLatency,
		})
	}

	// sbin_claude_avatar: hand the Avatar knobs to the platform builder.
	// They only take effect with -gpu=avatar.
	if r.GPUType == "avatar" {
		b = b.WithAvatar(timingconfig.AvatarPlatformConfig{
			CompressRatio:       r.AvatarCompressRatio,
			ValidationLatency:   r.AvatarValidationLatency,
			ModEntries:          r.AvatarModEntries,
			ConfidenceThreshold: r.AvatarConfidenceThreshold,
			FragDisabled:        !r.AvatarFrag,
		})
	}

	// sbin_claude_hpt: hand the HPT knob to the platform builder. It only
	// takes effect with -gpu=hpt.
	if r.GPUType == "hpt" {
		b = b.WithHPT(timingconfig.HPTPlatformConfig{
			AccessesPerWalk: r.HPTAccessesPerWalk,
		})
	}

	if *magicMemoryCopy {
		b = b.WithMagicMemoryCopy()
	}

	r.platform = b.Build()
	r.reporter = newReporter(r.simulation)
}

func (r *Runner) createUnifiedGPUs() {
	if *unifiedGPUFlag == "" {
		return
	}

	driver := r.simulation.GetComponentByName("Driver").(*driver.Driver)
	unifiedGPUID := driver.CreateUnifiedGPU(nil, r.GPUIDs)
	r.GPUIDs = []int{unifiedGPUID}
}

// AddBenchmark adds an benchmark that the driver runs
func (r *Runner) AddBenchmark(b benchmarks.Benchmark) {
	b.SelectGPU(r.GPUIDs)
	if r.UseUnifiedMemory {
		b.SetUnifiedMemory()
	}
	if r.UVM {
		b.SetManagedMemory()
	}

	r.benchmarks = append(r.benchmarks, b)
}

// AddBenchmarkWithoutSettingGPUsToUse allows for user specified GPUs for
// the benchmark to run.
func (r *Runner) AddBenchmarkWithoutSettingGPUsToUse(b benchmarks.Benchmark) {
	if r.UseUnifiedMemory {
		b.SetUnifiedMemory()
	}
	if r.UVM {
		b.SetManagedMemory()
	}

	r.benchmarks = append(r.benchmarks, b)
}

// sbin_codex: maxInstOnce ensures the -max-inst stop (flush + exit) runs
// exactly once, even though every CU's stopper tracer can cross the limit
// in the same parallel engine round.
var maxInstOnce sync.Once

// Run runs the benchmark
func (r *Runner) Run() {
	// sbin_codex: max_inst
	progressInterval = *progressIntervalFlag
	if *maxInstCount > 0 {
		onMaxInstReached = func() {
			maxInstOnce.Do(func() {
				r.flushReport()
				// sbin_codex: Simulation.Terminate() is never reached on the
				// -max-inst exit path, and only the recorder's Close() writes
				// the exec_info rows (start time, command, end time). Close it
				// here so a truncated run still records them.
				if err := r.simulation.GetDataRecorder().Close(); err != nil {
					log.Printf("failed to close data recorder: %v", err)
				}
				atexit.Exit(0)
			})
		}
	}
	atexit.Register(func() {
		r.flushReport()
	})

	r.Driver().Run()

	var wg sync.WaitGroup
	for _, b := range r.benchmarks {
		wg.Add(1)
		go func(b benchmarks.Benchmark, wg *sync.WaitGroup) {
			if r.Verify {
				if b, ok := b.(verificationPreEnablingBenchmark); ok {
					b.EnableVerification()
				}
			}

			b.Run()

			if r.Verify {
				b.Verify()
			}
			wg.Done()
		}(b, &wg)
	}
	wg.Wait()

	// Pre-edit code (commented per AGENTS.md convention):
	// if r.reporter != nil {
	// 	r.reporter.report()
	// 	r.reporter.dataRecorder.Flush()
	// }
	r.flushReport() // sbin_codex: single reporting path; marks reported so a late atexit flush cannot duplicate rows.

	r.Driver().Terminate()
	r.simulation.Terminate()
}

// sbin_codex: flushReport writes the collected metrics once. It is safe
// to call from the normal completion path and from the -max-inst atexit handler
// (which flushes partial metrics before the process exits).
func (r *Runner) flushReport() {
	if r.reporter == nil || r.reported {
		return
	}
	// sbin_codex: close out kernels still in flight so a run truncated by
	// -max-inst reports the partial kernel time instead of 0. On a normal
	// completion no kernel task is in flight and this is a no-op.
	r.reporter.terminateInflightKernels(r.Engine().CurrentTime())
	r.reporter.report()
	r.reporter.dataRecorder.Flush()
	r.reported = true
}

// Driver returns the GPU driver used by the current runner.
func (r *Runner) Driver() *driver.Driver {
	return r.simulation.GetComponentByName("Driver").(*driver.Driver)
}

// Engine returns the event-driven simulation engine used by the current runner.
func (r *Runner) Engine() sim.Engine {
	return r.simulation.GetEngine()
}
