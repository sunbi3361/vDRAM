# TIMING KNOWLEDGE BASE
# branch: main | generated: 2026-08-19

## OVERVIEW

Cycle-accurate AMD GCN3 timing simulation. Every major unit is an `akita/sim.TickingComponent` driven by `Tick()` returning a progress flag. The Command Processor dispatches work-groups, Compute Units execute wavefronts through pipeline stages, and memory, RDMA, PMC, and ROB components model request ordering and multi-GPU data movement.

## STRUCTURE

- `cp/` - Command Processor and internal dispatchers/resources.
- `cu/` - Compute Unit: scheduler, fetch/decode/issue, SIMD, scalar, branch, LDS, and vector-memory units, plus register files and wavefront pools.
- `mem/` - Address translator and simple banked memory.
- `rdma/` - Remote DMA engine for cross-GPU memory access.
- `pagemigrationcontroller/` - Unified memory page migration controller.
- `rob/` - Reorder buffer for in-order response delivery.
- `wavefront/` - Wavefront, work-group, and completion event types.

## WHERE TO LOOK

- `cp/builder.go`, `cp/commandprocessor.go`: CP construction, port wiring, dispatcher ticking, request/response middleware.
- `cp/internal/dispatching/`: dispatcher algorithms (round-robin, greedy, partition), launch overhead, WG completion tracking.
- `cp/internal/resource/`: `CUResourcePool`, V/SGPR and LDS resource masks, `DispatchableCU` interface.
- `cu/cubuilder.go`, `cu/computeunit.go`: CU construction, `Tick()` orchestration, pipeline flush/restart with shadow buffers, CP/ACE/inst/scalar/vector port handling.
- `cu/scheduler.go`, `cu/simdunit.go`, `cu/vectormemoryunit.go`, `cu/scalarunit.go`, `cu/ldsunit.go`: pipeline stages and issue arbitration.
- `rob/rob.go`, `mem/addresstranslator/addresstranslator.go`, `rdma/comp.go`, `pagemigrationcontroller/pmc.go`: memory/control flows, drain/restart, translation coalescing, page migration.

## CONVENTIONS

- Fluent builders: `MakeBuilder().WithEngine(...).WithFreq(...).Build(name)`.
- `Tick()` returns `madeProgress`; the engine reschedules only when progress is made.
- CP dispatches WGs via `MapWGReq` to CU `ToACE`; CU control messages flow through `ToCP`.
- CU shadow buffers preserve in-flight instruction fetches and memory accesses across pipeline flush/restart.
- `panic("never")` and `panic("unknown ...")` assertions mark unexpected messages or unreachable control states. Treat them as protocol violations, not runtime errors to suppress.

## ANTI-PATTERNS

- Do not duplicate r9nano platform wiring here; that belongs to `amd/samples/`.
- Do not bypass `CUResourcePool` when adding dispatch algorithms; SGPR/VGPR/LDS masks must stay consistent.
- Do not ignore `panic("never")` sites when extending control messages; add explicit cases instead.
- Do not conflate emulation (`amd/emu/`) and timing CU internals; timing has explicit scheduling, scoreboarding, and flush semantics.

## TESTING

- Each package has a `*_suite_test.go` bootstrapping Ginkgo.
- `go:generate mockgen` lines in suite files produce `mock_*_test.go` for `sim.Port`, `Engine`, and local interfaces.
- Run package tests with `ginkgo` or `go test` from `mgpusim/amd/timing/<package>`.
- Unit tests mock subcomponents and ports, then verify control flows (flush, restart, drain), request coalescing, dispatcher resource accounting, and ROB ordering invariants.
