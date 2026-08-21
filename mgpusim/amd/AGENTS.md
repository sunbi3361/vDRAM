# AMD SIMULATOR KNOWLEDGE BASE

Stable AMD GCN3 subtree of mgpusim.

---

## OVERVIEW

mgpusim/amd is the stable AMD GCN3 GPU simulator. It is built on the Akita discrete-event framework and supports both fast functional emulation and cycle-level timing simulation, including multi-GPU configurations.

## STRUCTURE

Source domains:
- arch: GCN3 architecture constants and helpers.
- driver: host-side driver (memory allocation, command queues, kernel launch).
- emu: functional emulator compute unit and ALU implementations.
- insts: GCN3 instruction definitions, decoder, disassembler, HSACO parsing.
- kernels: grid and work-group construction from HSACO metadata.
- protocol: message protocols between driver, CP, and CUs.
- timing: cycle-accurate components (CP, CU, RDMA, ROB, page migration).
- samples: runnable examples and shared runner/platform templates.
- benchmarks: benchmark suites with embedded HSACO binaries.
- tests: acceptance and deterministic test suites.

Generated or compiled artifacts (HSACO binaries and embedded kernel files) are not source. Treat them as build outputs.

## WHERE TO LOOK

End-to-end simulation flow:
- samples/runner: builds the platform, driver, and runner.
- driver: allocates GPU memory, copies arguments, and submits dispatch packets.
- protocol: messages between driver, CP, and CUs.
- timing/cp (CommandProcessor): receives dispatch packets and schedules work-groups.
- timing/cu (ComputeUnit): executes wavefronts through the timing pipeline.
- emu/ComputeUnit: executes the same kernels functionally for verification.

Binary path:
- insts/hsaco.go: parses HSACO ELF and kernel descriptors.
- kernels/GridBuilder: turns metadata into grids, work-groups, and wavefronts.
- insts/Disassembler: decodes raw instructions into GCN3 ops.

## CONVENTIONS

- Keep inter-component messages in `protocol/`; driver, CP, and CU packages should consume those shared types.
- Use builders to wire components and domains; constructors should not recreate platform topology.
- Run `go generate ./...` after interfaces used by generated mocks change.
- Parent guides own edit-marker, toolchain, and required-run-flag rules.
- Virtual-caching mandatory flags: `-timing -parallel -gpu=virtual-caching -arch=gcn3 -report-all`. <!-- sbin_codex -->

## ANTI-PATTERNS

- Do not rely on the NVIDIA path in the parent repo; it is unsupported.
- Virtual-caching uses a simplified virtual-address model for L1V, L1S, and L2 data caches; L2D misses/refills and dirty writebacks translate through the per-slice L2 address translator, shared L2TLB, and GMMU at the DRAM boundary. <!-- sbin_codex -->
- Do not assume `mi300a` or other unhandled selectors target distinct hardware; they may fall through to `r9nano` defaults. <!-- sbin_codex -->
- Do not edit HSACO binaries, embedded kernels, or generated mocks as authoritative source.
- Do not let the CP duplicate driver-level command queue logic.

## TESTING

Three test layers:
- Package suites: each major package has Ginkgo/Gomega unit tests.
- tests/acceptance: end-to-end benchmark acceptance suite, often run with -num-gpu=1.
- tests/deterministic: reproducibility tests that check identical results across runs.

run_before_merge.sh captures the CI-style sequence. Prefer targeted package tests during development and run acceptance/deterministic before merging.
