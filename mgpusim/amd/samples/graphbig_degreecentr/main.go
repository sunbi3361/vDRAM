package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/graphbig/degreecentr"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var dataset = flag.String("dataset", "", "Path to GraphBIG CSR dataset directory")
var numNodes = flag.Int("num-nodes", 1024, "Synthetic node count")
var degree = flag.Int("degree", 8, "Synthetic out degree")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := degreecentr.NewBenchmark(runner.Driver())
	benchmark.NumNodes = *numNodes
	benchmark.Degree = *degree
	benchmark.DatasetPath = *dataset
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
