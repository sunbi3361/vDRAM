package bfs

import (
	"fmt"
	"log"
	"math"

	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/common"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

const (
	infinity = math.MaxUint32
	warpSz   = 64
	chunkSz  = 64
	wgSize   = 256
)

// ArgsBFS keeps to 4 ptrs + scalar pad + hidden = 56B to stay under the
// SMEM dwordx16 threshold. All per-launch scalars live in `state[]`.
type ArgsBFS struct {
	Vplist              driver.Ptr
	Offsets             driver.Ptr
	Edges               driver.Ptr
	State               driver.Ptr
	Pad0                uint32
	Pad1                uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	queue   *driver.CommandQueue
	gpus    []int

	DatasetPath string
	Root        uint64
	NumNodes    int
	Degree      int
	MaxDepth    uint32
	VerifyOnCPU bool

	kernel *insts.KernelCodeObject

	graph     common.CSRGraph
	hVplist   []uint32
	cpuVplist []uint32

	dOffsets driver.Ptr
	dEdges   driver.Ptr
	dVplist  driver.Ptr
	dState   driver.Ptr

	Arch             arch.Type
	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := &Benchmark{
		driver: driver, context: driver.Init(),
		NumNodes: 1024, Degree: 8, VerifyOnCPU: true,
	}
	b.queue = driver.CreateCommandQueue(b.context)
	if len(hsacoBytes) > 0 {
		b.kernel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "bfs_data_warp_centric_kernel")
	}
	return b
}

func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }
func (b *Benchmark) SetUnifiedMemory()    { b.useUnifiedMemory = true }

// SetManagedMemory switches allocations to UVM managed memory. // sbin_claude
func (b *Benchmark) SetManagedMemory() { b.useManagedMemory = true }
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	b.initGraph()
	if b.kernel == nil {
		b.execCPU()
		return
	}
	b.initMem()
	b.execGPU()
}

func (b *Benchmark) initGraph() {
	if b.DatasetPath != "" {
		g, err := common.LoadCSR(b.DatasetPath)
		if err != nil {
			log.Panic(err)
		}
		b.graph = g
	} else {
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 1)
	}
	b.NumNodes = b.graph.NumNodes()
	b.hVplist = make([]uint32, b.NumNodes)
	for i := range b.hVplist {
		b.hVplist[i] = infinity
	}
	if int(b.Root) >= b.NumNodes {
		b.Root = 0
	}
	b.hVplist[b.Root] = 0
}

func (b *Benchmark) alloc(size uint64) driver.Ptr {
	if b.useManagedMemory { // sbin_claude
		return b.driver.AllocateManaged(b.context, size)
	} else if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	}
	return b.driver.AllocateMemory(b.context, size)
}

func (b *Benchmark) initMem() {
	n := b.NumNodes
	b.dOffsets = b.alloc(uint64(len(b.graph.Offsets) * 4))
	b.dEdges = b.alloc(uint64(len(b.graph.Edges) * 4))
	b.dVplist = b.alloc(uint64(n * 4))
	b.dState = b.alloc(4 * 3) // [changed, curr_level, num_nodes]

	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets) // sbin_claude
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)     // sbin_claude
	b.driver.MemCopyH2D(b.context, b.dVplist, b.hVplist)        // sbin_claude
}

func (b *Benchmark) execGPU() {
	maxDepth := b.MaxDepth
	if maxDepth == 0 {
		maxDepth = uint32(b.NumNodes)
	}

	// Total warps cover all vertices in chunks of CHUNK_SZ.
	warpsNeeded := uint32((b.NumNodes + chunkSz - 1) / chunkSz)
	threads := warpsNeeded * warpSz
	threads = ((threads + wgSize - 1) / wgSize) * wgSize
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for level := uint32(0); level < maxDepth; level++ {
		state := []uint32{0, level, uint32(b.NumNodes)}
		b.driver.MemCopyH2D(b.context, b.dState, state) // sbin_claude

		args := ArgsBFS{
			Vplist:  b.dVplist,
			Offsets: b.dOffsets,
			Edges:   b.dEdges,
			State:   b.dState,
		}
		b.driver.EnqueueLaunchKernel(b.queue, b.kernel, global, local, &args)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, state, b.dState)
		if state[0] == 0 {
			break
		}
	}
	b.driver.MemCopyD2H(b.context, b.hVplist, b.dVplist)
}

func (b *Benchmark) execCPU() {
	queue := make([]uint32, 0, b.NumNodes)
	queue = append(queue, uint32(b.Root))
	maxDepth := b.MaxDepth
	if maxDepth == 0 {
		maxDepth = uint32(b.NumNodes)
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if b.hVplist[v] >= maxDepth {
			continue
		}
		for e := b.graph.Offsets[v]; e < b.graph.Offsets[v+1]; e++ {
			to := b.graph.Edges[e]
			if b.hVplist[to] == infinity {
				b.hVplist[to] = b.hVplist[v] + 1
				queue = append(queue, to)
			}
		}
	}
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	gpu := append([]uint32(nil), b.hVplist...)
	b.hVplist = make([]uint32, b.NumNodes)
	for i := range b.hVplist {
		b.hVplist[i] = infinity
	}
	b.hVplist[b.Root] = 0
	b.execCPU()
	b.cpuVplist = append([]uint32(nil), b.hVplist...)
	b.hVplist = gpu

	mismatch := 0
	for i := range gpu {
		if gpu[i] != b.cpuVplist[i] {
			mismatch++
		}
	}
	fmt.Printf("GraphBIG BFS (topo-warp-centric, atomics simulated): %d/%d match reference\n",
		b.NumNodes-mismatch, b.NumNodes)
}
