package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/mafiaports/gups"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var tableSize = flag.Uint64("table-size", 0, "The size of the lookup table.")
var updates = flag.Uint64("n-updates", 0, "The number of updates.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := gups.NewBenchmark(runner.Driver())
	if *tableSize > 0 {
		benchmark.TableSize = *tableSize
	}
	if *updates > 0 {
		benchmark.NUpdates = *updates
	}
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
