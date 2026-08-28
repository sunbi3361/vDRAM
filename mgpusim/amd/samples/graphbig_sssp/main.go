package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/sssp"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var dataset = flag.String("dataset", "", "Path to GraphBIG CSR dataset directory")
var root = flag.Uint64("root", 0, "SSSP root vertex")
var numNodes = flag.Int("num-nodes", 1024, "Number of vertices for synthetic graph")
var degree = flag.Int("degree", 8, "Out degree for synthetic graph")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := sssp.NewBenchmark(runner.Driver())
	benchmark.Root = *root
	benchmark.NumNodes = *numNodes
	benchmark.Degree = *degree
	benchmark.DatasetPath = *dataset
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
