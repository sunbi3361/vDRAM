package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/polybench/lu"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var n = flag.Int("size", 64, "The matrix dimension N.")
var k = flag.Int("k", 1, "The pivot step K.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := lu.NewBenchmark(runner.Driver())
	benchmark.N = *n
	benchmark.K = *k
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
