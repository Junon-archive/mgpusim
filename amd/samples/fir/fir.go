package main

import (
	"flag"

	"github.com/sarchlab/mgpusim/v4/amd/benchmarks/heteromark/fir"
	"github.com/sarchlab/mgpusim/v4/amd/samples/runner"
)

var numData = flag.Int("length", 4096, "The number of samples to filter.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).Init()

	benchmark := fir.NewBenchmark(runner.Driver())
	benchmark.Length = *numData

	runner.AddBenchmark(benchmark)

	runner.Run()
}
