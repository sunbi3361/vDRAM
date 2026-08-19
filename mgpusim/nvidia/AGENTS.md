# NVIDIA PROTOTYPE AGENTS GUIDE

## STATUS

This directory is an unsupported prototype. It is a trace-driven AccelSim skeleton, not a production GPU simulator. Build and test passes do not change its experimental status. Do not apply AMD GCN3 conventions, flags, or workflows here.

## STRUCTURE

- `nvidia.go`: entry point. Expects `-trace-dir` and hardcodes an A100 platform.
- `tracereader/`: parses `kernelslist.g` and AccelSim kernel traces into `KernelTrace`.
- `benchmark/`: turns trace metadata into a list of `TraceExec` objects (kernel launches and memcpys).
- `runner/`: runs each `TraceExec` against the platform driver, then starts the Akita engine.
- `platform/`: builds the A100 platform with one GPU.
- `driver/`: host-side command dispatcher. Sends kernel messages to the GPU.
- `gpu/`, `sm/`, `subcore/`: device hierarchy. The GPU contains SMs; each SM contains subcores.

## WHERE TO LOOK

- Hierarchy wiring: `platform/A100builder.go`, `gpu/builder.go`, `sm/builder.go`.
- Trace parsing: `tracereader/builder.go`, `tracereader/reader.go`, `tracereader/trace.go`.
- Kernel execution flow: `driver/driver.go`, `gpu/gpu.go`, `sm/sm.go`, `subcore/subcore.go`.
- Entry and orchestration: `nvidia.go`, `runner/runner.go`, `benchmark/benchmark.go`.
- Test fixtures: `data/simple-trace-example/`.

## CONVENTIONS

- Use pointer-builder style: `new(platform.A100PlatformBuilder).WithFreq(...).Build()`.
- The A100 platform is hardcoded to one GPU, 108 SMs, and 4 subcores per SM.
- Simulation is trace driven. Benchmarks come from AccelSim trace directories, not HSACO binaries.
- Components are Akita `TickingComponent`s that return `madeProgress`.

## ANTI-PATTERNS

- Do not use AMD flags such as `-timing`, `-parallel`, `-arch=gcn3`, `-gpu=r9nano`, or `-report-all`.
- Do not look for HSACO parsing, wavefronts, compute units, command processors, or RDMA.
- Do not add multi-GPU logic expecting the platform to support it; `gpuCount` is pinned to 1.
- Do not treat passing `go test` as evidence that the model is correct.

## TESTING

- Unit tests live in `tracereader/`, `platform/`, and `benchmark/`.
- Tests use trace fixtures in `data/simple-trace-example/` and mock kernel objects.
- CI skips NVIDIA suites (`ginkgo -r --skip-package=nvidia`).
- Known prototype gaps:
  - No memory hierarchy, caches, or DRAM model.
  - Opcode parsing is disabled in `tracereader/reader.go`.
  - `ExecMemcpy.Run` is a no-op.
  - Subcores count instructions but do not execute semantics.
