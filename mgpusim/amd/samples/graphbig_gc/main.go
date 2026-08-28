package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/gc"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var dataset = flag.String("dataset", "", "Path to GraphBIG CSR dataset directory")
var numNodes = flag.Int("num-nodes", 1024, "Number of vertices for synthetic graph")
var degree = flag.Int("degree", 8, "Out degree for synthetic graph")
var seed = flag.Int64("seed", 123, "Random seed used to initialize priorities")
var maxIterations = flag.Uint("max-iterations", 64, "Max coloring rounds (0 = num-nodes)")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := gc.NewBenchmark(runner.Driver())
	benchmark.NumNodes = *numNodes
	benchmark.Degree = *degree
	benchmark.RandomSeed = *seed
	benchmark.MaxIterations = uint32(*maxIterations)
	benchmark.DatasetPath = *dataset
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
