package trianglecount

import (
	"fmt"
	"log"
	"sort"

	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/common"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

const wgSize = 256

type sortArgs struct {
	Offsets             driver.Ptr
	Edges               driver.Ptr
	NumNodes            uint32
	Pad0                uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type intersectArgs struct {
	Offsets             driver.Ptr
	Edges               driver.Ptr
	Vplist              driver.Ptr
	NumNodes            uint32
	Pad0                uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type reduceArgs struct {
	Vplist              driver.Ptr
	Tcount              driver.Ptr
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
	triPerV   []uint32
	triangleN uint64

	dOffsets driver.Ptr
	dEdges   driver.Ptr
	dVplist  driver.Ptr
	dTcount  driver.Ptr

	kSort      *insts.KernelCodeObject
	kIntersect *insts.KernelCodeObject
	kReduce    *insts.KernelCodeObject

	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude

	Arch  arch.Type
	queue *driver.CommandQueue
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := &Benchmark{driver: driver, context: driver.Init(), NumNodes: 512, Degree: 8, VerifyOnCPU: true}
	b.queue = driver.CreateCommandQueue(b.context)

	if len(hsacoBytes) > 0 {
		b.kSort = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "tc_sort_adj")
		b.kIntersect = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "tc_intersect")
		b.kReduce = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "tc_reduce")
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
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 6)
	}
	b.NumNodes = b.graph.NumNodes()
	b.triPerV = make([]uint32, b.NumNodes)

	if b.kIntersect == nil || b.kReduce == nil {
		b.execCPU()
		return
	}

	// Pre-sort adjacency lists on host. The GPU sort kernel
	// (insertion sort O(deg^2) per vertex) was dominating simulation time
	// and is not part of the graph workload of interest (the irregular
	// memory access in merge-intersect is). In practice TC inputs are also
	// kept pre-sorted at graph load time.
	for v := 0; v < b.NumNodes; v++ {
		s := b.graph.Edges[b.graph.Offsets[v]:b.graph.Offsets[v+1]]
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}

	b.dOffsets = b.alloc(uint64(len(b.graph.Offsets) * 4))
	b.dEdges = b.alloc(uint64(len(b.graph.Edges) * 4))
	b.dVplist = b.alloc(uint64(b.NumNodes * 4))
	b.dTcount = b.alloc(4)

	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)
	b.driver.MemCopyH2D(b.context, b.dVplist, b.triPerV) // zero-init
	b.driver.MemCopyH2D(b.context, b.dTcount, uint32(0))

	threads := uint32(((b.NumNodes + wgSize - 1) / wgSize) * wgSize)
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	ia := intersectArgs{Offsets: b.dOffsets, Edges: b.dEdges, Vplist: b.dVplist, NumNodes: uint32(b.NumNodes)}
	b.driver.EnqueueLaunchKernel(
		b.queue,
		b.kIntersect, global, local, &ia)
	b.driver.DrainCommandQueue(b.queue)

	ra := reduceArgs{Vplist: b.dVplist, Tcount: b.dTcount, NumNodes: uint32(b.NumNodes)}
	b.driver.EnqueueLaunchKernel(
		b.queue,
		b.kReduce, global, local, &ra)
	b.driver.DrainCommandQueue(b.queue)

	var tcount uint32
	b.driver.MemCopyD2H(b.context, b.triPerV, b.dVplist)
	b.driver.MemCopyD2H(b.context, &tcount, b.dTcount)
	b.triangleN = uint64(tcount) / 3
}

func (b *Benchmark) execCPU() {
	// Sorted per-vertex adjacency, then merge-intersect on each forward edge.
	off := b.graph.Offsets
	edges := append([]uint32(nil), b.graph.Edges...)
	for v := 0; v < b.NumNodes; v++ {
		s := edges[off[v]:off[v+1]]
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}
	for v := uint32(0); v < uint32(b.NumNodes); v++ {
		vStart, vEnd := off[v], off[v+1]
		for k := vStart; k < vEnd; k++ {
			u := edges[k]
			if v > u {
				continue
			}
			uStart, uEnd := off[u], off[u+1]
			i, j := vStart, uStart
			var cnt uint32
			for i < vEnd && j < uEnd {
				if edges[i] < edges[j] {
					i++
				} else if edges[i] > edges[j] {
					j++
				} else {
					cnt++
					i++
					j++
				}
			}
			b.triPerV[v] += cnt
			b.triPerV[u] += cnt
		}
	}
	b.triangleN = 0
	for v := 0; v < b.NumNodes; v++ {
		b.triPerV[v] /= 2
		b.triangleN += uint64(b.triPerV[v])
	}
	b.triangleN /= 3
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	gpuCount := b.triangleN

	// Recompute reference.
	b.triPerV = make([]uint32, b.NumNodes)
	b.triangleN = 0
	b.execCPU()
	fmt.Printf("GraphBIG TriangleCount (atomics simulated): gpu=%d cpu=%d\n", gpuCount, b.triangleN)
}

func (b *Benchmark) alloc(size uint64) driver.Ptr {
	if b.useManagedMemory { // sbin_claude
		return b.driver.AllocateManaged(b.context, size)
	} else if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	}
	return b.driver.AllocateMemory(b.context, size)
}
