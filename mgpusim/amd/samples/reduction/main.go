package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/amdappsdk/reduction"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var size = flag.Int("size", 25165824, "The number of elements in the input.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := reduction.NewBenchmark(runner.Driver())
	benchmark.Length = uint32(*size)
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
