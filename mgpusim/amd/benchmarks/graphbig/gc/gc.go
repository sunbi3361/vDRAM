package gc

import (
	"fmt"
	"log"
	"math"
	"math/rand"

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

// ArgsGC: 6 ptrs = 48B user + 24B hidden = 72B.
type ArgsGC struct {
	Vplist              driver.Ptr
	Randlist            driver.Ptr
	Offsets             driver.Ptr
	Edges               driver.Ptr
	State               driver.Ptr
	Dummy               driver.Ptr // padding to push kargs to spmv-proven 72B
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int

	DatasetPath   string
	NumNodes      int
	Degree        int
	RandomSeed    int64
	MaxIterations uint32
	VerifyOnCPU   bool

	kernel *insts.KernelCodeObject

	graph    common.CSRGraph
	hVplist  []uint32
	hRand    []uint32
	dOffsets driver.Ptr
	dEdges   driver.Ptr
	dVplist  driver.Ptr
	dRand    driver.Ptr
	dState   driver.Ptr
	dDummy   driver.Ptr

	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude

	Arch  arch.Type
	queue *driver.CommandQueue
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := &Benchmark{
		driver: driver, context: driver.Init(),
		NumNodes: 1024, Degree: 8, RandomSeed: 123, VerifyOnCPU: true,
	}
	b.queue = driver.CreateCommandQueue(b.context)
	if len(hsacoBytes) > 0 {
		b.kernel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "gc_data_warp_centric_kernel")
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
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, b.RandomSeed)
	}
	b.NumNodes = b.graph.NumNodes()
	b.hVplist = make([]uint32, b.NumNodes)
	for i := range b.hVplist {
		b.hVplist[i] = infinity
	}
	rng := rand.New(rand.NewSource(b.RandomSeed))
	b.hRand = make([]uint32, b.NumNodes)
	for i := range b.hRand {
		b.hRand[i] = rng.Uint32()
	}
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
	b.dRand = b.alloc(uint64(n * 4))
	b.dState = b.alloc(4 * 3)
	b.dDummy = b.alloc(4)

	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)
	b.driver.MemCopyH2D(b.context, b.dVplist, b.hVplist)
	b.driver.MemCopyH2D(b.context, b.dRand, b.hRand)
}

func (b *Benchmark) execGPU() {
	maxIter := b.MaxIterations
	if maxIter == 0 {
		maxIter = uint32(b.NumNodes)
	}

	warpsNeeded := uint32((b.NumNodes + chunkSz - 1) / chunkSz)
	threads := warpsNeeded * warpSz
	threads = ((threads + wgSize - 1) / wgSize) * wgSize
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for color := uint32(0); color < maxIter; color++ {
		state := []uint32{0, color, uint32(b.NumNodes)}
		b.driver.MemCopyH2D(b.context, b.dState, state)

		args := ArgsGC{
			Vplist:   b.dVplist,
			Randlist: b.dRand,
			Offsets:  b.dOffsets,
			Edges:    b.dEdges,
			State:    b.dState,
			Dummy:    b.dDummy,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kernel, global, local, &args)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, state, b.dState)
		if state[0] == 0 {
			break
		}
	}
	b.driver.MemCopyD2H(b.context, b.hVplist, b.dVplist)
}

func (b *Benchmark) execCPU() {
	colors := make([]int32, b.NumNodes)
	for i := range colors {
		colors[i] = -1
	}
	for v := 0; v < b.NumNodes; v++ {
		var used [256]bool
		for e := b.graph.Offsets[v]; e < b.graph.Offsets[v+1]; e++ {
			to := b.graph.Edges[e]
			c := colors[to]
			if c >= 0 && int(c) < len(used) {
				used[c] = true
			}
		}
		c := int32(0)
		for int(c) < len(used) && used[c] {
			c++
		}
		colors[v] = c
	}
	b.hVplist = make([]uint32, b.NumNodes)
	for i, c := range colors {
		b.hVplist[i] = uint32(c)
	}
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	conflicts := 0
	uncolored := 0
	for v := 0; v < b.NumNodes; v++ {
		if b.hVplist[v] == infinity {
			uncolored++
			continue
		}
		for e := b.graph.Offsets[v]; e < b.graph.Offsets[v+1]; e++ {
			to := b.graph.Edges[e]
			if b.hVplist[to] == infinity {
				continue
			}
			if b.hVplist[v] == b.hVplist[to] {
				conflicts++
			}
		}
	}
	fmt.Printf("GraphBIG GC (topo-warp-centric, atomics simulated): %d vertices, %d uncolored, %d conflicts\n",
		b.NumNodes, uncolored, conflicts)
}
