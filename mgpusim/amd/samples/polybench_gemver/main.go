package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/polybench/gemver"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var n = flag.Int("size", 64, "The matrix dimension N.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := gemver.NewBenchmark(runner.Driver())
	benchmark.N = *n
	benchmark.Alpha = 1.5
	benchmark.Beta = 1.5
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
