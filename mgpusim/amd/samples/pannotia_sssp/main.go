package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/pannotia/sssp"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var numNodes = flag.Int("num-nodes", 1024, "The number of graph nodes.")
var numEdges = flag.Int("num-edges", 8192, "The number of graph edges.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := sssp.NewBenchmark(runner.Driver())
	benchmark.NumNodes = *numNodes
	benchmark.NumItems = *numEdges
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
