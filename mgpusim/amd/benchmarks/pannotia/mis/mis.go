package mis

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
	SArrayD             driver.Ptr
	CArrayD             driver.Ptr
	CArrayUD            driver.Ptr
	NumNodes            int32
	NumEdges            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel2Args list first set of kernel arguments
type Kernel2Args struct {
	RowD                driver.Ptr
	ColD                driver.Ptr
	NodeValueD          driver.Ptr
	SArrayD             driver.Ptr
	CArrayD             driver.Ptr
	MinArrayD           driver.Ptr
	StopD               driver.Ptr
	NumNodes            int32
	NumEdges            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel3Args list first set of kernel arguments
type Kernel3Args struct {
	RowD                driver.Ptr
	ColD                driver.Ptr
	NodeValueD          driver.Ptr
	SArrayD             driver.Ptr
	CArrayD             driver.Ptr
	CArrayUD            driver.Ptr
	MinArrayD           driver.Ptr
	NumNodes            int32
	NumEdges            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel4Args list first set of kernel arguments
type Kernel4Args struct {
	CArrayUD            driver.Ptr
	CArrayD             driver.Ptr
	NumNodes            int32
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
	kernel3 *insts.KernelCodeObject
	kernel4 *insts.KernelCodeObject

	NumNodes, NumEdges int
	matrix             csr.Matrix
	node_value         []float32
	stop               []int32
	row_d, col_d       driver.Ptr
	min_array_d        driver.Ptr
	c_array_d          driver.Ptr
	c_array_u_d        driver.Ptr
	s_array_d          driver.Ptr
	node_value_d       driver.Ptr
	stop_d             driver.Ptr

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
		hsacoBytes, "init")
	if b.kernel1 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel2 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "mis1")
	if b.kernel2 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel3 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "mis2")
	if b.kernel3 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel4 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "mis3")
	if b.kernel4 == nil {
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

// sbin_gmmu_omo: symMatrix symmetrizes a directed CSR graph so that every
// edge appears in both directions. The MIS kernels mark only outgoing
// neighbors as excluded when a node joins the set; on a directed graph two
// nodes can then both join the MIS while an edge connects them. Pannotia
// MIS assumes undirected inputs.
func symMatrix(m csr.Matrix, numNodes int) csr.Matrix {
	edgeCount := 2 * len(m.ColumnNumbers)
	t := csr.Matrix{
		RowOffsets:    make([]uint32, numNodes+1),
		ColumnNumbers: make([]uint32, edgeCount),
		Values:        make([]float32, edgeCount),
	}
	for src := 0; src < numNodes; src++ {
		t.RowOffsets[src+1] += m.RowOffsets[src+1] - m.RowOffsets[src]
	}
	for _, dst := range m.ColumnNumbers {
		t.RowOffsets[dst+1]++
	}
	for i := 0; i < numNodes; i++ {
		t.RowOffsets[i+1] += t.RowOffsets[i]
	}
	pos := make([]uint32, numNodes)
	copy(pos, t.RowOffsets[:numNodes])
	for src := 0; src < numNodes; src++ {
		for j := m.RowOffsets[src]; j < m.RowOffsets[src+1]; j++ {
			dst := int(m.ColumnNumbers[j])
			p := pos[src]
			t.ColumnNumbers[p] = m.ColumnNumbers[j]
			t.Values[p] = m.Values[j]
			pos[src]++
			p = pos[dst]
			t.ColumnNumbers[p] = uint32(src)
			t.Values[p] = m.Values[j]
			pos[dst]++
		}
	}
	return t
}

func (b *Benchmark) initMem() {
	b.matrix = csr.
		MakeMatrixGenerator(uint32(b.NumNodes), uint32(b.NumEdges)).
		GenerateMatrix()
	// sbin_gmmu_omo: original: (no symmetrization)
	// sbin_gmmu_omo: MIS requires an undirected graph; the generated CSR is
	// directed, which produces an invalid result (adjacent MIS nodes).
	b.matrix = symMatrix(b.matrix, b.NumNodes)
	numEdges := len(b.matrix.ColumnNumbers)

	if b.useManagedMemory { // sbin_claude
		// sbin_gmmu_omo: original: b.row_d = ... uint64((b.NumNodes)*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; the kernels
		// read Row[node+1] for the last node, so allocate N+1 slots.
		b.row_d = b.driver.AllocateManaged(b.context,
			uint64((b.NumNodes+1)*4))
		b.col_d = b.driver.AllocateManaged(b.context,
			uint64(numEdges*4))
		b.stop_d = b.driver.AllocateManaged(b.context,
			uint64(4))
		b.min_array_d = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
		b.c_array_d = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
		b.c_array_u_d = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
		b.s_array_d = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
		b.node_value_d = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
	} else if b.useUnifiedMemory {
		// sbin_gmmu_omo: original: b.row_d = ... uint64((b.NumNodes)*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; the kernels
		// read Row[node+1] for the last node, so allocate N+1 slots.
		b.row_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64((b.NumNodes+1)*4))
		b.col_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(numEdges*4))
		b.stop_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(4))
		b.min_array_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
		b.c_array_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
		b.c_array_u_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
		b.s_array_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
		b.node_value_d = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
	} else {
		// sbin_gmmu_omo: original: b.row_d = ... uint64((b.NumNodes)*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; allocate N+1
		b.row_d = b.driver.AllocateMemory(b.context,
			uint64((b.NumNodes+1)*4))
		b.col_d = b.driver.AllocateMemory(b.context,
			uint64(numEdges*4))
		b.stop_d = b.driver.AllocateMemory(b.context,
			uint64(4))
		b.min_array_d = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
		b.c_array_d = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
		b.c_array_u_d = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
		b.s_array_d = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
		b.node_value_d = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
	}

	// sbin_gmmu_omo: original: float64(uint64(b.NumEdges*4)+...)
	// sbin_gmmu_omo: the symmetrized graph has 2x edges; print the real size
	fmt.Printf("Footprint: %.3f MB\n",
		float64(uint64(numEdges*4)+uint64(b.NumNodes*4)*6)/1024.0/1024.0)

	b.stop = make([]int32, 1)
	b.node_value = make([]float32, b.NumNodes)
	for i := 0; i < b.NumNodes; i++ {
		b.node_value[i] = float32(i)
	}
	// TODO: Set the node value as rand

}

func (b *Benchmark) exec() {
	// sbin_gmmu_omo: the symmetrized graph's true edge count (2x the
	// generated connections) for the kernel arguments
	numEdges := len(b.matrix.ColumnNumbers)
	b.driver.MemCopyH2D(b.context, b.row_d, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.col_d, b.matrix.ColumnNumbers)
	b.driver.MemCopyH2D(b.context, b.node_value_d, b.node_value)

	// sbin_wafer: CPU-side init (replace init kernel)
	sArray := make([]int32, b.NumNodes)
	cArray := make([]int32, b.NumNodes)
	cArrayU := make([]int32, b.NumNodes)
	for i := 0; i < b.NumNodes; i++ {
		cArray[i] = -1
		cArrayU[i] = -1
	}
	// sbin_wafer: sArray defaults to 0
	b.driver.MemCopyH2D(b.context, b.s_array_d, sArray)
	b.driver.MemCopyH2D(b.context, b.c_array_d, cArray)
	b.driver.MemCopyH2D(b.context, b.c_array_u_d, cArrayU)

	blockSize := int32(128) // BLOCK_SIZE

	// args := Kernel1Args{
	// 	SArrayD:             b.s_array_d,
	// 	CArrayD:             b.c_array_d,
	// 	CArrayUD:            b.c_array_u_d,
	// 	NumNodes:            int32(b.NumNodes),
	// 	NumEdges:            int32(numEdges),
	// 	HiddenGlobalOffsetX: 0,
	// 	HiddenGlobalOffsetY: 0,
	// 	HiddenGlobalOffsetZ: 0,
	// }

	globalSize := [3]uint32{uint32(b.NumNodes), 1, 1}
	localSize := [3]uint16{uint16(blockSize), 1, 1}

	// b.driver.LaunchKernel(b.context,
	// 	b.kernel1,
	// 	globalSize, localSize,
	// 	&args,
	// )

	// sbin_gmmu_omo: original: b.stop[0] = 1
	// sbin_gmmu_omo: original: for b.stop[0] != 0 {
	// sbin_gmmu_omo: original: 	b.stop[0] = 0
	//
	// sbin_gmmu_omo: iterate until the GPU reports convergence. The old
	// host code zeroed b.stop after every read-back, so the loop always
	// ran exactly once and never measured the real iterative MIS
	// convergence. stop_d must also be reset before the kernels each
	// iteration: mis1 writes 1 to stop_d while any node still has
	// c_array == -1, and without the reset stop_d would stay 1 forever.
	for {
		b.stop[0] = 0
		b.driver.MemCopyH2D(b.context, b.stop_d, b.stop)
		args2 := Kernel2Args{
			RowD:                b.row_d,
			ColD:                b.col_d,
			NodeValueD:          b.node_value_d,
			SArrayD:             b.s_array_d,
			CArrayD:             b.c_array_d,
			MinArrayD:           b.min_array_d,
			StopD:               b.stop_d,
			NumNodes:            int32(b.NumNodes),
			NumEdges:            int32(numEdges),
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		args3 := Kernel3Args{
			RowD:                b.row_d,
			ColD:                b.col_d,
			NodeValueD:          b.node_value_d,
			SArrayD:             b.s_array_d,
			CArrayD:             b.c_array_d,
			CArrayUD:            b.c_array_u_d,
			MinArrayD:           b.min_array_d,
			NumNodes:            int32(b.NumNodes),
			NumEdges:            int32(numEdges),
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		args4 := Kernel4Args{
			CArrayUD:            b.c_array_u_d,
			CArrayD:             b.c_array_d,
			NumNodes:            int32(b.NumNodes),
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel2,
			globalSize, localSize,
			&args2,
		)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel3,
			globalSize, localSize,
			&args3,
		)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel4,
			globalSize, localSize,
			&args4,
		)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, b.stop, b.stop_d)

		// sbin_gmmu_omo: original: b.stop[0] = 0
		if b.stop[0] == 0 {
			break
		}
	}

}

func (b *Benchmark) Verify() {
	return
}
