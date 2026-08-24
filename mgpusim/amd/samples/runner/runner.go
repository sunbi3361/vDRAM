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

	GPUIDs     []int
	benchmarks []benchmarks.Benchmark

	uvmConfig driver.UVMConfig // sbin_codex: validated UVM config from -uvm* flags.

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
		WithGPUType(r.GPUType).
		WithUVMConfig(r.uvmConfig) // sbin_codex: pass validated UVM config to the timing platform.

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
	// if r.UseUnifiedMemory {
	// 	b.SetUnifiedMemory()
	// }
	// sbin_codex: propagate the managed-memory capability when -uvm is
	// enabled; -use-unified-memory takes precedence (the two modes are
	// mutually exclusive and rejected together in parseFlag).
	if r.UseUnifiedMemory {
		b.SetUnifiedMemory()
	} else if r.uvmConfig.Enabled {
		if bm, ok := b.(benchmarks.ManagedMemoryCapable); ok {
			bm.SetManagedMemory()
		}
	}

	r.benchmarks = append(r.benchmarks, b)
}

// AddBenchmarkWithoutSettingGPUsToUse allows for user specified GPUs for
// the benchmark to run.
func (r *Runner) AddBenchmarkWithoutSettingGPUsToUse(b benchmarks.Benchmark) {
	// if r.UseUnifiedMemory {
	// 	b.SetUnifiedMemory()
	// }
	// sbin_codex: same managed-memory capability propagation as AddBenchmark.
	if r.UseUnifiedMemory {
		b.SetUnifiedMemory()
	} else if r.uvmConfig.Enabled {
		if bm, ok := b.(benchmarks.ManagedMemoryCapable); ok {
			bm.SetManagedMemory()
		}
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

	if r.reporter != nil {
		r.reporter.report()
		r.reporter.dataRecorder.Flush()
	}

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
