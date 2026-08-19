# BENCHMARK KNOWLEDGE BASE
# scope: mgpusim/amd/benchmarks

## OVERVIEW

GPU benchmark packages implement the shared `Benchmark` interface and typically embed HSACO kernel binaries via `//go:embed`. Native kernel sources under `native/` regenerate those binaries.

Benchmark families:
- `amdappsdk/`: bitonicsort, fastwalshtransform, floydwarshall, matrixmultiplication, matrixtranspose, nbody, simpleconvolution, vectoradd.
- `dnn/`: deep-learning layers, tensors, operators, and training benchmarks.
- `heteromark/`: aes, fir, kmeans, pagerank.
- `polybench/`: atax, bicg.
- `rodinia/`: nw (Needleman-Wunsch).
- `shoc/`: bfs, fft, spmv, stencil2d.
- `mccl/`: allreduce and broadcast collectives plus communicator helpers.
- `matrix/csr/`: CSR matrix definition and generator for sparse/graph kernels.

## STRUCTURE

Source of truth (edit here):
- `benchmark.go`: shared `Benchmark` interface.
- `*/benchmark.go` and sibling `*.go`: host setup, kernel args, memory allocation, verification.
- `*/native/*.cpp`, `*.cl`, `*.h`: kernel source code.
- `*/native/Makefile`: Docker HIP build for gfx942 kernels where supplied.

Generated/checked-in artifacts (do not edit by hand):
- `*.hsaco` and `*_gfx942.hsaco`: compiled kernel binaries.
- `*.disasm`: LLVM disassembly reference dumps.
- Build intermediates (*.ll, *.bc, *.o, *.cui, *.hipfb).

## WHERE TO LOOK

- `benchmark.go`: common interface.
- `heteromark/fir/fir.go`: canonical two-arch pattern with GCN3/CDNA3 arg structs and `//go:embed`.
- `amdappsdk/matrixmultiplication/`: benchmark that delegates to a separate `GPUMatrixMultiplier`.
- `mccl/`: multi-GPU collective primitives.
- `matrix/csr/`: sparse matrix helper reused by SHOC SPMV/BFS-like workloads.

## CONVENTIONS

- Go packages embed HSACO with `//go:embed`.
- Most benchmarks ship two arch variants selected by `arch.Type`: GCN3 (`kernels.hsaco`) and CDNA3/GFX942 (`kernels_gfx942.hsaco`).
- Native kernels are rebuilt with `native/Makefile` using ROCm Docker `rocm/dev-ubuntu-24.04:7.1.1` and `--offload-arch=gfx942`.

## ANTI-PATTERNS

- Do not edit `.hsaco` or `.disasm` files directly; regenerate from `native/` sources.
- Do not assume every benchmark has both GCN3 and GFX942 kernels; some packages carry only one binary.
- Do not use the wrong kernel build path: lowercase `makefile` targets GCN3/fiji with `clang-ocl`, while `native/Makefile` targets gfx942 through Docker HIP.
- Do not rely on the OpenCL host binaries under `dnn/gputensor/native/` for the simulator; the simulator consumes only the embedded HSACO.

## TESTING

- Build benchmarks: `( cd mgpusim && go build ./amd/benchmarks/... )`
- Validate through a sample that imports the benchmark, such as `mgpusim/amd/samples/fir`; use the canonical sample command in the root guide.
