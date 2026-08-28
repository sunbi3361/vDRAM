package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/kcore"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var dataset = flag.String("dataset", "", "Path to GraphBIG CSR dataset directory")
var numNodes = flag.Int("num-nodes", 1024, "Synthetic node count")
var degree = flag.Int("degree", 8, "Synthetic out degree")
var kval = flag.Uint("kcore", 3, "k-core value")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := kcore.NewBenchmark(runner.Driver())
	benchmark.NumNodes = *numNodes
	benchmark.Degree = *degree
	benchmark.K = uint32(*kval)
	benchmark.DatasetPath = *dataset
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
