package sssp

import (
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
	VectorD1            driver.Ptr
	VectorD2            driver.Ptr
	SourceVertex        int32
	NumNodes            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel2Args list first set of kernel arguments
// sbin_gmmu_omo: original field order had Padding before NumNodes, but the
// kernel reads NumNodes from kernarg offset 0 (s_load_dword s3, s[6:7],
// 0x0). With the old order the kernel received NumNodes=0 (it read Padding),
// so the boundary check 0 > global_id always failed, the wavefronts exited
// immediately, and the relaxation never ran.
type Kernel2Args struct {
	NumNodes            int32
	Padding             int32
	RowD                driver.Ptr
	ColD                driver.Ptr
	DataD               driver.Ptr
	VectorD1            driver.Ptr
	VectorD2            driver.Ptr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel3Args list first set of kernel arguments
type Kernel3Args struct {
	VectorD1            driver.Ptr
	VectorD2            driver.Ptr
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Kernel4Args list first set of kernel arguments
type Kernel4Args struct {
	VectorD1            driver.Ptr
	VectorD2            driver.Ptr
	StopD               driver.Ptr
	NumNodes            int32
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
	kernel3 *insts.KernelCodeObject
	kernel4 *insts.KernelCodeObject

	row, col, data           []float32
	stop                     []int32
	vector1, vector2         []float32
	NumNodes, NumItems       int
	rowd, cold, datad, stopd driver.Ptr
	vectord1, vectord2       driver.Ptr
	matrix                   csr.Matrix

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
		hsacoBytes, "vector_init")
	if b.kernel1 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel2 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "spmv_min_dot_plus_kernel")
	if b.kernel2 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel3 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "vector_assign")
	if b.kernel3 == nil {
		log.Panic("Failed to load kernel binary")
	}
	b.kernel4 = insts.LoadKernelCodeObjectFromBytes(
		hsacoBytes, "vector_diff")
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

// sbin_gmmu_omo: transposeCSR builds the transpose of a CSR matrix so that
// each row lists a node's incoming edges. The relax kernel pulls
// dist[node] = min over row entries of dist[col[j]] + w, which only
// propagates source-rooted distances with incoming-edge rows.
func transposeCSR(m csr.Matrix, numNodes int) csr.Matrix {
	t := csr.Matrix{
		RowOffsets:    make([]uint32, numNodes+1),
		ColumnNumbers: make([]uint32, len(m.ColumnNumbers)),
		Values:        make([]float32, len(m.Values)),
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
			dst := m.ColumnNumbers[j]
			p := pos[dst]
			t.ColumnNumbers[p] = uint32(src)
			t.Values[p] = m.Values[j]
			pos[dst]++
		}
	}
	return t
}

func (b *Benchmark) initMem() {
	b.matrix = csr.
		MakeMatrixGenerator(uint32(b.NumNodes), uint32(b.NumItems)).
		GenerateMatrix()

	if b.useManagedMemory { // sbin_claude
		b.rowd = b.driver.AllocateManaged(b.context,
			uint64((b.NumNodes+1)*4))
		b.cold = b.driver.AllocateManaged(b.context,
			uint64(b.NumItems*4))
		b.datad = b.driver.AllocateManaged(b.context,
			uint64(b.NumItems*4))
		b.stopd = b.driver.AllocateManaged(b.context,
			uint64(4))
		b.vectord1 = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
		b.vectord2 = b.driver.AllocateManaged(b.context,
			uint64(b.NumNodes*4))
	} else if b.useUnifiedMemory {
		b.rowd = b.driver.AllocateUnifiedMemory(b.context,
			uint64((b.NumNodes+1)*4))
		b.cold = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumItems*4))
		b.datad = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumItems*4))
		b.stopd = b.driver.AllocateUnifiedMemory(b.context,
			uint64(4))
		b.vectord1 = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
		b.vectord2 = b.driver.AllocateUnifiedMemory(b.context,
			uint64(b.NumNodes*4))
	} else {
		// sbin_gmmu_omo: original: b.rowd = ... uint64(b.NumNodes*4)
		// sbin_gmmu_omo: CSR RowOffsets has NumNodes+1 entries; allocate N+1
		b.rowd = b.driver.AllocateMemory(b.context,
			uint64((b.NumNodes+1)*4))
		b.cold = b.driver.AllocateMemory(b.context,
			uint64(b.NumItems*4))
		b.datad = b.driver.AllocateMemory(b.context,
			uint64(b.NumItems*4))
		b.stopd = b.driver.AllocateMemory(b.context,
			uint64(4))
		b.vectord1 = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
		b.vectord2 = b.driver.AllocateMemory(b.context,
			uint64(b.NumNodes*4))
	}

	b.stop = make([]int32, 1)
}

func (b *Benchmark) exec() {
	// sbin_gmmu_omo: the spmv_min_dot_plus kernel pulls over each node's
	// row (dist[node] = min(dist[col[j]] + w)), which only propagates
	// source-rooted distances when the row lists incoming edges. The old
	// code uploaded the outgoing-edge CSR, so no distance ever changed and
	// the convergence loop always exited after one iteration.
	matrix := transposeCSR(b.matrix, b.NumNodes)
	// sbin_gmmu_omo: original: (weights were uploaded as float32 values)
	// sbin_gmmu_omo: the kernel relaxes with integer add/min, so the float
	// edge values (whose bit patterns are ~1e9 as int32) were always larger
	// than the BIG_NUM sentinel and no relaxation ever succeeded. Convert
	// them to small integer weights in [1, 1000].
	weights := make([]int32, len(matrix.Values))
	for i, v := range matrix.Values {
		weights[i] = int32(v*999) + 1
	}
	b.driver.MemCopyH2D(b.context, b.rowd, matrix.RowOffsets)    // sbin_claude
	b.driver.MemCopyH2D(b.context, b.cold, matrix.ColumnNumbers) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.datad, weights)             // sbin_claude

	// sbin_wafer: CPU-side init (replace vector_init kernel)
	const BIG_NUM = 99999999
	vector1 := make([]int32, b.NumNodes)
	vector2 := make([]int32, b.NumNodes)
	sourceVertex := 0
	for i := 0; i < b.NumNodes; i++ {
		if i == sourceVertex {
			vector1[i] = 0
			vector2[i] = 0
		} else {
			vector1[i] = BIG_NUM
			vector2[i] = BIG_NUM
		}
	}
	b.driver.MemCopyH2D(b.context, b.vectord1, vector1) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.vectord2, vector2) // sbin_claude

	blockSize := int32(256) // BLOCK_SIZE

	// args := Kernel1Args{
	// 	VectorD1:            b.vectord1,
	// 	VectorD2:            b.vectord2,
	// 	SourceVertex:        0,
	// 	NumNodes:            int32(b.NumNodes),
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

	for i := 0; i < b.NumNodes; i++ {
		// sbin_gmmu_omo: original: for i := 0; i < 1; i++ {
		args2 := Kernel2Args{
			NumNodes:            int32(b.NumNodes),
			Padding:             0,
			RowD:                b.rowd,
			ColD:                b.cold,
			DataD:               b.datad,
			VectorD1:            b.vectord1,
			VectorD2:            b.vectord2,
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		args3 := Kernel3Args{
			VectorD1:            b.vectord1,
			VectorD2:            b.vectord2,
			NumNodes:            int32(b.NumNodes),
			Padding:             0,
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		args4 := Kernel4Args{
			VectorD1:            b.vectord1,
			VectorD2:            b.vectord2,
			StopD:               b.stopd,
			NumNodes:            int32(b.NumNodes),
			Padding:             0,
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		b.stop[0] = 0
		b.driver.MemCopyH2D(b.context, b.stopd, b.stop)

		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel3,
			globalSize, localSize,
			&args3,
		)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel2,
			globalSize, localSize,
			&args2,
		)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.EnqueueLaunchKernel(b.queue,
			b.kernel4,
			globalSize, localSize,
			&args4,
		)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, b.stop, b.stopd)
		if b.stop[0] == 0 {
			break
		}
	}

}

func (b *Benchmark) Verify() {
	return
}
