package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/polybench/jacobi1d"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var nFlag = flag.Int("size", 100, "The number of grid points.")
var steps = flag.Int("tsteps", 100, "The number of Jacobi iterations.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := jacobi1d.NewBenchmark(runner.Driver())
	benchmark.N = *nFlag
	benchmark.Steps = *steps
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
