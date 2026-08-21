// Package benchmarks defines Benchmark interface.
package benchmarks

// A Benchmark is a GPU program that can run on the GCN3 simulator
type Benchmark interface {
	SelectGPU(gpuIDs []int)
	Run()
	Verify()
	SetUnifiedMemory()
	SetManagedMemory() // sbin_codex: switch allocations to AllocateManaged for UVM.
}
