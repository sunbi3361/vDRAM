// Package matrixtranspose implements the matrix transpose benchmark from
// AMDAPPSDK.
package reduction

import (
	"fmt"
	"log"

	// embed hsaco files
	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

// KernelArgs defines kernel arguments
type KernelArgs struct {
	Input               driver.Ptr
	Output              driver.Ptr
	SData               driver.LocalPtr
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

	kernel *insts.KernelCodeObject

	GroupSize, VectorSize, Multiply uint32
	Length                          uint32

	Input, Output, SData          []int32
	DevInput, DevOutput, DevSData driver.Ptr

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
	b.Length = 4096
	b.GroupSize = 256
	b.VectorSize = 1
	b.Multiply = 1
	return b
}

// SelectGPU selects GPU
func (b *Benchmark) SelectGPU(gpus []int) {
	b.gpus = gpus
}

// SetUnifiedMemory use Unified Memory
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
	b.kernel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "reduce")
	if b.kernel == nil {
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

	b.Length = b.Length / b.VectorSize
	b.Input = make([]int32, b.Length)
	b.Output = make([]int32, b.Length)
	b.SData = make([]int32, b.Length)

	numData := b.Length

	for i := 0; i < int(numData); i++ {
		b.Input[i] = int32(i)
	}

	if b.useManagedMemory { // sbin_claude
		b.DevInput = b.driver.AllocateManaged(
			b.context, uint64(numData*4))
		b.DevOutput = b.driver.AllocateManaged(
			b.context, uint64(numData*4))
		b.DevSData = b.driver.AllocateManaged(
			b.context, uint64(numData*4))
	} else if b.useUnifiedMemory {
		b.DevInput = b.driver.AllocateUnifiedMemory(
			b.context, uint64(numData*4))
		b.DevOutput = b.driver.AllocateUnifiedMemory(
			b.context, uint64(numData*4))
		b.DevSData = b.driver.AllocateUnifiedMemory(
			b.context, uint64(numData*4))
	} else {
		b.DevInput = b.driver.AllocateMemory(
			b.context, uint64(numData*4))
		b.DevOutput = b.driver.AllocateMemory(
			b.context, uint64(numData*4))
		b.DevSData = b.driver.AllocateMemory(
			b.context, uint64(numData*4))
		// b.driver.Distribute(b.context, b.DevInput, uint64(numData*4), b.gpus)
		// b.driver.Distribute(b.context, b.DevOutput, uint64(numData*4), b.gpus)
		// b.driver.Distribute(b.context, b.DevSData, uint64(numData*4), b.gpus)
	}

	fmt.Printf("Footprint: %.3f MB\n",
		float64(uint64(numData*4)*3)/1024.0/1024.0)

	b.driver.MemCopyH2D(b.context, b.DevInput, b.Input) // sbin_claude
}

func (b *Benchmark) exec() {
	kernArg := KernelArgs{
		b.DevInput,
		b.DevOutput,
		driver.LocalPtr(b.GroupSize * 4),
		0, 0, 0,
	}

	b.driver.EnqueueLaunchKernel(
		b.queue,
		b.kernel,
		[3]uint32{uint32(b.Length / b.Multiply), 1, 1},
		[3]uint16{uint16(b.GroupSize), 1, 1},
		&kernArg,
	)
	b.driver.DrainCommandQueue(b.queue)

	b.driver.MemCopyD2H(b.context, b.Output, b.DevOutput)
}

// Verify verifies
func (b *Benchmark) Verify() {
	log.Printf("How will it pass if it is not implemented at all?")
}
