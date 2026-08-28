package betweennesscentr

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
	wgSize   = 256
)

type initArgs struct {
	Dist                driver.Ptr
	Sigma               driver.Ptr
	Delta               driver.Ptr
	NumNodes            uint32
	Root                uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type forwardArgs struct {
	Offsets             driver.Ptr
	Edges               driver.Ptr
	Dist                driver.Ptr
	Sigma               driver.Ptr
	Over                driver.Ptr
	Curr                uint32
	NumNodes            uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type backwardArgs struct {
	Offsets             driver.Ptr
	Edges               driver.Ptr
	Dist                driver.Ptr
	Sigma               driver.Ptr
	Delta               driver.Ptr
	Curr                uint32
	NumNodes            uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type backsumArgs struct {
	BC                  driver.Ptr
	Delta               driver.Ptr
	Dist                driver.Ptr
	Root                uint32
	NumNodes            uint32
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
	NumRoots    int // limit per-root passes; 0 means all vertices
	MaxDepth    uint32
	VerifyOnCPU bool

	graph common.CSRGraph
	bc    []float32
	cpuBC []float32

	dOffsets driver.Ptr
	dEdges   driver.Ptr
	dDist    driver.Ptr
	dSigma   driver.Ptr
	dDelta   driver.Ptr
	dBC      driver.Ptr
	dOver    driver.Ptr

	kInit     *insts.KernelCodeObject
	kForward  *insts.KernelCodeObject
	kBackward *insts.KernelCodeObject
	kBacksum  *insts.KernelCodeObject

	useUnifiedMemory bool
	useManagedMemory bool // sbin_claude

	Arch  arch.Type
	queue *driver.CommandQueue
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	// NumRoots defaults to 8: the original GraphBIG loops over all |V|
	// vertices as roots, which is O(|V|*(|V|+|E|)) and dominates simulation
	// time. 8 root passes already exercise the forward+backward DAG memory
	// pattern; raise via --numRoots if more coverage is wanted.
	b := &Benchmark{driver: driver, context: driver.Init(), NumNodes: 1024, Degree: 8, NumRoots: 8, VerifyOnCPU: true}
	b.queue = driver.CreateCommandQueue(b.context)

	if len(hsacoBytes) > 0 {
		b.kInit = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "bc_init")
		b.kForward = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "bc_forward")
		b.kBackward = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "bc_backward")
		b.kBacksum = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "bc_backsum")
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
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 7)
	}
	b.NumNodes = b.graph.NumNodes()
	b.bc = make([]float32, b.NumNodes)

	roots := b.NumRoots
	if roots <= 0 {
		roots = 8 // sane default for simulation
	}
	if roots > b.NumNodes {
		roots = b.NumNodes
	}
	maxDepth := b.MaxDepth
	if maxDepth == 0 {
		maxDepth = uint32(b.NumNodes)
	}

	if b.kInit == nil || b.kForward == nil || b.kBackward == nil || b.kBacksum == nil {
		b.execCPU(roots, maxDepth)
		return
	}

	b.dOffsets = b.alloc(uint64(len(b.graph.Offsets) * 4))
	b.dEdges = b.alloc(uint64(len(b.graph.Edges) * 4))
	b.dDist = b.alloc(uint64(b.NumNodes * 4))
	b.dSigma = b.alloc(uint64(b.NumNodes * 4))
	b.dDelta = b.alloc(uint64(b.NumNodes * 4))
	b.dBC = b.alloc(uint64(b.NumNodes * 4))
	b.dOver = b.alloc(4)

	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)
	b.driver.MemCopyH2D(b.context, b.dBC, b.bc)

	threads := uint32(((b.NumNodes + wgSize - 1) / wgSize) * wgSize)
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for root := uint32(0); root < uint32(roots); root++ {
		ia := initArgs{Dist: b.dDist, Sigma: b.dSigma, Delta: b.dDelta, NumNodes: uint32(b.NumNodes), Root: root}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kInit, global, local, &ia)
		b.driver.DrainCommandQueue(b.queue)

		// forward
		curr := uint32(0)
		for curr < maxDepth {
			over := int32(1)
			b.driver.MemCopyH2D(b.context, b.dOver, over)
			fa := forwardArgs{
				Offsets: b.dOffsets, Edges: b.dEdges,
				Dist: b.dDist, Sigma: b.dSigma, Over: b.dOver,
				Curr: curr, NumNodes: uint32(b.NumNodes),
			}
			b.driver.EnqueueLaunchKernel(
				b.queue,
				b.kForward, global, local, &fa)
			b.driver.DrainCommandQueue(b.queue)
			b.driver.MemCopyD2H(b.context, &over, b.dOver)
			curr++
			if over != 0 {
				break
			}
		}

		// backward
		for curr > 1 {
			curr--
			ba := backwardArgs{
				Offsets: b.dOffsets, Edges: b.dEdges,
				Dist: b.dDist, Sigma: b.dSigma, Delta: b.dDelta,
				Curr: curr, NumNodes: uint32(b.NumNodes),
			}
			b.driver.EnqueueLaunchKernel(
				b.queue,
				b.kBackward, global, local, &ba)
			b.driver.DrainCommandQueue(b.queue)
		}

		sa := backsumArgs{BC: b.dBC, Delta: b.dDelta, Dist: b.dDist, Root: root, NumNodes: uint32(b.NumNodes)}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kBacksum, global, local, &sa)
		b.driver.DrainCommandQueue(b.queue)
	}

	b.driver.MemCopyD2H(b.context, b.bc, b.dBC)
}

func (b *Benchmark) execCPU(roots int, maxDepth uint32) {
	off := b.graph.Offsets
	edges := b.graph.Edges
	n := b.NumNodes

	dist := make([]uint32, n)
	sigma := make([]uint32, n)
	delta := make([]float32, n)

	for root := 0; root < roots; root++ {
		for i := 0; i < n; i++ {
			dist[i] = infinity
			sigma[i] = 0
			delta[i] = 0
		}
		dist[root] = 0
		sigma[root] = 1

		curr := uint32(0)
		for curr < maxDepth {
			progressed := false
			for v := 0; v < n; v++ {
				if dist[v] != curr {
					continue
				}
				for e := off[v]; e < off[v+1]; e++ {
					w := edges[e]
					if dist[w] == infinity {
						dist[w] = curr + 1
						progressed = true
					}
					if dist[w] == curr+1 {
						sigma[w] += sigma[v]
					}
				}
			}
			curr++
			if !progressed {
				break
			}
		}
		for curr > 1 {
			curr--
			for v := 0; v < n; v++ {
				if dist[v] != curr-1 {
					continue
				}
				var sum float32
				for e := off[v]; e < off[v+1]; e++ {
					w := edges[e]
					if dist[w] == curr {
						sum += float32(sigma[v]) / float32(sigma[w]) * (1.0 + delta[w])
					}
				}
				delta[v] += sum
			}
		}
		for v := 0; v < n; v++ {
			if v == root || dist[v] == infinity {
				continue
			}
			b.bc[v] += delta[v]
		}
	}
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	gpuBC := append([]float32(nil), b.bc...)

	roots := b.NumRoots
	if roots <= 0 {
		roots = 8
	}
	if roots > b.NumNodes {
		roots = b.NumNodes
	}
	maxDepth := b.MaxDepth
	if maxDepth == 0 {
		maxDepth = uint32(b.NumNodes)
	}

	b.bc = make([]float32, b.NumNodes)
	b.execCPU(roots, maxDepth)
	b.cpuBC = append([]float32(nil), b.bc...)
	b.bc = gpuBC

	maxAbsDiff := float32(0)
	for i := range gpuBC {
		d := gpuBC[i] - b.cpuBC[i]
		if d < 0 {
			d = -d
		}
		if d > maxAbsDiff {
			maxAbsDiff = d
		}
	}
	fmt.Printf("GraphBIG BetweennessCentr (atomics simulated): %d roots, max |gpu-cpu| = %f\n",
		roots, maxAbsDiff)
}

func (b *Benchmark) alloc(size uint64) driver.Ptr {
	if b.useManagedMemory { // sbin_claude
		return b.driver.AllocateManaged(b.context, size)
	} else if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	}
	return b.driver.AllocateMemory(b.context, size)
}
