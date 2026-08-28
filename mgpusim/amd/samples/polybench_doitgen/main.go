package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/polybench/doitgen"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var nr = flag.Int("nr", 128, "NR dimension.")
var nq = flag.Int("nq", 128, "NQ dimension.")
var np = flag.Int("np", 128, "NP dimension.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := doitgen.NewBenchmark(runner.Driver())
	benchmark.NR = *nr
	benchmark.NQ = *nq
	benchmark.NP = *np
	benchmark.Arch = runner.ArchType

	runner.AddBenchmark(benchmark)

	runner.Run()
}
