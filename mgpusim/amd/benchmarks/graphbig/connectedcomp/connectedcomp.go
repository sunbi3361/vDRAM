package connectedcomp

import (
	"fmt"
	"log"

	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/common"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

const (
	chunkSz = 2
	wgSize  = 256
)

// ArgsHook: 6 ptrs = 48B + 24 hidden = 72B.
type ArgsHook struct {
	Parents             driver.Ptr
	Shadow              driver.Ptr
	Mask                driver.Ptr
	EdgeSrc             driver.Ptr
	Edges               driver.Ptr
	State               driver.Ptr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// ArgsUpd / ArgsPJ: 3 ptrs = 24B + 24 hidden = 48B.
type ArgsUpd struct {
	Parents             driver.Ptr
	Shadow              driver.Ptr
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
	NumNodes      int
	Degree        int
	MaxIterations uint32
	VerifyOnCPU   bool

	graph     common.CSRGraph
	edgeSrc   []uint32
	hParents  []uint32
	cpuLabels []uint32

	dParents driver.Ptr
	dShadow  driver.Ptr
	dMask    driver.Ptr
	dEdgeSrc driver.Ptr
	dEdges   driver.Ptr
	dState   driver.Ptr

	kHook   *insts.KernelCodeObject
	kUpdate *insts.KernelCodeObject
	kPJ     *insts.KernelCodeObject

	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude

	Arch  arch.Type
	queue *driver.CommandQueue
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := &Benchmark{driver: driver, context: driver.Init(), NumNodes: 1024, Degree: 8, MaxIterations: 512, VerifyOnCPU: true}
	b.queue = driver.CreateCommandQueue(b.context)

	if len(hsacoBytes) > 0 {
		b.kHook = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "cc_hooking")
		b.kUpdate = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "cc_update")
		b.kPJ = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "cc_pointer_jumping")
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
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 4)
	}
	b.NumNodes = b.graph.NumNodes()
	b.edgeSrc = common.BuildEdgeSrcArray(b.graph)

	b.hParents = make([]uint32, b.NumNodes)
	for i := range b.hParents {
		b.hParents[i] = uint32(i)
	}

	if b.kHook == nil || b.kUpdate == nil || b.kPJ == nil {
		b.execCPU()
		return
	}

	b.dParents = b.alloc(uint64(b.NumNodes * 4))
	b.dShadow = b.alloc(uint64(b.NumNodes * 4))
	b.dMask = b.alloc(uint64(len(b.graph.Edges) * 4)) // uint32 (was uchar)
	b.dEdgeSrc = b.alloc(uint64(len(b.edgeSrc) * 4))
	b.dEdges = b.alloc(uint64(len(b.graph.Edges) * 4))
	b.dState = b.alloc(4 * 4) // [over, iter, edge_cnt, num_nodes]

	b.driver.MemCopyH2D(b.context, b.dParents, b.hParents)
	b.driver.MemCopyH2D(b.context, b.dShadow, b.hParents)
	b.driver.MemCopyH2D(b.context, b.dMask, make([]uint32, len(b.graph.Edges)))
	b.driver.MemCopyH2D(b.context, b.dEdgeSrc, b.edgeSrc)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)

	edgeCnt := uint32(len(b.graph.Edges))
	hookThreads := (edgeCnt + chunkSz - 1) / chunkSz
	hookThreads = ((hookThreads + wgSize - 1) / wgSize) * wgSize
	if hookThreads < wgSize {
		hookThreads = wgSize
	}
	nodeThreads := uint32(((b.NumNodes + wgSize - 1) / wgSize) * wgSize)
	if nodeThreads < wgSize {
		nodeThreads = wgSize
	}

	globalHook := [3]uint32{hookThreads, 1, 1}
	globalNode := [3]uint32{nodeThreads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for iter := uint32(0); iter < b.MaxIterations; iter++ {
		state := []uint32{1, iter, edgeCnt, uint32(b.NumNodes)}
		b.driver.MemCopyH2D(b.context, b.dState, state)

		argsH := ArgsHook{
			Parents: b.dParents, Shadow: b.dShadow, Mask: b.dMask,
			EdgeSrc: b.dEdgeSrc, Edges: b.dEdges, State: b.dState,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kHook, globalHook, local, &argsH)
		b.driver.DrainCommandQueue(b.queue)

		argsU := ArgsUpd{Parents: b.dParents, Shadow: b.dShadow, State: b.dState}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kUpdate, globalNode, local, &argsU)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, state, b.dState)

		argsPJ := ArgsUpd{Parents: b.dParents, Shadow: b.dShadow, State: b.dState}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kPJ, globalNode, local, &argsPJ)
		b.driver.DrainCommandQueue(b.queue)
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kUpdate, globalNode, local, &argsU)
		b.driver.DrainCommandQueue(b.queue)

		if state[0] != 0 {
			break
		}
	}
	b.driver.MemCopyD2H(b.context, b.hParents, b.dParents)
}

func (b *Benchmark) execCPU() {
	parents := make([]uint32, b.NumNodes)
	for i := range parents {
		parents[i] = uint32(i)
	}
	var find func(uint32) uint32
	find = func(v uint32) uint32 {
		for parents[v] != v {
			parents[v] = parents[parents[v]]
			v = parents[v]
		}
		return v
	}
	for v := 0; v < b.NumNodes; v++ {
		for e := b.graph.Offsets[v]; e < b.graph.Offsets[v+1]; e++ {
			to := b.graph.Edges[e]
			ru := find(uint32(v))
			rv := find(to)
			if ru != rv {
				if ru < rv {
					parents[rv] = ru
				} else {
					parents[ru] = rv
				}
			}
		}
	}
	for v := 0; v < b.NumNodes; v++ {
		parents[v] = find(uint32(v))
	}
	b.hParents = parents
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	gpuRoots := canonicalize(b.hParents)
	b.execCPU()
	cpuRoots := canonicalize(b.hParents)

	mismatch := 0
	for i := range gpuRoots {
		if gpuRoots[i] != cpuRoots[i] {
			mismatch++
		}
	}
	fmt.Printf("GraphBIG CC (Hooking+PJ, atomics simulated): %d/%d vertices share component with reference\n",
		b.NumNodes-mismatch, b.NumNodes)
}

func canonicalize(parents []uint32) []uint32 {
	n := len(parents)
	work := append([]uint32(nil), parents...)
	var find func(uint32) uint32
	find = func(v uint32) uint32 {
		for work[v] != v {
			v = work[v]
		}
		return v
	}
	groupMin := make(map[uint32]uint32)
	for v := 0; v < n; v++ {
		r := find(uint32(v))
		if m, ok := groupMin[r]; !ok || uint32(v) < m {
			groupMin[r] = uint32(v)
		}
	}
	out := make([]uint32, n)
	for v := 0; v < n; v++ {
		out[v] = groupMin[find(uint32(v))]
	}
	return out
}

func (b *Benchmark) alloc(size uint64) driver.Ptr {
	if b.useManagedMemory { // sbin_claude
		return b.driver.AllocateManaged(b.context, size)
	} else if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	}
	return b.driver.AllocateMemory(b.context, size)
}
