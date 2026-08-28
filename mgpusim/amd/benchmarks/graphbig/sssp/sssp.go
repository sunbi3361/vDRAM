package sssp

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

// ArgsRelax: 6 ptrs = 48B user + 24B hidden = 72B (matches working spmv).
type ArgsRelax struct {
	Vplist              driver.Ptr
	Eplist              driver.Ptr
	Update              driver.Ptr
	Offsets             driver.Ptr
	Edges               driver.Ptr
	State               driver.Ptr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// ArgsCommit: 3 ptrs = 24B user + 24B hidden = 48B.
type ArgsCommit struct {
	Vplist              driver.Ptr
	Update              driver.Ptr
	State               driver.Ptr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int

	DatasetPath   string
	Root          uint64
	NumNodes      int
	Degree        int
	MaxIterations uint32
	VerifyOnCPU   bool

	kRelax  *insts.KernelCodeObject
	kCommit *insts.KernelCodeObject

	graph   common.CSRGraph
	weights []uint32
	hVplist []uint32
	cpuDist []uint32

	dOffsets driver.Ptr
	dEdges   driver.Ptr
	dEplist  driver.Ptr
	dVplist  driver.Ptr
	dUpdate  driver.Ptr
	dState   driver.Ptr

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
		b.kRelax = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "sssp_data_warp_centric_relax")
		b.kCommit = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "sssp_data_warp_centric_commit")
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
	if b.kRelax == nil || b.kCommit == nil {
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
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 2)
	}
	b.NumNodes = b.graph.NumNodes()
	if int(b.Root) >= b.NumNodes {
		b.Root = 0
	}
	b.weights = common.UnitWeights(len(b.graph.Edges))
	b.hVplist = make([]uint32, b.NumNodes)
	for i := range b.hVplist {
		b.hVplist[i] = infinity
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
	b.dEplist = b.alloc(uint64(len(b.weights) * 4))
	b.dVplist = b.alloc(uint64(n * 4))
	b.dUpdate = b.alloc(uint64(n * 4))
	b.dState = b.alloc(4 * 2)

	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)
	b.driver.MemCopyH2D(b.context, b.dEplist, b.weights)
	b.driver.MemCopyH2D(b.context, b.dVplist, b.hVplist)
	b.driver.MemCopyH2D(b.context, b.dUpdate, b.hVplist)
}

func (b *Benchmark) execGPU() {
	maxIter := b.MaxIterations
	if maxIter == 0 {
		maxIter = uint32(b.NumNodes)
	}

	warpsNeeded := uint32((b.NumNodes + chunkSz - 1) / chunkSz)
	relaxThreads := warpsNeeded * warpSz
	relaxThreads = ((relaxThreads + wgSize - 1) / wgSize) * wgSize
	if relaxThreads < wgSize {
		relaxThreads = wgSize
	}
	commitThreads := uint32(((b.NumNodes + wgSize - 1) / wgSize) * wgSize)
	if commitThreads < wgSize {
		commitThreads = wgSize
	}
	gRelax := [3]uint32{relaxThreads, 1, 1}
	gCommit := [3]uint32{commitThreads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for iter := uint32(0); iter < maxIter; iter++ {
		state := []uint32{0, uint32(b.NumNodes)}
		b.driver.MemCopyH2D(b.context, b.dState, state)

		argsR := ArgsRelax{
			Vplist:  b.dVplist,
			Eplist:  b.dEplist,
			Update:  b.dUpdate,
			Offsets: b.dOffsets,
			Edges:   b.dEdges,
			State:   b.dState,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kRelax, gRelax, local, &argsR)
		b.driver.DrainCommandQueue(b.queue)

		argsC := ArgsCommit{
			Vplist: b.dVplist,
			Update: b.dUpdate,
			State:  b.dState,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kCommit, gCommit, local, &argsC)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, state, b.dState)
		if state[0] == 0 {
			break
		}
	}
	b.driver.MemCopyD2H(b.context, b.hVplist, b.dVplist)
}

func (b *Benchmark) execCPU() {
	dist := make([]uint32, len(b.hVplist))
	copy(dist, b.hVplist)
	maxIter := b.MaxIterations
	if maxIter == 0 {
		maxIter = uint32(b.NumNodes)
	}
	for iter := uint32(0); iter < maxIter; iter++ {
		changed := false
		next := append([]uint32(nil), dist...)
		for v := 0; v < b.NumNodes; v++ {
			if dist[v] == infinity {
				continue
			}
			for e := b.graph.Offsets[v]; e < b.graph.Offsets[v+1]; e++ {
				to := b.graph.Edges[e]
				cand := dist[v] + b.weights[e]
				if cand < next[to] {
					next[to] = cand
					changed = true
				}
			}
		}
		dist = next
		if !changed {
			break
		}
	}
	copy(b.hVplist, dist)
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	gpu := append([]uint32(nil), b.hVplist...)
	b.execCPU()
	b.cpuDist = append([]uint32(nil), b.hVplist...)
	mismatch := 0
	for i := range gpu {
		if gpu[i] != b.cpuDist[i] {
			mismatch++
		}
	}
	fmt.Printf("GraphBIG SSSP (topo-warp-centric, atomics simulated): %d/%d match reference\n",
		b.NumNodes-mismatch, b.NumNodes)
}
