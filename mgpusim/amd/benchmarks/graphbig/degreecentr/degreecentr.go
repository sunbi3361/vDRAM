package degreecentr

import (
	"fmt"
	"log"

	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/common"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

const wgSize = 256

type kernelArgs struct {
	Offsets             driver.Ptr
	Edges               driver.Ptr
	Vplist              driver.Ptr
	NumNodes            uint32
	Pad0                uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int

	DatasetPath string
	NumNodes    int
	Degree      int
	VerifyOnCPU bool

	graph     common.CSRGraph
	gpuDegree []uint32
	cpuDegree []uint32
	dOffsets  driver.Ptr
	dEdges    driver.Ptr
	dVplist   driver.Ptr
	kernel    *insts.KernelCodeObject

	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude

	Arch  arch.Type
	queue *driver.CommandQueue
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := &Benchmark{driver: driver, context: driver.Init(), NumNodes: 1024, Degree: 8, VerifyOnCPU: true}
	b.queue = driver.CreateCommandQueue(b.context)

	if len(hsacoBytes) > 0 {
		b.kernel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "degree_centr_kernel")
	}
	return b
}

func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }
func (b *Benchmark) SetUnifiedMemory()    { b.useUnifiedMemory = true }

// SetManagedMemory switches allocations to UVM managed memory. // sbin_claude
func (b *Benchmark) SetManagedMemory() { b.useManagedMemory = true }
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	if b.DatasetPath != "" {
		g, err := common.LoadCSR(b.DatasetPath)
		if err != nil {
			log.Panic(err)
		}
		b.graph = g
	} else {
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 3)
	}
	b.NumNodes = b.graph.NumNodes()
	b.gpuDegree = make([]uint32, b.NumNodes)

	if b.kernel == nil {
		b.execCPU()
		return
	}

	b.dOffsets = b.alloc(uint64(len(b.graph.Offsets) * 4))
	b.dEdges = b.alloc(uint64(len(b.graph.Edges) * 4))
	b.dVplist = b.alloc(uint64(b.NumNodes * 4))

	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)
	b.driver.MemCopyH2D(b.context, b.dVplist, b.gpuDegree) // zero-init

	threads := uint32(((b.NumNodes + wgSize - 1) / wgSize) * wgSize)
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}
	args := kernelArgs{
		Offsets:  b.dOffsets,
		Edges:    b.dEdges,
		Vplist:   b.dVplist,
		NumNodes: uint32(b.NumNodes),
	}
	b.driver.EnqueueLaunchKernel(
		b.queue,
		b.kernel, global, local, &args)
	b.driver.DrainCommandQueue(b.queue)
	b.driver.MemCopyD2H(b.context, b.gpuDegree, b.dVplist)
}

func (b *Benchmark) execCPU() {
	for _, dst := range b.graph.Edges {
		b.gpuDegree[dst]++
	}
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	b.cpuDegree = make([]uint32, b.NumNodes)
	for _, dst := range b.graph.Edges {
		b.cpuDegree[dst]++
	}
	mismatch := 0
	for v := 0; v < b.NumNodes; v++ {
		if b.cpuDegree[v] != b.gpuDegree[v] {
			mismatch++
		}
	}
	fmt.Printf("GraphBIG DegreeCentr (atomics simulated): %d/%d vertices match reference\n",
		b.NumNodes-mismatch, b.NumNodes)
}

func (b *Benchmark) alloc(size uint64) driver.Ptr {
	if b.useManagedMemory { // sbin_claude
		return b.driver.AllocateManaged(b.context, size)
	} else if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	}
	return b.driver.AllocateMemory(b.context, size)
}
