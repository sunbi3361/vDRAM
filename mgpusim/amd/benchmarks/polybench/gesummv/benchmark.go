// Package gesummv implements the gesummv benchmark from Polybench.
package gesummv

import (
	"fmt"
	"log"

	// "math"
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
	X                   driver.Ptr
	Y                   driver.Ptr
	Tmp                 driver.Ptr
	Alpha               float32
	Beta                float32
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

	N           int
	a, b, x, y  []float32
	tmp         []float32
	dA, dB, dX  driver.Ptr
	dY, dTmp    driver.Ptr
	yOutput     []float32
	alpha, beta float32
	Alpha, Beta float32

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
		hsacoBytes, "gesummv_kernel")
	if b.kernel1 == nil {
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
	b.alpha = 43532.0
	b.beta = 12313.0
	b.a = make([]float32, b.N*b.N)
	b.b = make([]float32, b.N*b.N)
	b.x = make([]float32, b.N)
	b.y = make([]float32, b.N)
	b.tmp = make([]float32, b.N)
	b.yOutput = make([]float32, b.N)

	for i := 0; i < b.N; i++ {
		b.x[i] = float32(i) / float32(b.N)
		for j := 0; j < b.N; j++ {
			b.a[i*b.N+j] = float32(i) * float32(j) / float32(b.N)
			b.b[i*b.N+j] = float32(i) * float32(j) / float32(b.N)
		}
	}

	if b.useManagedMemory { // sbin_claude
		b.dA = b.driver.AllocateManaged(b.context,
			uint64(b.N*b.N*4))
		b.dB = b.driver.AllocateManaged(b.context,
			uint64(b.N*b.N*4))
		b.dX = b.driver.AllocateManaged(b.context,
			uint64(b.N*4))
		b.dY = b.driver.AllocateManaged(b.context,
			uint64(b.N*4))
		b.dTmp = b.driver.AllocateManaged(b.context,
			uint64(b.N*4))
	} else if b.useUnifiedMemory {
		b.dA = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*b.N*4))
		b.dB = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*b.N*4))
		b.dX = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*4))
		b.dY = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*4))
		b.dTmp = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.N*4))
	} else {
		b.dA = b.driver.AllocateMemory(b.context,
			uint64(b.N*b.N*4))
		b.dB = b.driver.AllocateMemory(b.context,
			uint64(b.N*b.N*4))
		b.dX = b.driver.AllocateMemory(b.context,
			uint64(b.N*4))
		b.dY = b.driver.AllocateMemory(b.context,
			uint64(b.N*4))
		b.dTmp = b.driver.AllocateMemory(b.context,
			uint64(b.N*4))
	}
	fmt.Printf("Footprint: %.3f MB\n",
		float64(uint64(b.N*b.N*4)*2+uint64(b.N*4)*3)/1024.0/1024.0)
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.dA, b.a) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.dB, b.b) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.dX, b.x) // sbin_claude

	// width := 256
	width := 64
	localSize := [3]uint16{uint16(width), 1, 1}
	globalSizeX := uint32(((b.N-1)/width + 1) * width)
	// globalSizeY := uint32(((b.N-1)/1 + 1) * 1)
	globalSize := [3]uint32{globalSizeX, 1, 1}

	kernel1Arg := Kernel1Args{
		b.dA,
		b.dB,
		b.dX,
		b.dY,
		b.dTmp,
		float32(b.alpha),
		float32(b.beta),
		int32(b.N),
		0,
		0, 0, 0,
	}
	b.driver.EnqueueLaunchKernel(b.queue, b.kernel1,
		globalSize, localSize, &kernel1Arg)
	b.driver.DrainCommandQueue(b.queue)

	b.driver.MemCopyD2H(b.context, b.yOutput, b.dY)
}

// Verify verifies
func (b *Benchmark) Verify() {
	b.cpugesummv()

	for i := 0; i < b.N; i++ {
		if b.y[i] != b.yOutput[i] {
			log.Panicf("Mismatch at %d, expected %f, but get %f",
				i, b.y[i], b.yOutput[i])
		}
	}

	log.Printf("Passed!\n")
}

func (b *Benchmark) cpugesummv() {
	for i := 0; i < b.N; i++ {
		b.tmp[i] = 0.0
		b.y[i] = 0.0
		for j := 0; j < b.N; j++ {
			b.tmp[i] = b.a[i*b.N+j]*b.x[j] + b.tmp[i]
			b.y[i] = b.b[i*b.N+j]*b.x[j] + b.y[i]
		}
		b.y[i] = b.alpha*b.tmp[i] + b.beta*b.y[i]
	}
}
