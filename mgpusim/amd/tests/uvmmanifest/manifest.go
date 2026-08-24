// Package uvmmanifest is the single checked-in contract table shared by the
// UVM evaluation todos. It enumerates every timing-evaluation application
// benchmark case (24 rows: 18 core + 6 DNN), its exact sample argument
// vector, its D/R/E metric profile, its verification oracle class, and the
// required GPU-accessed application buffers that must be allocated through
// the driver's managed-memory (UVM) API at their existing allocation seams
// when -uvm is enabled.
//
// Each buffer row records the allocation seam as (package, owning symbol,
// enabled AllocateManagedMemory expression, exact disabled expression,
// exclusion reason). The AST audit in manifest_test.go proves every required
// row has BOTH branches in the current source, and that every excluded row is
// truthfully excluded. Todo 26 drives the acceptance runs from this table.
//
// sbin_codex
package uvmmanifest

// Profile is the UVM metric profile assigned to a case by the plan's Todo 26
// table: D = demand, R = remote/counter, E = eviction.
type Profile string

const (
	// ProfileD is the demand profile: -uvm -uvm-access-counter=false,
	// capacity omitted; requires managed pages, raw faults and H2D bytes > 0.
	ProfileD Profile = "D"
	// ProfileR is the remote/counter profile:
	// -uvm -uvm-access-counter=true -uvm-access-counter-threshold=8; adds
	// remote bytes, notifications and AC migrations > 0.
	ProfileR Profile = "R"
	// ProfileE is the eviction profile:
	// -uvm -uvm-access-counter=false -uvm-gpu-memory-capacity=65536;
	// requires eviction count/bytes > 0 and capacity <= 65536.
	ProfileE Profile = "E"
)

// OracleClass names the verification oracle family for a case.
type OracleClass string

const (
	// OracleCPUCompare is the existing CPU shadow compare used by the core
	// benchmarks (matrix compare, BFS cost compare, ...).
	OracleCPUCompare OracleClass = "cpu-compare"
	// OracleReceipt is the seeded finite/tolerance-checked verification
	// receipt used by the DNN benchmarks (forward/output, loss, gradients,
	// parameters).
	OracleReceipt OracleClass = "receipt"
)

// BufferRow describes one GPU-accessed application buffer allocation seam.
//
// OwningSymbol is either "Struct.field" (a struct field assigned the
// allocation, e.g. "Benchmark.dInputData") or "Struct.Method" (a method whose
// body performs the allocation, e.g. "GPUOperator.Create" or
// "Benchmark.allocate"). EnabledExpr is the exact AllocateManagedMemory call
// expression; DisabledExpr is the exact original allocation call expression
// that must be preserved when -uvm is disabled. Exclusion, when non-empty, is
// the reason this buffer is deliberately not converted; excluded rows still
// record their exact disabled expression (the current reality).
type BufferRow struct {
	Package      string // package dir under mgpusim/amd/benchmarks
	OwningSymbol string // "Struct.field" or "Struct.Method"
	EnabledExpr  string // exact AllocateManagedMemory call expression
	DisabledExpr string // exact original allocation call expression
	Exclusion    string // exclusion reason; empty means the buffer is required
}

// CaseRow is one evaluation case contract.
type CaseRow struct {
	Case        string      // benchmark case name
	SamplePath  string      // sample dir under mgpusim, e.g. "amd/samples/atax"
	Args        []string    // exact bounded sample argument vector (Todo 26 table)
	Profile     Profile     // D/R/E metric profile
	OracleClass OracleClass // verification oracle family
	Buffers     []BufferRow // required/excluded buffer allocation seams
}

// Exclusion records a deliberate whole-benchmark or category exclusion with
// its reason. These are audited by TestManifestExclusions.
type Exclusion struct {
	Target string // benchmark or category name
	Reason string // why it is excluded from UVM evaluation
}

// buf builds a required buffer row.
func buf(pkg, symbol, enabled, disabled string) BufferRow {
	return BufferRow{
		Package:      pkg,
		OwningSymbol: symbol,
		EnabledExpr:  enabled,
		DisabledExpr: disabled,
	}
}

// excl builds an excluded buffer row. Excluded rows carry only the exact
// disabled expression (the current reality) plus the exclusion reason.
func excl(pkg, symbol, disabled, reason string) BufferRow {
	return BufferRow{
		Package:      pkg,
		OwningSymbol: symbol,
		DisabledExpr: disabled,
		Exclusion:    reason,
	}
}

// gputensorCreate is the single allocation funnel for every DNN tensor:
// gputensor.GPUOperator.Create. All six DNN cases share it.
var gputensorCreate = buf(
	"dnn/gputensor",
	"GPUOperator.Create",
	"o.driver.AllocateManagedMemory(o.ctx, uint64(t.NumElement()*sizeOfFloat32))",
	"o.driver.AllocateMemory(o.ctx, uint64(t.NumElement()*sizeOfFloat32))",
)

// gputensorTempBuffers are the GPU-used transpose/rotate/dilate/sum/
// cross-entropy temporary buffers in gputensor/operator.go. They are shared by
// every DNN case.
var gputensorTempBuffers = []BufferRow{
	buf("dnn/gputensor", "GPUOperator.prepareTranspose.dOrder",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(dim*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(dim*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.prepareTranspose.dInIndexBuf",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(t.NumElement()*dim*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(t.NumElement()*dim*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.Rotate180.dInSize",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(dim*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(dim*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.Rotate180.dInIndexBuf",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(t.NumElement()*dim*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(t.NumElement()*dim*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.Dilate.dInSize",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(dim*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(dim*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.Dilate.dDilate",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(2*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(2*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.Dilate.dInIndexBuf",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(output.NumElement()*dim*sizeOfInt32))",
		"o.driver.AllocateMemory(o.ctx, uint64(output.NumElement()*dim*sizeOfInt32))"),
	buf("dnn/gputensor", "GPUOperator.prepareSumOneAxis.dInSize",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(t.Dim()*4))",
		"o.driver.AllocateMemory(o.ctx, uint64(t.Dim()*4))"),
	buf("dnn/gputensor", "GPUOperator.prepareSumOneAxis.dOutSize",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(len(outSize)*4))",
		"o.driver.AllocateMemory(o.ctx, uint64(len(outSize)*4))"),
	buf("dnn/gputensor", "GPUOperator.prepareSumOneAxis.dInIndexBuf",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(globalSize*t.Dim()*4))",
		"o.driver.AllocateMemory(o.ctx, uint64(globalSize*t.Dim()*4))"),
	buf("dnn/gputensor", "GPUOperator.prepareSumOneAxis.dOutIndexBuf",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(globalSize*out.Dim()*4))",
		"o.driver.AllocateMemory(o.ctx, uint64(globalSize*out.Dim()*4))"),
	buf("dnn/gputensor", "GPUOperator.CrossEntropyDerivative.dLabel",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(len(label)*4))",
		"o.driver.AllocateMemory(o.ctx, uint64(len(label)*4))"),
	buf("dnn/gputensor", "GPUOperator.SoftmaxCrossEntropyDerivative.dLabel",
		"o.driver.AllocateManagedMemory(o.ctx, uint64(len(label)*4))",
		"o.driver.AllocateMemory(o.ctx, uint64(len(label)*4))"),
}

// dnnBuffers returns the shared gputensor buffer rows for a DNN case.
func dnnBuffers() []BufferRow {
	rows := make([]BufferRow, 0, 1+len(gputensorTempBuffers))
	rows = append(rows, gputensorCreate)
	rows = append(rows, gputensorTempBuffers...)
	return rows
}

// Manifest is the 24-row contract table. It is the sole source shared by
// Todo 26 for the UVM evaluation acceptance runs.
var Manifest = []CaseRow{
	{
		Case:        "atax",
		SamplePath:  "amd/samples/atax",
		Args:        []string{"-x=256", "-y=256"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("polybench/atax", "Benchmark.dA",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NY*b.NX*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NY*b.NX*4))"),
			buf("polybench/atax", "Benchmark.dX",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NY*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NY*4))"),
			buf("polybench/atax", "Benchmark.dY",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NY*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NY*4))"),
			buf("polybench/atax", "Benchmark.dTmp",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NX*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NX*4))"),
		},
	},
	{
		Case:        "bfs",
		SamplePath:  "amd/samples/bfs",
		Args:        []string{"-node=1024", "-degree=3", "-depth=0"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("shoc/bfs", "Benchmark.dFrontier",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumNode*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumNode*4))"),
			buf("shoc/bfs", "Benchmark.dEdgeArray",
				"b.driver.AllocateManagedMemory(b.context, uint64((b.NumNode+1)*4))",
				"b.driver.AllocateMemory(b.context, uint64((b.NumNode+1)*4))"),
			buf("shoc/bfs", "Benchmark.dEdgeArrayAux",
				"b.driver.AllocateManagedMemory(b.context, uint64(len(b.hEdgeList)*4))",
				"b.driver.AllocateMemory(b.context, uint64(len(b.hEdgeList)*4))"),
			buf("shoc/bfs", "Benchmark.dFlag",
				"b.driver.AllocateManagedMemory(b.context, 4)",
				"b.driver.AllocateMemory(b.context, 4)"),
		},
	},
	{
		Case:        "bicg",
		SamplePath:  "amd/samples/bicg",
		Args:        []string{"-x=256", "-y=256"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("polybench/bicg", "Benchmark.dA",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NY*b.NX*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NY*b.NX*4))"),
			buf("polybench/bicg", "Benchmark.dR",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NX*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NX*4))"),
			buf("polybench/bicg", "Benchmark.dS",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NY*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NY*4))"),
			buf("polybench/bicg", "Benchmark.dP",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NY*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NY*4))"),
			buf("polybench/bicg", "Benchmark.dQ",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NX*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NX*4))"),
		},
	},
	{
		Case:        "fastwalshtransform",
		SamplePath:  "amd/samples/fastwalshtransform",
		Args:        []string{"-length=1024"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/fastwalshtransform", "Benchmark.dInputArray",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Length*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Length*4))"),
		},
	},
	{
		Case:        "fft",
		SamplePath:  "amd/samples/fft",
		Args:        []string{"-MB=1", "-bytes=0", "-passes=1"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("shoc/fft", "Benchmark.dSource",
				"b.driver.AllocateManagedMemory(b.context, b.usedBytes)",
				"b.driver.AllocateMemory(b.context, b.usedBytes)"),
		},
	},
	{
		Case:        "fir",
		SamplePath:  "amd/samples/fir",
		Args:        []string{"-length=8192", "-taps=16"},
		Profile:     ProfileR,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("heteromark/fir", "Benchmark.gFilterData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.numTaps*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.numTaps*4))"),
			buf("heteromark/fir", "Benchmark.gHistoryData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.numTaps*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.numTaps*4))"),
			buf("heteromark/fir", "Benchmark.gInputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Length*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Length*4))"),
			buf("heteromark/fir", "Benchmark.gOutputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Length*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Length*4))"),
		},
	},
	{
		Case:        "floydwarshall",
		SamplePath:  "amd/samples/floydwarshall",
		Args:        []string{"-node=16", "-iter=4"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/floydwarshall", "Benchmark.dOutputPathMatrix",
				"b.driver.AllocateManagedMemory(b.context, uint64(numNodes*numNodes*4))",
				"b.driver.AllocateMemory(b.context, uint64(numNodes*numNodes*4))"),
			buf("amdappsdk/floydwarshall", "Benchmark.dOutputPathDistanceMatrix",
				"b.driver.AllocateManagedMemory(b.context, uint64(numNodes*numNodes*4))",
				"b.driver.AllocateMemory(b.context, uint64(numNodes*numNodes*4))"),
		},
	},
	{
		Case:        "kmeans",
		SamplePath:  "amd/samples/kmeans",
		Args:        []string{"-points=1024", "-features=32", "-clusters=5", "-max-iter=5"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("heteromark/kmeans", "Benchmark.dFeatures",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumPoints*b.NumFeatures*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumPoints*b.NumFeatures*4))"),
			buf("heteromark/kmeans", "Benchmark.dFeaturesSwap",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumPoints*b.NumFeatures*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumPoints*b.NumFeatures*4))"),
			buf("heteromark/kmeans", "Benchmark.dMembership",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumPoints*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumPoints*4))"),
			buf("heteromark/kmeans", "Benchmark.dClusters",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumClusters*b.NumFeatures*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumClusters*b.NumFeatures*4))"),
		},
	},
	{
		Case:        "matrixmultiplication",
		SamplePath:  "amd/samples/matrixmultiplication",
		Args:        []string{"-x=128", "-y=128", "-z=128"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/matrixmultiplication", "GPUMatrixMultiplier.initMemory.gA",
				"m.driver.AllocateManagedMemory(m.context, uint64(mA.Width*mA.Height*4))",
				"m.driver.AllocateMemory(m.context, uint64(mA.Width*mA.Height*4))"),
			buf("amdappsdk/matrixmultiplication", "GPUMatrixMultiplier.initMemory.gB",
				"m.driver.AllocateManagedMemory(m.context, uint64(mB.Width*mB.Height*4))",
				"m.driver.AllocateMemory(m.context, uint64(mB.Width*mB.Height*4))"),
			buf("amdappsdk/matrixmultiplication", "GPUMatrixMultiplier.initMemory.gC",
				"m.driver.AllocateManagedMemory(m.context, uint64(mC.Width*mC.Height*4))",
				"m.driver.AllocateMemory(m.context, uint64(mC.Width*mC.Height*4))"),
			excl("amdappsdk/matrixmultiplication", "GPUMatrixMultiplier.blockABuf",
				"m.driver.AllocateMemory(m.context, uint64(32*32*4))",
				"CDNA3-only scratch (allocated only when Arch == CDNA3); GCN3-unreachable in the timing evaluation"),
		},
	},
	{
		Case:        "matrixtranspose",
		SamplePath:  "amd/samples/matrixtranspose",
		Args:        []string{"-width=1024"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/matrixtranspose", "Benchmark.dInputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(numData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numData*4))"),
			buf("amdappsdk/matrixtranspose", "Benchmark.dOutputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(numData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numData*4))"),
		},
	},
	{
		Case:        "nbody",
		SamplePath:  "amd/samples/nbody",
		Args:        []string{"-iter=1", "-particles=256"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/nbody", "Benchmark.currPos",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.numBodies*4*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.numBodies*4*4))"),
			buf("amdappsdk/nbody", "Benchmark.currVel",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.numBodies*4*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.numBodies*4*4))"),
			buf("amdappsdk/nbody", "Benchmark.newPos",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.numBodies*4*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.numBodies*4*4))"),
			buf("amdappsdk/nbody", "Benchmark.newVel",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.numBodies*4*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.numBodies*4*4))"),
			excl("amdappsdk/nbody", "Benchmark.localPosBuf",
				"b.driver.AllocateMemory(b.context, uint64(b.groupSize*4*4))",
				"CDNA3-only LDS-replacement buffer (allocated only when Arch == CDNA3); GCN3-unreachable in the timing evaluation"),
		},
	},
	{
		Case:        "nw",
		SamplePath:  "amd/samples/nw",
		Args:        []string{"-length=64"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("rodinia/nw", "Benchmark.allocate.dInputItemSets",
				"b.driver.AllocateManagedMemory(b.context, byteSize)",
				"b.driver.AllocateMemory(b.context, byteSize)"),
			buf("rodinia/nw", "Benchmark.allocate.dOutputItemSets",
				"b.driver.AllocateManagedMemory(b.context, byteSize)",
				"b.driver.AllocateMemory(b.context, byteSize)"),
			buf("rodinia/nw", "Benchmark.allocate.dReference",
				"b.driver.AllocateManagedMemory(b.context, byteSize)",
				"b.driver.AllocateMemory(b.context, byteSize)"),
		},
	},
	{
		Case:        "pagerank",
		SamplePath:  "amd/samples/pagerank",
		Args:        []string{"-node=64", "-sparsity=0.5", "-iterations=2"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("heteromark/pagerank", "Benchmark.dPageRank",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumNodes*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumNodes*4))"),
			buf("heteromark/pagerank", "Benchmark.dPageRankTemp",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumNodes*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumNodes*4))"),
			buf("heteromark/pagerank", "Benchmark.dRowOffsets",
				"b.driver.AllocateManagedMemory(b.context, uint64((b.NumNodes+1)*4))",
				"b.driver.AllocateMemory(b.context, uint64((b.NumNodes+1)*4))"),
			buf("heteromark/pagerank", "Benchmark.dColumnNumbers",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumConnections*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumConnections*4))"),
			buf("heteromark/pagerank", "Benchmark.dValues",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.NumConnections*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.NumConnections*4))"),
		},
	},
	{
		Case:        "relu",
		SamplePath:  "amd/samples/relu",
		Args:        []string{"-length=4096"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("dnn/layer_benchmarks/relu", "Benchmark.gInputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Length*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Length*4))"),
			buf("dnn/layer_benchmarks/relu", "Benchmark.gOutputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Length*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Length*4))"),
		},
	},
	{
		Case:        "simpleconvolution",
		SamplePath:  "amd/samples/simpleconvolution",
		Args:        []string{"-width=64", "-height=64", "-mask-size=3"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/simpleconvolution", "Benchmark.dInputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(numInputData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numInputData*4))"),
			buf("amdappsdk/simpleconvolution", "Benchmark.dOutputData",
				"b.driver.AllocateManagedMemory(b.context, uint64(numOutputData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numOutputData*4))"),
			buf("amdappsdk/simpleconvolution", "Benchmark.dMasks",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.maskSize*b.maskSize*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.maskSize*b.maskSize*4))"),
		},
	},
	{
		Case:        "spmv",
		SamplePath:  "amd/samples/spmv",
		Args:        []string{"-dim=128", "-sparsity=0.01"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("shoc/spmv", "Benchmark.dValData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.nItems*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.nItems*4))"),
			buf("shoc/spmv", "Benchmark.dVecData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Dim*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Dim*4))"),
			buf("shoc/spmv", "Benchmark.dColsData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.nItems*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.nItems*4))"),
			buf("shoc/spmv", "Benchmark.dRowDData",
				"b.driver.AllocateManagedMemory(b.context, uint64((b.Dim+1)*4))",
				"b.driver.AllocateMemory(b.context, uint64((b.Dim+1)*4))"),
			buf("shoc/spmv", "Benchmark.dOutData",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.Dim*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.Dim*4))"),
		},
	},
	{
		Case:        "stencil2d",
		SamplePath:  "amd/samples/stencil2d",
		Args:        []string{"-row=128", "-col=128", "-iter=1"},
		Profile:     ProfileE,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("shoc/stencil2d", "Benchmark.dData1",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.paddedDataSize*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.paddedDataSize*4))"),
			buf("shoc/stencil2d", "Benchmark.dData2",
				"b.driver.AllocateManagedMemory(b.context, uint64(b.paddedDataSize*4))",
				"b.driver.AllocateMemory(b.context, uint64(b.paddedDataSize*4))"),
		},
	},
	{
		Case:        "vectoradd",
		SamplePath:  "amd/samples/vectoradd",
		Args:        []string{"-width=4096", "-height=1"},
		Profile:     ProfileD,
		OracleClass: OracleCPUCompare,
		Buffers: []BufferRow{
			buf("amdappsdk/vectoradd", "Benchmark.dA",
				"b.driver.AllocateManagedMemory(b.context, uint64(numData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numData*4))"),
			buf("amdappsdk/vectoradd", "Benchmark.dB",
				"b.driver.AllocateManagedMemory(b.context, uint64(numData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numData*4))"),
			buf("amdappsdk/vectoradd", "Benchmark.dC",
				"b.driver.AllocateManagedMemory(b.context, uint64(numData*4))",
				"b.driver.AllocateMemory(b.context, uint64(numData*4))"),
		},
	},
	{
		Case:       "conv2d",
		SamplePath: "amd/samples/conv2d",
		Args: []string{
			"-N=1", "-C=1", "-H=28", "-W=28", "-output-channel=3",
			"-kernel-height=3", "-kernel-width=3", "-pad-x=0", "-pad-y=0",
			"-stride-x=1", "-stride-y=1", "-enable-backward=false",
		},
		Profile:     ProfileD,
		OracleClass: OracleReceipt,
		Buffers:     dnnBuffers(),
	},
	{
		Case:       "im2col",
		SamplePath: "amd/samples/im2col",
		Args: []string{
			"-N=1", "-C=1", "-H=28", "-W=28", "-kernel-height=3",
			"-kernel-width=3", "-pad-x=0", "-pad-y=0", "-stride-x=1",
			"-stride-y=1", "-dilate-x=1", "-dilate-y=1",
		},
		Profile:     ProfileD,
		OracleClass: OracleReceipt,
		Buffers:     dnnBuffers(),
	},
	{
		Case:       "lenet",
		SamplePath: "amd/samples/lenet",
		Args: []string{
			"-epoch=1", "-max-batch-per-epoch=2", "-batch-size=32",
			"-enable-testing=false", "-enable-verification=false",
		},
		Profile:     ProfileD,
		OracleClass: OracleReceipt,
		Buffers:     dnnBuffers(),
	},
	{
		Case:       "minerva",
		SamplePath: "amd/samples/minerva",
		Args: []string{
			"-epoch=1", "-max-batch-per-epoch=2", "-batch-size=32",
			"-enable-testing=false", "-enable-verification=false",
		},
		Profile:     ProfileD,
		OracleClass: OracleReceipt,
		Buffers:     dnnBuffers(),
	},
	{
		Case:       "vgg16",
		SamplePath: "amd/samples/vgg16",
		Args: []string{
			"-epoch=1", "-max-batch-per-epoch=2", "-batch-size=8",
			"-enable-testing=false", "-enable-verification=false",
		},
		Profile:     ProfileD,
		OracleClass: OracleReceipt,
		Buffers:     dnnBuffers(),
	},
	{
		Case:        "xor",
		SamplePath:  "amd/samples/xor",
		Args:        []string{"-epoch=1", "-max-batch-per-epoch=1", "-batch-size=4"},
		Profile:     ProfileD,
		OracleClass: OracleReceipt,
		Buffers:     dnnBuffers(),
	},
}

// Exclusions records every deliberate exclusion from the UVM evaluation with
// its reason. TestManifestExclusions audits that each category is present and
// that excluded benchmarks are absent from the case list.
var Exclusions = []Exclusion{
	{
		Target: "bitonicsort",
		Reason: "not part of the 24-case timing-evaluation set; excluded from UVM evaluation",
	},
	{
		Target: "aes",
		Reason: "not part of the 24-case timing-evaluation set; excluded from UVM evaluation",
	},
	{
		Target: "multi-GPU dataparallelism scratch",
		Reason: "gputraining.DataParallelismMultiGPUTrainer scratch (averageGradient, MCCL buffers, bufs) stays unmanaged; no dataparallel scratch allocation becomes managed",
	},
	{
		Target: "GCN3-unreachable CDNA3 buffers",
		Reason: "CDNA3-only allocations (nbody.localPosBuf, matrixmultiplication.blockABuf) are unreachable in the GCN3 timing evaluation and are not converted",
	},
	{
		Target: "infrastructure",
		Reason: "driver-internal allocations, page-table storage, and command buffers are simulator infrastructure and must not be converted per spec §3 scope",
	},
	{
		Target: "generated copies",
		Reason: "scripts/benchmarks/ and other generated/copied areas are not authoritative source and are never edited",
	},
}
