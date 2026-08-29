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

// Model selects the traversal kernel. // sbin_claude
const (
	// ModelFrontier is the GraphBIG topology-driven frontier model
	// (gpu_BFS/bfs_topo_frontier.cu, Harish HiPC 2007): one vertex per
	// thread, two kernels per level, no atomics.
	ModelFrontier = "frontier"
	// ModelWarpCentric is the previous kernel, kept so earlier results stay
	// reproducible. One warp cooperatively walks one adjacency list, which
	// coalesces every edge read and leaves the page-walk unit idle.
	ModelWarpCentric = "warp-centric"
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

// ArgsBFSExpand matches bfs_frontier_expand_kernel. // sbin_claude
type ArgsBFSExpand struct {
	Vplist              driver.Ptr
	Offsets             driver.Ptr
	Edges               driver.Ptr
	Frontier            driver.Ptr
	Updating            driver.Ptr
	State               driver.Ptr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// ArgsBFSCompact matches bfs_frontier_compact_kernel. // sbin_claude
type ArgsBFSCompact struct {
	Frontier            driver.Ptr
	Updating            driver.Ptr
	State               driver.Ptr
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

	// Model picks the traversal kernel; defaults to ModelFrontier. // sbin_claude
	Model string

	kernel        *insts.KernelCodeObject
	kernelExpand  *insts.KernelCodeObject // sbin_claude
	kernelCompact *insts.KernelCodeObject // sbin_claude

	graph     common.CSRGraph
	hVplist   []uint32
	hFrontier []uint32 // sbin_claude
	cpuVplist []uint32

	dOffsets  driver.Ptr
	dEdges    driver.Ptr
	dVplist   driver.Ptr
	dState    driver.Ptr
	dFrontier driver.Ptr // sbin_claude
	dUpdating driver.Ptr // sbin_claude

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
		Model: ModelFrontier, // sbin_claude
	}
	b.queue = driver.CreateCommandQueue(b.context)
	if len(hsacoBytes) > 0 {
		b.kernel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "bfs_data_warp_centric_kernel")
		// sbin_claude: both models live in the same hsaco.
		b.kernelExpand = insts.LoadKernelCodeObjectFromBytes(
			hsacoBytes, "bfs_frontier_expand_kernel")
		b.kernelCompact = insts.LoadKernelCodeObjectFromBytes(
			hsacoBytes, "bfs_frontier_compact_kernel")
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

	// sbin_claude: the frontier model starts with the root on the frontier.
	b.hFrontier = make([]uint32, b.NumNodes)
	b.hFrontier[b.Root] = 1
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

	// sbin_claude: frontier[] and updating[] are one uint per vertex (see the
	// deviation note in native/bfs_topo_frontier.cl). The expand kernel probes
	// updating[dst] at scattered dst, which is a large part of the page-walk
	// pressure this model is meant to create.
	if b.Model != ModelWarpCentric {
		b.dFrontier = b.alloc(uint64(n * 4))
		b.dUpdating = b.alloc(uint64(n * 4))
		b.driver.MemCopyH2D(b.context, b.dFrontier, b.hFrontier)
		b.driver.MemCopyH2D(b.context, b.dUpdating, make([]uint32, n))
	}
}

// sbin_claude: dispatch on the selected traversal model.
func (b *Benchmark) execGPU() {
	if b.Model == ModelWarpCentric {
		b.execGPUWarpCentric()
		return
	}
	b.execGPUFrontier()
}

// execGPUFrontier runs the GraphBIG topology-driven frontier model
// (gpu_BFS/bfs_topo_frontier.cu). Each level is two launches: expand walks the
// adjacency lists of the current frontier one vertex per thread, and compact
// swaps updating[] into frontier[] and reports whether anything moved.
// No atomics: every writer of a given vertex at a given level writes the same
// value, so the races are benign. // sbin_claude
func (b *Benchmark) execGPUFrontier() {
	maxDepth := b.MaxDepth
	if maxDepth == 0 {
		maxDepth = uint32(b.NumNodes)
	}

	threads := uint32(b.NumNodes)
	threads = ((threads + wgSize - 1) / wgSize) * wgSize
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for level := uint32(0); level < maxDepth; level++ {
		state := []uint32{0, level, uint32(b.NumNodes)}
		b.driver.MemCopyH2D(b.context, b.dState, state)

		expandArgs := ArgsBFSExpand{
			Vplist:   b.dVplist,
			Offsets:  b.dOffsets,
			Edges:    b.dEdges,
			Frontier: b.dFrontier,
			Updating: b.dUpdating,
			State:    b.dState,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue, b.kernelExpand, global, local, &expandArgs)
		b.driver.DrainCommandQueue(b.queue)

		compactArgs := ArgsBFSCompact{
			Frontier: b.dFrontier,
			Updating: b.dUpdating,
			State:    b.dState,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue, b.kernelCompact, global, local, &compactArgs)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, state, b.dState)
		if state[0] == 0 {
			break
		}
	}
	b.driver.MemCopyD2H(b.context, b.hVplist, b.dVplist)
}

// Pre-edit code (commented per AGENTS.md convention): execGPU used to be the
// warp-centric driver below, with no model dispatch. // sbin_claude
func (b *Benchmark) execGPUWarpCentric() {
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
	// sbin_claude: report which traversal model produced the result.
	fmt.Printf("GraphBIG BFS (%s, no atomics): %d/%d match reference\n",
		b.Model, b.NumNodes-mismatch, b.NumNodes)
}
