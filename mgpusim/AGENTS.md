# MGPUSIM MODULE GUIDE

## OVERVIEW

MGPUSim is a cycle-accurate multi-GPU simulator for AMD GCN3, built on the Akita discrete-event framework. `go.mod` replaces the published Akita module with the local checkout at `../akita`. The `nvidia/` directory is a prototype and is not supported.

## STRUCTURE

Stable AMD paths:
- `amd/driver/`: host command queues, memory allocation, kernel launch.
- `amd/insts/`: GCN3 instruction definitions, decoder, disassembler.
- `amd/kernels/`: HSACO ELF loading.
- `amd/emu/`: fast functional emulation for correctness checks.
- `amd/timing/`: cycle-accurate performance simulation.
- `amd/benchmarks/`: benchmark implementations with embedded HSACO binaries.
- `amd/samples/`: runnable examples and the shared runner.
- `amd/tests/`: acceptance and deterministic test suites.

Unsupported prototype:
- `nvidia/`

## WHERE TO LOOK

- Sample entry point and wiring: `amd/samples/*/*.go`, `amd/samples/runner/runner.go`.
- Platform and GPU configuration: `amd/samples/runner/timingconfig/`.
- Driver and dispatch: `amd/driver/` and `amd/timing/cp/`.
- Functional execution: `amd/emu/`.
- Timing compute unit: `amd/timing/cu/`.
- Multi-GPU communication: `amd/timing/rdma/`.
- Unified memory page migration: `amd/timing/pagemigrationcontroller/`.

## CONVENTIONS

- Two simulation modes:
  - Emulation: fast functional execution, usually run with `-verify`.
  - Timing: cycle-accurate modeling, run with `-timing`.
- HSACO binaries are embedded with `//go:embed kernels.hsaco`.
- GCN3 uses HSACO V2/V3 with a 256-byte kernel header in `.text`; V5 uses a 64-byte descriptor in `.rodata`.
- Tests use Ginkgo/Gomega with `go.uber.org/mock`; regenerate mocks with `mockgen` after interface changes.
- Mandatory timing flags: `-timing -parallel -gpu=virtual-caching -arch=gcn3 -report-all`. <!-- sbin_codex -->

## ANTI-PATTERNS

- Do not rely on `nvidia/`; it is unsupported.
- Virtual-caching uses a simplified virtual-address model for L1V, L1S, and L2 data caches; L2D misses/refills and dirty writebacks translate through the per-slice L2 address translator, shared L2TLB, and GMMU at the DRAM boundary. <!-- sbin_codex -->
- Do not assume `mi300a` or other unhandled GPU selectors target distinct hardware; they may fall through to `r9nano` defaults. <!-- sbin_codex -->
- Do not treat `HSACO/binaries/` or embedded `.hsaco` files as editable source; regenerate them through the HIP/ROCm toolchain.
- Do not change `go.mod` without confirming the local Akita `replace` directive remains intact.

## TESTING

- Package tests use Ginkgo/Gomega; CI excludes the `mccl` package from the recursive unit run.
- Acceptance tests compile samples and cover GCN3/CDNA3 in emulation and timing modes.
- Deterministic tests rerun selected samples and compare SQLite metric tables.
- Single-GPU acceptance takes minutes; multi-GPU cases can exceed 30 minutes.
- See the repository root `AGENTS.md` for exact commands and required sample flags.

## NOTES

- The repository root owns toolchain setup and the canonical command reference.
- AMD GCN3 is the only stable target; NVIDIA support is experimental.
