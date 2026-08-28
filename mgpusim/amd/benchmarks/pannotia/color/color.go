package color

import (
	"fmt"
	"log"

	// embed hsaco files
	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/matrix/csr"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

// Kernel1Args list first set of kernel arguments
type Kernel1Args struct {
	Row                 driver.Ptr
	Col                 driver.Ptr
	Node_value          driver.Ptr
	Color_array         driver.Ptr
	Stop                driver.Ptr
	Max_d               driver.Ptr
	Color               int32
	Num_nodes           int32
	Num_edges           int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel2Args list first set of kernel arguments
type Kernel2Args struct {
	Node_value          driver.Ptr
	Color_array         driver.Ptr
	Max_d               driver.Ptr
	Color               int32
	Num_nodes           int32
	Num_edges           int32
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

	NumNodes, NumEdges int
	matrix             csr.Matrix
	color              []int32
	colorD             driver.Ptr
	maxD               driver.Ptr
	rowD, colD         driver.Ptr
	nodeValueD         driver.Ptr
	stopD              driver.Ptr

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
		hsacoBytes, "color")
	if b.kernel1 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel2 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "color2")
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
	b.matrix = csr.
		MakeMatrixGenerator(uint32(b.NumNodes), uint32(b.NumEdges)).
		GenerateMatrix()

	if b.useManagedMemory { // sbin_claude
		b.colorD = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
		b.maxD = b.driver.AllocateManaged(b.context,
			uint64((b.NumNodes)*4)) //numNodes+1 was here
		// sbin_gmmu_omo: original: b.rowD = ... uint64(b.NumNodes*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; the kernels
		// read Row[node+1] for the last node, so allocate N+1 slots.
		b.rowD = b.driver.AllocateManaged(b.context,
			uint64((b.NumNodes+1)*4))
		b.colD = b.driver.AllocateManaged(b.context,
			uint64(b.NumEdges*4))
		b.nodeValueD = b.driver.AllocateManaged(b.context,
			uint64(b.NumEdges*4))
		b.stopD = b.driver.AllocateManaged(b.context,
			uint64(4))

		fmt.Printf("Footprint: %.3f MB\n",
			float64(uint64(b.NumNodes*4)*3+uint64(b.NumEdges*4)*2)/1024.0/1024.0)
	} else if b.useUnifiedMemory {
		b.colorD = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
		b.maxD = b.driver.AllocateUnifiedMemory(b.context,
			uint64((b.NumNodes)*4)) //numNodes+1 was here
		// sbin_gmmu_omo: original: b.rowD = ... uint64(b.NumNodes*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; the kernels
		// read Row[node+1] for the last node, so allocate N+1 slots.
		b.rowD = b.driver.AllocateUnifiedMemory(b.context,
			uint64((b.NumNodes+1)*4))
		b.colD = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumEdges*4))
		b.nodeValueD = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumEdges*4))
		b.stopD = b.driver.AllocateUnifiedMemory(b.context,
			uint64(4))

		fmt.Printf("Footprint: %.3f MB\n",
			float64(uint64(b.NumNodes*4)*3+uint64(b.NumEdges*4)*2)/1024.0/1024.0)
	} else {
		// sbin_gmmu_omo: original: b.rowD = ... uint64(b.NumNodes*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; allocate N+1
		b.rowD = b.driver.AllocateMemory(b.context,
			uint64((b.NumNodes+1)*4))

		b.colD = b.driver.AllocateMemory(b.context,
			uint64(b.NumEdges*4))

		b.stopD = b.driver.AllocateMemory(b.context,
			uint64(4))

		b.colorD = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
		b.nodeValueD = b.driver.AllocateMemory(b.context,
			uint64(b.NumEdges*4))
		b.maxD = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4)) //numNodes+1 was here
	}
}

func (b *Benchmark) exec() {

	// sbin_gmmu_omo: the color array must be int32: the kernels compare
	// uncolored entries with an integer -1 (v_cmp_eq_u32), so the initial
	// value must be 0xFFFFFFFF, not the float32 -1.0 (0xBF800000) that the
	// old code uploaded. With the float value every node looked already
	// colored, no node was ever colored and the loop ran once.
	b.color = make([]int32, b.NumNodes)
	for n := 0; n < b.NumNodes; n++ {
		b.color[n] = -1
	}
	// sbin_gmmu_omo: original: b.driver.MemCopyH2D(b.context, b.maxD, b.color)
	// sbin_gmmu_omo: original: b.driver.MemCopyH2D(b.context, b.maxD, b.color)
	// sbin_gmmu_omo: max_d is compared as float32 by the kernels, so it
	// must be initialized with -1.0f independently of the int32 color array.
	maxDInit := make([]float32, b.NumNodes)
	for n := 0; n < b.NumNodes; n++ {
		maxDInit[n] = -1.0
	}

	b.driver.MemCopyH2D(b.context, b.colorD, b.color)              // sbin_claude
	b.driver.MemCopyH2D(b.context, b.maxD, maxDInit)               // sbin_claude
	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)    // sbin_claude
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.nodeValueD, b.matrix.Values)  // sbin_claude

	blockSize := 64 // BLOCKSIZE
	globalSize := b.NumNodes
	if b.NumNodes%blockSize > 0 {
		globalSize = (b.NumNodes/blockSize + 1) * blockSize
	}

	globalWork := [3]uint32{uint32(globalSize), 1, 1}
	localWork := [3]uint16{uint16(blockSize), 1, 1}

	stop := int32(1)
	graphColor := int32(1)

	args1 := Kernel1Args{
		Row:                 b.rowD,
		Col:                 b.colD,
		Node_value:          b.nodeValueD,
		Color_array:         b.colorD,
		Stop:                b.stopD,
		Max_d:               b.maxD,
		Num_nodes:           int32(b.NumNodes),
		Num_edges:           int32(b.NumEdges),
		HiddenGlobalOffsetX: 0,
		HiddenGlobalOffsetY: 0,
		HiddenGlobalOffsetZ: 0,
	}

	args2 := Kernel2Args{
		Node_value:          b.nodeValueD,
		Color_array:         b.colorD,
		Max_d:               b.maxD,
		Num_nodes:           int32(b.NumNodes),
		Num_edges:           int32(b.NumEdges),
		HiddenGlobalOffsetX: 0,
		HiddenGlobalOffsetY: 0,
		HiddenGlobalOffsetZ: 0,
	}

	for stop > 0 {
		stop = 0
		b.driver.MemCopyH2D(b.context, b.stopD, &stop)
		args1.Color = graphColor
		args2.Color = graphColor
		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel1,
			globalWork, localWork,
			&args1,
		)
		b.driver.DrainCommandQueue(b.queue)
		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel2,
			globalWork, localWork,
			&args2,
		)
		b.driver.DrainCommandQueue(b.queue)
		b.driver.MemCopyD2H(b.context, &stop, b.stopD)
		graphColor++
	}

}

func (b *Benchmark) Verify() {
	return
}
