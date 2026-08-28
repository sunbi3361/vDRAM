package gups

import (
	"fmt"
	"log"
	"math/rand"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

// KernelArgs defines kernel arguments
type KernelArgs struct {
	TableSize           uint64
	Table               driver.Ptr
	Starts              driver.Ptr
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

	ThreadBlockSize, NThreadBlocks uint64
	NUpdates                       uint64
	TableSize                      uint64
	DevTable, DevStarts            driver.Ptr
	HostTable, HostStarts          []uint64

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
	b.ThreadBlockSize = 32 // take from gups_kernel.cl
	b.NThreadBlocks = 128  // take from gups_kernel.cl
	// b.TableSize = 2048 * 64 * b.ThreadBlockSize * b.NThreadBlocks
	b.TableSize = 1024 * b.ThreadBlockSize * b.NThreadBlocks
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

func (b *Benchmark) loadProgram() {
	hsacoBytes := _escFSMustByte(false, "/kernels.hsaco")

	b.kernel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "RandomAccessUpdate")
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

func HPCC_Starts(n int64) uint64 {

	return uint64(rand.Int63())

	var PERIOD int64
	PERIOD = 1317624576693539401
	var POLY uint64
	POLY = 7
	var i int

	var m2 [64]uint64

	for n < 0 {
		n += PERIOD
	}
	for n > PERIOD {
		n -= PERIOD
	}
	if n == 0 {
		return 0x1
	}

	var temp uint64
	temp = 1
	for i = 0; i < 64; i++ {
		m2[i] = temp
		if temp < 0 {
			temp = (temp << 1) ^ POLY
		} else {
			temp = (temp << 1)
		}
	}

	for i = 62; i >= 0; i-- {
		if ((n >> i) & 1) != 0 {
			break
		}
	}

	var ran uint64
	ran = 0x2
	for i > 0 {
		temp = 0
		for j := 0; j < 64; j++ {
			if ((ran >> j) & 1) != 0 {
				temp ^= m2[j]
			}
		}
		ran = temp
		i -= 1
		if ((n >> i) & 1) != 0 {
			if ran < 0 {
				ran = (ran << 1) ^ POLY
			} else {
				ran = (ran << 1)
			}
		}
	}

	return ran
}

func (b *Benchmark) initMem() {

	size := b.TableSize * 8
	startsSize := b.NThreadBlocks * b.ThreadBlockSize * 8

	b.HostTable = make([]uint64, b.TableSize)
	b.HostStarts = make([]uint64, b.NThreadBlocks*b.ThreadBlockSize)
	b.NUpdates = 1 * b.NThreadBlocks * b.ThreadBlockSize

	for i := uint64(0); i < b.ThreadBlockSize*b.NThreadBlocks; i++ {
		b.HostStarts[i] = HPCC_Starts(int64((b.NUpdates/b.NThreadBlocks/b.ThreadBlockSize)*i)) % b.TableSize
	}

	if b.useManagedMemory { // sbin_claude
		b.DevTable = b.driver.AllocateManaged(b.context, uint64(size))
		b.DevStarts = b.driver.AllocateManaged(b.context, uint64(startsSize))
	} else if b.useUnifiedMemory {
		b.DevTable = b.driver.AllocateUnifiedMemory(b.context, uint64(size))
		b.DevStarts = b.driver.AllocateUnifiedMemory(b.context, uint64(startsSize))
	} else {
		b.DevTable = b.driver.AllocateMemory(b.context, uint64(size))
		b.DevStarts = b.driver.AllocateMemory(b.context, uint64(startsSize))
	}

	fmt.Printf("Footprint: %.3f MB\n",
		float64(uint64(size)+uint64(startsSize))/1024.0/1024.0)

	b.driver.MemCopyH2D(b.context, b.DevTable, b.HostTable)   // sbin_claude
	b.driver.MemCopyH2D(b.context, b.DevStarts, b.HostStarts) // sbin_claude
}

func (b *Benchmark) exec() {
	kernArg := KernelArgs{
		b.TableSize,
		b.DevTable,
		b.DevStarts,
		0, 0, 0,
	}

	b.driver.EnqueueLaunchKernel(
		b.queue,
		b.kernel,
		[3]uint32{uint32(b.ThreadBlockSize * b.NThreadBlocks), uint32(1), 1},
		[3]uint16{uint16(b.ThreadBlockSize), uint16(1), 1},
		&kernArg,
	)
	b.driver.DrainCommandQueue(b.queue)
}

// Verify verifies
func (b *Benchmark) Verify() {
	log.Printf("How will it pass if it is not implemented at all?")
}
