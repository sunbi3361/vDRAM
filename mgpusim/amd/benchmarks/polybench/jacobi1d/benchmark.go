// Package jacobi1d implements the jacobi1d benchmark from Polybench.
package jacobi1d

import (
	"fmt"
	"log"
	"math"
	"math/rand"

	// embed hsaco files
	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

// Kernel1Args list first set of kernel arguments
type Kernel1Args struct {
	A                   driver.Ptr
	B                   driver.Ptr
	N                   int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel2Args list first set of kernel arguments
type Kernel2Args struct {
	A                   driver.Ptr
	B                   driver.Ptr
	N                   int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark defines a benchmark
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	queue   *driver.CommandQueue
	gpus    []int
	kernel1 *insts.KernelCodeObject
	kernel2 *insts.KernelCodeObject

	a, b            []float32
	N, Steps        int
	da, db          driver.Ptr
	a_outputFromGPU []float32
	b_outputFromGPU []float32

	Arch             arch.Type
	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude
}

// NewBenchmark makes a new benchmark
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = driver.Init()
	b.queue = driver.CreateCommandQueue(b.context)
	return b
}

// SelectGPU selects GPU
func (b *Benchmark) SelectGPU(gpus []int) {
	b.gpus = gpus
}

// SetUnifiedMemory uses Unified Memory
func (b *Benchmark) SetUnifiedMemory() {
	b.useUnifiedMemory = true
}

// SetManagedMemory switches allocations to UVM managed memory. // sbin_claude
func (b *Benchmark) SetManagedMemory() {
	b.useManagedMemory = true
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func (b *Benchmark) loadProgram() {
	b.kernel1 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "runJacobi1D_kernel1")
	if b.kernel1 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel2 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "runJacobi1D_kernel2")
	if b.kernel2 == nil {
		log.Panic("Failed to load kernel binary")
	}
}

// Run runs
func (b *Benchmark) Run() {
	b.loadProgram()
	b.driver.SelectGPU(b.context, b.gpus[0])

	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	rand.Seed(1)
	b.a = make([]float32, b.N)
	b.b = make([]float32, b.N)
	b.a_outputFromGPU = make([]float32, b.N)
	b.b_outputFromGPU = make([]float32, b.N)

	for i := 0; i < b.N; i++ {
		b.a[i] = float32(i*4+10) / float32(b.N)
		b.b[i] = float32(i*7+11) / float32(b.N)
	}

	if b.useManagedMemory { // sbin_claude
		b.da = b.driver.AllocateManaged(b.context,
			uint64(b.N*4))
		b.db = b.driver.AllocateManaged(b.context,
			uint64(b.N*4))
	} else if b.useUnifiedMemory {
		b.da = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*4))
		b.db = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*4))
	} else {
		b.da = b.driver.AllocateMemory(b.context,
			uint64(b.N*4))
		b.db = b.driver.AllocateMemory(b.context,
			uint64(b.N*4))
	}

	fmt.Printf("Footprint: %.3f MB\n",
		float64(uint64(b.N*4)*2)/1024.0/1024.0)
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.da, b.a) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.db, b.b) // sbin_claude

	localSize := [3]uint16{256, 1, 1}
	globalSizeX := uint32(((b.N-1)/256 + 1) * 256)
	globalSize := [3]uint32{globalSizeX, 1, 1}

	for t := 0; t < b.Steps; t++ {
		kernel1Arg := Kernel1Args{
			b.da,
			b.db,
			int32(b.N),
			0,
			0, 0, 0,
		}
		b.driver.EnqueueLaunchKernel(b.queue, b.kernel1,
			globalSize, localSize, &kernel1Arg)
		b.driver.DrainCommandQueue(b.queue)

		kernel2Arg := Kernel2Args{
			b.da,
			b.db,
			int32(b.N),
			0,
			0, 0, 0,
		}
		b.driver.EnqueueLaunchKernel(b.queue, b.kernel2,
			globalSize, localSize, &kernel2Arg)
		b.driver.DrainCommandQueue(b.queue)
	}

	b.driver.MemCopyD2H(b.context, b.a_outputFromGPU, b.da)
	b.driver.MemCopyD2H(b.context, b.b_outputFromGPU, b.db)
}

// Verify verifies
func (b *Benchmark) Verify() {
	b.cpujacobi1d()

	// allow some amount of slack (not 0.001).
	for i := 1; i < b.N-1; i++ {
		if math.Abs(float64(b.a_outputFromGPU[i]-b.a[i])/float64(b.a[i])) > 0.01 {
			log.Panicf("Mismatch at %d, expected %f, but get %f",
				i,
				b.a[i],
				b.a_outputFromGPU[i])
		}
	}
	for i := 1; i < b.N-1; i++ {
		if math.Abs(float64(b.b_outputFromGPU[i]-b.b[i])/float64(b.b[i])) > 0.01 {
			log.Panicf("Mismatch at %d, expected %f, but get %f",
				i,
				b.b[i],
				b.b_outputFromGPU[i])
		}
	}

	log.Printf("Passed!\n")
}

func (b *Benchmark) cpujacobi1d() {
	for t := 0; t < b.Steps; t++ {
		for i := 1; i < b.N-1; i++ {
			b.b[i] = 0.3333 * (b.a[i-1] + b.a[i] + b.a[i+1])
		}
		for j := 1; j < b.N-1; j++ {
			b.a[j] = b.b[j]
		}
	}
}
