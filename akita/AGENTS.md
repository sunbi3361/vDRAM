# AKITA MODULE GUIDE

## OVERVIEW

Akita is the discrete-event simulation framework that mgpusim builds on.
This module is `github.com/sarchlab/akita/v4`, pinned to Go 1.24.0 with toolchain go1.24.7.
`sim/` is the foundation: event scheduling, ports, components, and the engine.
`simulation/` is the facade: `Builder`, component registration, and running a simulation.
`mem/`, `noc/`, `tracing/`, `monitoring/`, and `daisen/` provide memory systems, networks, tracing, real-time dashboards, and visualization.
V5 packages are in development alongside v4; v4 remains the stable API.

## STRUCTURE

Maintained source:
- `sim/` - core engine
- `simulation/` - high-level wiring facade
- `mem/` - caches, DRAM, virtual memory, MMU, TLB
- `noc/` - Network-on-Chip messaging, switching, arbitration, mesh
- `tracing/` - trace and profiling infrastructure
- `monitoring/` - AkitaRTM web dashboard
- `daisen/` - visualization server and TypeScript frontend
- `pipelining/` - pipeline simulation components
- `analysis/` - performance analysis tools
- `examples/` - sample simulators such as `ping`

Generated but committed assets:
- `monitoring/web/dist/` and `daisen/static/dist/` are built by npm and embedded in Go
- Mock files are produced by `go generate ./...` with mockgen

## WHERE TO LOOK

- `sim/` - `Engine`, events, ports, components
- `simulation/` - `Builder`, register components, run control
- `mem/` - memory hierarchy and VM
- `noc/` - routing, switches, topologies
- `tracing/` - trace collection
- `monitoring/` - runtime monitoring and dashboard
- `daisen/` - trace visualization
- `examples/ping/` - minimal working simulator

## CONVENTIONS

- Prefer dependency injection; components depend on interfaces, not concrete types.
- Use builder pattern for configuration; `With` methods return the builder for chaining.
- Document packages, structs, and functions. Avoid inline comments; break tricky code into small named functions.
- Keep functions under 60 lines and indentation within three levels.
- Use Ginkgo, Gomega, and gomock for unit tests. Regenerate mocks with `go generate ./...`.
- Build with `go build ./...`. Lint with `golangci-lint run ./...`.
- Run acceptance tests in `noc/acceptance/` and `mem/`.
- The `dist/` directories are generated but committed and embedded; regenerate them, do not hand-edit.

## ANTI-PATTERNS

- Do not edit `dist/` assets or generated mocks as authoritative source.
- Do not rely on v5 packages while v4 compatibility is required.
- Do not add generic Go advice here; keep guidance Akita-specific.
