// Package benchmarks defines Benchmark interface.
package benchmarks

// A Benchmark is a GPU program that can run on the GCN3 simulator
type Benchmark interface {
	SelectGPU(gpuIDs []int)
	Run()
	Verify()
	SetUnifiedMemory()
}

// sbin_codex: ManagedMemoryCapable is implemented by benchmarks whose
// application buffers can be allocated through the driver's managed-memory
// (UVM) API. The runner calls SetManagedMemory on such benchmarks when -uvm
// is enabled. It is a separate optional capability interface so that
// benchmarks not yet converted to managed memory are not forced to change.
type ManagedMemoryCapable interface {
	SetManagedMemory()
}
