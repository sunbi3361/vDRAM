package kcore

import (
	"fmt"
	"log"

	_ "embed"

	"github.com/sarchlab/mgpusim/v4/amd/arch"
	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/common"
	"github.com/sarchlab/mgpusim/v4/amd/driver"
	"github.com/sarchlab/mgpusim/v4/amd/insts"
)

const wgSize = 256

// ArgsPeel: 6 ptrs = 48B + 24 hidden = 72B.
type ArgsPeel struct {
	Vplist              driver.Ptr
	Rmlist              driver.Ptr
	Flaglist            driver.Ptr
	Offsets             driver.Ptr
	Edges               driver.Ptr
	State               driver.Ptr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// ArgsPrune: 6 ptrs = 48B + 24 hidden = 72B.
type ArgsPrune struct {
	Vplist              driver.Ptr
	Rmlist              driver.Ptr
	Flaglist            driver.Ptr
	RmCnt               driver.Ptr
	State               driver.Ptr
	Dummy               driver.Ptr
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
	K             uint32
	MaxIterations uint32
	VerifyOnCPU   bool

	graph    common.CSRGraph
	hVplist  []uint32
	hRmlist  []uint32
	hFlag    []uint32
	dVplist  driver.Ptr
	dRmlist  driver.Ptr
	dFlag    driver.Ptr
	dOffsets driver.Ptr
	dEdges   driver.Ptr
	dRmCnt   driver.Ptr
	dState   driver.Ptr
	dDummy   driver.Ptr

	kPeel  *insts.KernelCodeObject
	kPrune *insts.KernelCodeObject

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
		NumNodes: 1024, Degree: 8, K: 3, MaxIterations: 512, VerifyOnCPU: true,
	}
	b.queue = driver.CreateCommandQueue(b.context)
	if len(hsacoBytes) > 0 {
		b.kPeel = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "kcore_peel_neighbors")
		b.kPrune = insts.LoadKernelCodeObjectFromBytes(hsacoBytes, "kcore_prune")
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
		b.graph = common.GenerateSynthetic(b.NumNodes, b.Degree, 5)
	}
	b.NumNodes = b.graph.NumNodes()
	b.hVplist = make([]uint32, b.NumNodes)
	for v := 0; v < b.NumNodes; v++ {
		b.hVplist[v] = b.graph.Offsets[v+1] - b.graph.Offsets[v]
	}
	b.hRmlist = make([]uint32, b.NumNodes)
	b.hFlag = make([]uint32, b.NumNodes)

	if b.kPeel == nil || b.kPrune == nil {
		b.execCPU()
		return
	}

	b.dVplist = b.alloc(uint64(b.NumNodes * 4))
	b.dRmlist = b.alloc(uint64(b.NumNodes * 4))
	b.dFlag = b.alloc(uint64(b.NumNodes * 4))
	b.dOffsets = b.alloc(uint64(len(b.graph.Offsets) * 4))
	b.dEdges = b.alloc(uint64(len(b.graph.Edges) * 4))
	b.dRmCnt = b.alloc(4)
	b.dState = b.alloc(4 * 4)
	b.dDummy = b.alloc(4)

	b.driver.MemCopyH2D(b.context, b.dVplist, b.hVplist)
	b.driver.MemCopyH2D(b.context, b.dRmlist, b.hRmlist)
	b.driver.MemCopyH2D(b.context, b.dFlag, b.hFlag)
	b.driver.MemCopyH2D(b.context, b.dOffsets, b.graph.Offsets)
	b.driver.MemCopyH2D(b.context, b.dEdges, b.graph.Edges)
	b.driver.MemCopyH2D(b.context, b.dRmCnt, uint32(0))

	threads := uint32(((b.NumNodes + wgSize - 1) / wgSize) * wgSize)
	if threads < wgSize {
		threads = wgSize
	}
	global := [3]uint32{threads, 1, 1}
	local := [3]uint16{wgSize, 1, 1}

	for iter := uint32(0); iter < b.MaxIterations; iter++ {
		state := []uint32{1, b.K, 0, uint32(b.NumNodes)} // [over, k, _, num_nodes]
		b.driver.MemCopyH2D(b.context, b.dState, state)

		argsP := ArgsPeel{
			Vplist: b.dVplist, Rmlist: b.dRmlist, Flaglist: b.dFlag,
			Offsets: b.dOffsets, Edges: b.dEdges, State: b.dState,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kPeel, global, local, &argsP)
		b.driver.DrainCommandQueue(b.queue)

		argsR := ArgsPrune{
			Vplist: b.dVplist, Rmlist: b.dRmlist, Flaglist: b.dFlag,
			RmCnt: b.dRmCnt, State: b.dState, Dummy: b.dDummy,
		}
		b.driver.EnqueueLaunchKernel(
			b.queue,
			b.kPrune, global, local, &argsR)
		b.driver.DrainCommandQueue(b.queue)

		b.driver.MemCopyD2H(b.context, state, b.dState)
		if state[0] != 0 {
			break
		}
	}
	b.driver.MemCopyD2H(b.context, b.hVplist, b.dVplist)
	b.driver.MemCopyD2H(b.context, b.hRmlist, b.dRmlist)
}

func (b *Benchmark) execCPU() {
	for iter := uint32(0); iter < b.MaxIterations; iter++ {
		for v := 0; v < b.NumNodes; v++ {
			if b.hFlag[v] == 0 {
				continue
			}
			for e := b.graph.Offsets[v]; e < b.graph.Offsets[v+1]; e++ {
				to := b.graph.Edges[e]
				if b.hRmlist[to] == 0 {
					b.hVplist[to]--
				}
			}
			b.hFlag[v] = 0
		}
		over := true
		for v := 0; v < b.NumNodes; v++ {
			if b.hRmlist[v] == 0 && b.hVplist[v] < b.K {
				b.hRmlist[v] = 1
				b.hFlag[v] = 1
				over = false
			}
		}
		if over {
			break
		}
	}
}

func (b *Benchmark) Verify() {
	if !b.VerifyOnCPU {
		return
	}
	gpuRm := append([]uint32(nil), b.hRmlist...)

	b.hVplist = make([]uint32, b.NumNodes)
	for v := 0; v < b.NumNodes; v++ {
		b.hVplist[v] = b.graph.Offsets[v+1] - b.graph.Offsets[v]
	}
	b.hRmlist = make([]uint32, b.NumNodes)
	b.hFlag = make([]uint32, b.NumNodes)
	b.execCPU()

	mismatch := 0
	for i := range gpuRm {
		if gpuRm[i] != b.hRmlist[i] {
			mismatch++
		}
	}
	fmt.Printf("GraphBIG kCore (k=%d): %d/%d vertices match reference removal state\n",
		b.K, b.NumNodes-mismatch, b.NumNodes)
}

func (b *Benchmark) alloc(size uint64) driver.Ptr {
	if b.useManagedMemory { // sbin_claude
		return b.driver.AllocateManaged(b.context, size)
	} else if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	}
	return b.driver.AllocateMemory(b.context, size)
}
