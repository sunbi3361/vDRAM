# Akita Memory-System Packages

## OVERVIEW
`akita/mem/` implements the memory hierarchy: request protocols, backing storage, cache models, cycle-accurate DRAM, simple functional memory controllers, and virtual-memory translation. It is a distinct subsystem with active TLB and GMMU work.

## STRUCTURE
- `mem/mem/` - shared protocol (`ReadReq`, `WriteReq`, `DataReadyRsp`, `WriteDoneRsp`, `ControlMsg`) and `Storage`.
- `mem/cache/` - cache framework (`directory.go`, `mshr.go`, `victimfinder.go`) and four policy variants.
- `mem/dram/` - DRAM controller with internal address mapping, command queues, bank state, and refresh timing.
- `mem/vm/` - virtual-memory translation: page tables, TLB, MMU, GMMU, ideal TLB, and page-walk cache.
- `mem/idealmemcontroller/` / `mem/simplebankedmemory/` - fast functional memory back-ends.
- `mem/acceptancetests/` - end-to-end binaries exercised by `acceptance_test.py`.

## WHERE TO LOOK
- Protocol and storage: `mem/mem/protocol.go`, `mem/mem/storage.go`.
- Cache framework: `mem/cache/directory.go`, `mem/cache/mshr.go`, `mem/cache/victimfinder.go`, `mem/cache/protocol.go`.
- Cache policies: `mem/cache/writeback/`, `mem/cache/writeevict/`, `mem/cache/writethrough/`, `mem/cache/writearound/`.
- DRAM internals: `mem/dram/builder.go`, `mem/dram/internal/addressmapping/`, `mem/dram/internal/cmdq/`, `mem/dram/internal/org/`, `mem/dram/internal/trans/`.
- VM translation: `mem/vm/pagetable.go`, `mem/vm/tlb/`, `mem/vm/mmu/`, `mem/vm/gmmu/`, `mem/vm/idealtlb/`, `mem/vm/pagewalkcache/`.

## CONVENTIONS
- Components use the builder pattern; `With*` methods return the builder for chaining.
- Requests carry `vm.PID`; addresses are byte addresses unless converted by `mem.AddressConverter`.
- DRAM timings are expressed in cycles and grouped by same-bank, other-banks-in-group, same-rank, and other-ranks.
- Memory components use interface-based dependency injection and Ginkgo/Gomega unit tests with gomock.
- See the root `AGENTS.md` for build, generate, lint, and test commands.

## ANTI-PATTERNS
- Do not treat generated mocks in `mock_*_test.go` as authoritative source.
- Do not confuse protocol messages with storage; `mem/mem/protocol.go` defines messages, `mem/mem/storage.go` is the backing store.
- Cache variants (`writeback`, `writeevict`, `writethrough`, `writearound`) duplicate stage structure. A change in one builder or pipeline stage usually needs matching changes in the others, and any builder change must preserve the protocol wiring to lower and top ports.
- Do not assume one DRAM timing table fits all protocols; DDR3/4, GDDR5/5X/6, LPDDR4, and HBM have distinct burst-cycle and timing rules.
- Do not bypass page-table permissions in TLB/MMU/GMMU paths; use the provided translation components.

## TESTING
- Unit tests: `cd akita && ginkgo -r ./mem/...`.
- Acceptance: `cd akita/mem && python3 acceptance_test.py` builds and runs:
  - `idealmemcontroller`
  - `writebackcache`, `writeevictcache`, `writethroughcache`, `writearoundcache`
  - `dram`
  - `virtualmem`

Each program is tested serially and with `-parallel` at multiple address ranges.
