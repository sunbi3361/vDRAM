package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/bfs"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var dataset = flag.String("dataset", "", "Path to GraphBIG CSR dataset directory")
var numNodes = flag.Int("num-nodes", 1024, "The number of graph nodes.")
var degree = flag.Int("degree", 8, "The average degree.")
var root = flag.Uint64("root", 0, "The BFS root vertex.")

// sbin_claude: traversal model. "frontier" is the GraphBIG topology-driven
// frontier model (one vertex per thread, two kernels per level, no atomics);
// "warp-centric" is the previous kernel, kept for reproducing earlier results.
var model = flag.String("model", bfs.ModelFrontier,
	"BFS traversal model: frontier or warp-centric.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := bfs.NewBenchmark(runner.Driver())
	benchmark.NumNodes = *numNodes
	benchmark.Degree = *degree
	benchmark.Root = *root
	benchmark.DatasetPath = *dataset
	benchmark.Model = *model // sbin_claude
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
