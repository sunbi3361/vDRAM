# MGPUSim UVM / Demand-Paging Implementation Specification

## 1. Objective

Extend MGPUSim with a configurable Unified Virtual Memory (UVM) model that supports:

1. `-uvm` to enable/disable UVM execution.
   - When UVM is enabled, benchmark memory allocations must use `AllocateManaged` rather than the normal `Allocate`.
2. A Tree-Based Neighborhood (TBN) prefetcher.
   - Base GPU page size remains 4 KB.
   - The minimum migration/prefetch granularity is 64 KB.
3. A fixed **20 us page-fault handling latency**.
4. An **Access Counter** mechanism operating at **64 KB granularity**.
5. `-ideal-uvm` mode.
   - Page-fault handling latency = 0.
   - PCIe/interconnect migration latency = 0.
   - Page faults, residency changes, evictions, migrations, prefetches, and Access Counter events must still execute functionally and be counted.
6. GPU-memory **oversubscription** with page eviction and re-migration.

The goal is to model the important runtime behavior of demand-paged UVM without making simulator runtime unnecessarily dependent on long host-side fault latencies.

---

# 2. High-Level Requirements

The implementation must preserve two separate concerns:

- **Functional UVM state**
  - residency
  - mappings
  - page faults
  - migration
  - eviction
  - prefetch
  - Access Counter state
  - remote mappings/accesses
- **Timing cost**
  - 20 us CPU/driver fault handling
  - page migration latency over the CPU-GPU interconnect

`-ideal-uvm` disables only the timing cost. It must **not** bypass the functional state machine.

The intended modes are:

| `-uvm` | `-ideal-uvm` | Behavior |
|---:|---:|---|
| 0 | 0 | Existing non-UVM behavior |
| 0 | 1 | Invalid configuration; reject or warn and disable `ideal-uvm` |
| 1 | 0 | Full UVM timing simulation |
| 1 | 1 | Functional UVM with zero fault/migration timing |

---

# 3. Modeling Assumptions

Unless the current MGPUSim implementation already requires otherwise, use the following hierarchy.

| Granularity | Size | Purpose |
|---|---:|---|
| Base page | 4 KB | Page table entry, basic residency/fault accounting |
| Access Counter region | 64 KB | Remote-access tracking and migration trigger |
| Minimum TBN fetch region | 64 KB | Minimum page-fault migration/prefetch unit |
| UVM VA block | 2 MB | Grouping scope for TBN, residency metadata, and victim management |

A 2 MB VA block therefore contains:

- 512 × 4 KB pages
- 32 × 64 KB regions

Do **not** silently change the simulator's architectural page size if the current VM implementation assumes 4 KB pages. The 64 KB unit is a UVM management/migration granularity layered above the 4 KB translation granularity.

---

# 4. Command-Line Flags

Add configuration flags through the normal MGPUSim runner/configuration path.

## 4.1 `-uvm`

Example:

```text
-uvm=true
```

Semantics:

- Enables demand-paged managed memory.
- Managed allocations initially have virtual address space and CPU backing, but are not assumed to be fully GPU-resident.
- GPU access to a non-resident page may generate a page fault.
- TBN prefetching, migration, eviction, oversubscription, and Access Counter behavior are enabled.
- Benchmark allocations that are intended to participate in UVM must use `AllocateManaged`.

When false:

- Preserve existing MGPUSim behavior.
- Existing applications using ordinary `Allocate` must continue to work unchanged.

## 4.2 `-ideal-uvm`

Example:

```text
-ideal-uvm=true
```

Valid only when `-uvm=true`.

Semantics:

```text
faultHandlingLatency = 0
migrationLatency      = 0
```

However, the following must still occur:

- fault detection
- fault coalescing
- TBN region selection
- eviction
- residency changes
- page-table/mapping updates
- migration request creation/completion
- Access Counter updates
- all UVM statistics

This mode is intended to measure the upper-bound performance of the same UVM placement/migration policy while preserving migration traffic and fault counts.

## 4.3 Recommended additional configuration knobs

Prefer making policy constants configurable even if default values are fixed for the paper experiments:

```text
-uvm-fault-latency-us=20
-uvm-access-counter-granularity=65536
-uvm-access-counter-threshold=<default>
-uvm-tbn-min-fetch-size=65536
-uvm-va-block-size=2097152
-uvm-oversubscription-ratio=<ratio>
```

If MGPUSim already has an equivalent memory-capacity option, reuse it instead of introducing a duplicate capacity mechanism.

---

# 5. Managed Allocation

## 5.1 API behavior

When `-uvm=true`, benchmark/application allocations that represent UVM-managed data must call:

```go
driver.AllocateManaged(...)
```

rather than:

```go
driver.Allocate(...)
```

Do not globally rewrite all allocator behavior internally such that an explicit ordinary allocation unexpectedly becomes managed memory. The runner/benchmark path should deliberately select the API according to the `-uvm` option.

Recommended pattern:

```go
if cfg.UVM {
    ptr = driver.AllocateManaged(ctx, size)
} else {
    ptr = driver.Allocate(ctx, size)
}
```

If the benchmark framework centralizes allocation through a helper, implement the switch in that helper to avoid modifying every benchmark independently.

## 5.2 Managed-allocation metadata

For each managed allocation, maintain enough metadata to determine:

- virtual range
- owner/context/PID
- page count
- CPU residency
- GPU residency
- current physical frame if GPU-resident
- dirty state if required by the current migration model
- VA block membership
- 64 KB region membership
- outstanding fault/migration state

Avoid creating a second independent source of truth if the current page table/page allocator already stores part of this state.

---

# 6. GPU Access Path

The desired GPU access state machine is:

```text
GPU memory request
        |
        v
   TLB / translation
        |
        v
 Is page GPU-resident?
      /     \
    yes      no
    |         |
    |         v
    |     Page fault
    |         |
    |         v
    |   Fault coalescing
    |         |
    |         v
    |   TBN region selection
    |         |
    |         v
    |   Ensure GPU capacity
    |         |
    |    eviction if needed
    |         |
    |         v
    |   20 us fault stage
    |         |
    |         v
    |      migration
    |         |
    |         v
    +---- request replay
```

For a GPU-resident page, the request follows the normal memory path.

For a CPU-resident page that has a valid remote mapping, the request may perform a remote access instead of faulting, depending on the Access Counter policy described below.

---

# 7. Fault Detection and Coalescing

## 7.1 Fault granularity

A fault is detected based on the accessed **4 KB page**.

Statistics must distinguish:

- raw faulting requests
- unique page faults
- coalesced requests waiting on an already outstanding fault

Example counters:

```text
uvm_fault_requests
uvm_unique_page_faults
uvm_coalesced_fault_requests
```

## 7.2 Duplicate fault handling

If page `P` already has an outstanding fault/migration:

```text
new access to P
    -> do not create another migration
    -> attach request to P's pending request list
    -> replay all waiting requests when migration completes
```

Use a structure conceptually equivalent to:

```go
map[PageID]*OutstandingFault
```

where each entry owns the waiting memory requests.

The implementation must not charge the fixed 20 us latency once per waiting warp/request.

---

# 8. Page-Fault Handling Latency

Model fixed host/driver handling latency as:

```text
20 us per unique fault-handling batch
```

Do not simply add 20 us to every memory request that touches a missing page.

## 8.1 Cycle conversion

Convert 20 us using the simulated GPU timing domain rather than hard-coding a cycle number:

```text
faultCycles = ceil(20e-6 * GPUCoreFrequencyHz)
```

If MGPUSim's event engine works directly in simulated time instead of cycles, schedule a 20 us delay directly.

## 8.2 Placement in the pipeline

Keep fault handling and migration logically separate:

```text
fault detected
    |
    +-- fault-handling delay: 20 us
    |
    +-- migration delay: size / effective link bandwidth
```

The fixed latency represents software/control overhead, not data-copy time.

## 8.3 `-ideal-uvm`

In ideal mode:

```text
faultHandlingDelay = 0
migrationDelay      = 0
```

but enqueue/complete the same logical operations through the same state machine where practical.

Avoid a separate "magic make-resident" implementation that bypasses counters or replacement logic.

---

# 9. TBN Prefetcher

Implement a Tree-Based Neighborhood prefetcher scoped to a 2 MB VA block.

## 9.1 Required minimum behavior

A fault to any 4 KB page must migrate **at least the containing 64 KB region**.

Thus:

```text
fault address
    -> align down to 64 KB
    -> select [base, base + 64 KB)
```

All resident pages already present in the selected region should be excluded from redundant transfer accounting.

## 9.2 Hierarchical regions

Represent each 2 MB VA block as a hierarchy:

```text
2 MB
├── 1 MB
│   ├── 512 KB
│   │   ├── ...
│   │   └── 64 KB
│   └── ...
└── 1 MB
    └── ...
```

The 64 KB regions are leaves.

The prefetcher may expand a selected region to a larger power-of-two neighborhood when the current TBN policy indicates sufficient spatial activity/residency.

Preferred region sizes:

```text
64 KB
128 KB
256 KB
512 KB
1 MB
2 MB
```

If an existing TBN implementation/reference is already present in the repository or branch, preserve its policy rather than inventing a new threshold.

If no TBN policy exists, implement the hierarchy cleanly and keep the expansion criterion configurable. The minimum correctness requirement is 64 KB fetch.

## 9.3 Transfer accounting

For every fault-triggered migration record:

```text
demanded 4 KB page
prefetched 4 KB pages
total migrated bytes
```

Track at least:

```text
uvm_demand_migrated_pages
uvm_prefetched_pages
uvm_migrated_pages
uvm_migrated_bytes
uvm_tbn_64kb_fetches
uvm_tbn_larger_fetches
```

A 64 KB transfer corresponds to up to 16 × 4 KB pages, excluding pages already GPU-resident.

---

# 10. Access Counter

Implement Access Counter tracking at **64 KB granularity**.

## 10.1 Purpose

The Access Counter monitors GPU accesses to data that is currently mapped remotely in CPU memory.

A sufficiently hot 64 KB CPU-resident region becomes a migration candidate.

This provides an alternative to immediately migrating every remotely accessed page.

## 10.2 Counter key

Conceptually:

```go
type AccessCounterKey struct {
    ContextID
    RegionBase // 64 KB aligned VA
}
```

For every remote GPU memory access:

```text
region = VA & ~(64KB - 1)
counter[region]++
```

Use whatever context/address-space identifier is already present in the VM subsystem.

## 10.3 Trigger

When the counter reaches the configured threshold:

```text
remote access
    -> increment 64 KB counter
    -> threshold reached?
       -> yes: create migration request for the 64 KB region
       -> no:  continue remote access
```

The exact threshold should be configurable.

Do not count accesses that hit GPU-resident pages.

## 10.4 Counter reset

At minimum, reset or invalidate the counter when:

- the 64 KB region migrates to GPU memory
- its ownership/mapping becomes invalid
- the managed allocation is freed

When a GPU-resident region is later evicted to CPU memory, initialize the remote access counter for the new residency epoch.

## 10.5 Access Counter statistics

Record:

```text
uvm_access_counter_remote_accesses
uvm_access_counter_notifications
uvm_access_counter_triggered_migrations
uvm_access_counter_resets
```

If feasible, also report the distribution of counter values at migration time.

---

# 11. Remote Access Semantics

The simulator must explicitly distinguish:

```text
GPU-local resident
CPU-resident but remotely accessible
migration in progress
invalid/unmapped
```

A remote mapping must not be treated as equivalent to GPU residency.

Recommended page state:

```go
type ResidencyState int

const (
    Unmapped ResidencyState = iota
    CPUResident
    GPUResident
    MigratingToGPU
    MigratingToCPU
)
```

If the current implementation already has equivalent fields/states, extend those rather than duplicating them.

Remote accesses should:

1. use CPU/host-memory/interconnect latency,
2. increment the 64 KB Access Counter,
3. potentially trigger migration when the threshold is reached.

---

# 12. Oversubscription

## 12.1 Capacity enforcement

When UVM is enabled, GPU physical memory capacity must be a hard limit.

Managed virtual allocation is allowed to exceed GPU capacity:

```text
managed allocation size > GPU physical memory
```

The simulator must not reject this allocation solely because it exceeds GPU memory.

Instead, only currently resident GPU pages consume GPU physical frames.

## 12.2 Allocation before migration

Before migrating a TBN-selected region:

```text
requiredFrames = number of non-resident 4 KB pages to migrate

if freeFrames < requiredFrames:
    evict enough GPU-resident pages
```

Do not over-allocate GPU physical frames.

## 12.3 Eviction policy

Use a simple deterministic baseline first, preferably:

```text
LRU at 64 KB region granularity
```

unless MGPUSim already provides a suitable UVM eviction policy.

Why 64 KB:

- matches Access Counter granularity
- matches minimum migration granularity
- simplifies residency and replacement accounting

However, maintain underlying 4 KB page mappings so address translation remains correct.

A 64 KB victim consists of up to 16 × 4 KB pages.

## 12.4 Pages not eligible for eviction

Never choose:

- a region currently being migrated
- a region with unresolved fault waiters
- pinned/non-managed allocations
- reserved page-table/system pages
- the destination region of the active migration

## 12.5 Eviction state transition

Conceptually:

```text
GPUResident
    |
    | select victim
    v
MigratingToCPU
    |
    | copy/writeback if necessary
    v
CPUResident
```

Update mappings/TLB-visible state using the existing MGPUSim invalidation/update mechanism.

If dirty-page tracking is not currently modeled, clearly document whether all evictions conservatively incur transfer traffic.

## 12.6 Oversubscription statistics

Track:

```text
uvm_evictions
uvm_evicted_pages
uvm_evicted_bytes
uvm_gpu_resident_pages_peak
uvm_gpu_resident_bytes_peak
uvm_oversubscription_ratio
uvm_repeated_migrations
```

`uvm_repeated_migrations` should count pages/regions that migrate to GPU more than once and is useful for detecting thrashing.

---

# 13. Migration Timing

Do not model migration as the fixed 20 us fault latency.

Use:

```text
migration latency = transfer size / effective CPU-GPU bandwidth
```

plus any existing interconnect queuing/contention already modeled by MGPUSim.

Prefer reusing the current DMA/copy engine/interconnect implementation.

Do not create a second independent PCIe timing model if the simulator already models host-device copies.

The desired split is:

```text
Total fault service time
    =
fault handling
    +
eviction traffic, if required
    +
CPU -> GPU migration
    +
queueing/contention
```

In `-ideal-uvm`:

```text
all migration copies complete with zero timing latency
```

but transferred bytes and migration counts remain unchanged.

---

# 14. Recommended Event Model

Do not block the entire simulator for 20 us.

Model fault handling as asynchronous scheduled work.

Suggested objects:

```go
type PageFault struct {
    PageVA
    VABlockBase
    ContextID
    WaitingRequests
    CreatedAt
}

type Migration struct {
    Direction
    Pages
    Bytes
    DemandPages
    PrefetchPages
    Trigger // Fault, AccessCounter, etc.
}
```

Suggested pipeline:

```text
FaultDetected
    |
    v
FaultHandlingQueue
    | 20 us
    v
TBNSelect
    |
    v
CapacityCheck
    |
    +--> EvictionQueue
    |
    v
MigrationQueue
    |
    v
MappingUpdate
    |
    v
ReplayWaitingRequests
```

The implementation does not need these exact Go type names. Fit the design into the existing Akita/MGPUSim component/message/event architecture.

---

# 15. Likely Code Areas to Inspect

Before changing code, inspect the current implementation and identify the actual dataflow.

Likely relevant locations include:

```text
mgpusim/amd/driver/
mgpusim/amd/driver/internal/memoryallocator.go
mgpusim/amd/driver/pagefault.go
mgpusim/amd/protocol/
mgpusim/amd/samples/runner/
mgpusim/amd/samples/runner/timingconfig/
akita/mem/vm/
akita/mem/vm/tlb/
akita/mem/vm/addresstranslator/
```

The exact implementation may have moved. Search for:

```text
AllocateManaged
Allocate
PageFault
PageMigration
MemoryAllocator
PageTable
TLB
Migration
Remote
UnifiedMemory
AccessCounter
```

Do not modify APIs until the current ownership of each responsibility is understood.

---

# 16. Implementation Order

Implement in the following order so each stage is testable.

## Phase 1 — Flags and managed allocation

- Add `-uvm`.
- Add `-ideal-uvm`.
- Route benchmark allocation through `AllocateManaged` only when UVM is enabled.
- Confirm legacy non-UVM runs are bit-for-bit or statistically unchanged.

## Phase 2 — Residency and basic page faults

- Track CPU/GPU residency.
- Generate fault on non-resident 4 KB GPU access.
- Coalesce duplicate faults.
- Migrate the demanded region.
- Replay waiting requests.

Initially, TBN may always select exactly one 64 KB leaf.

## Phase 3 — 20 us timing

- Add asynchronous fixed fault-handling delay.
- Verify exactly one handling delay is charged per fault batch, not per waiting request.
- Add zero-latency override for `-ideal-uvm`.

## Phase 4 — TBN

- Add 2 MB VA-block hierarchy.
- Minimum fetch = 64 KB.
- Add larger neighborhood expansion.
- Add demand vs prefetch statistics.

## Phase 5 — Oversubscription

- Enforce finite GPU capacity.
- Add victim selection.
- Add GPU -> CPU eviction.
- Support repeated CPU -> GPU migration.

## Phase 6 — Access Counter

- Track remote GPU accesses at 64 KB.
- Trigger migration at threshold.
- Reset state across migration epochs.

## Phase 7 — validation and instrumentation

- Add assertions.
- Add deterministic microbenchmarks.
- Add end-of-run UVM statistics.

---

# 17. Required Statistics

Expose the following in normal simulator output or a clearly identifiable statistics block.

```text
UVMEnabled
IdealUVM

PageFaultRequests
UniquePageFaults
CoalescedFaultRequests

TBNFetches
TBN64KBFetches
TBNLargerFetches
DemandMigratedPages
PrefetchedPages

CPUToGPUMigrations
GPUToCPUMigrations
MigratedPages
MigratedBytes

Evictions
EvictedPages
EvictedBytes
RepeatedMigrations

RemoteAccesses
AccessCounterNotifications
AccessCounterTriggeredMigrations

PeakGPUResidentPages
PeakGPUResidentBytes

FaultHandlingTime
MigrationTime
EvictionTime
```

In ideal mode:

```text
FaultHandlingTime == 0
MigrationTime      == 0
```

while counts and bytes must remain meaningful.

---

# 18. Correctness Invariants

Add assertions or equivalent checks for the following.

## Residency

A 4 KB page must never simultaneously own two different GPU physical frames.

```text
GPU-resident page -> valid GPU frame exists
CPU-only page     -> no allocated GPU frame
```

## Capacity

At all times:

```text
residentGPUBytes <= configuredGPUCapacity
```

## Fault coalescing

At most one active CPU -> GPU migration may exist for the same 4 KB page.

## TBN

Every fault-triggered transfer contains the demanded 4 KB page.

Minimum selected TBN region:

```text
>= 64 KB
```

and is aligned to 64 KB.

## Access Counter

All Access Counter keys are 64 KB aligned.

## Ideal UVM

Changing:

```text
-ideal-uvm=false
```

to:

```text
-ideal-uvm=true
```

must not change, for a deterministic workload:

- unique fault count
- migration count
- migration bytes
- prefetch selection
- eviction decisions

unless timing itself intentionally affects application ordering.

The expected primary difference is simulated execution time.

---

# 19. Required Microbenchmarks

Create small deterministic tests before running large applications.

## Test A — Single first touch

Allocation:

```text
128 KB managed
```

Access one address.

Expected:

```text
1 unique page fault
>= 64 KB CPU->GPU migration
no eviction
```

With ordinary UVM:

```text
~20 us fixed fault-handling cost + migration cost
```

With ideal UVM:

```text
same fault/migration counters
zero fixed fault/migration timing
```

## Test B — Same-page fault coalescing

Generate many concurrent requests to one non-resident 4 KB page.

Expected:

```text
many fault requests
1 unique outstanding fault
1 TBN migration
1 fixed 20 us handling delay
```

## Test C — 64 KB alignment

Access:

```text
VA = region_base + arbitrary_offset
```

Expected migrated region contains:

```text
alignDown(VA, 64 KB)
```

## Test D — Two 64 KB regions

Touch two addresses in different 64 KB leaves of the same 2 MB VA block.

Verify TBN behavior and statistics.

## Test E — Oversubscription

Example:

```text
GPU capacity = 128 MB
managed working set = 192 MB
```

Sequentially touch the full working set.

Expected:

```text
allocation succeeds
GPU residency never exceeds 128 MB
evictions occur
```

## Test F — Thrashing/reuse

Alternate accesses between two working sets whose combined footprint exceeds GPU capacity.

Expected:

```text
repeated migrations > 0
evictions > 0
```

## Test G — Access Counter

Keep a 64 KB region CPU-resident but remotely accessible.

Issue remote GPU accesses.

Expected:

```text
counter increments only for that 64 KB region
threshold event occurs
one Access-Counter-triggered migration
counter resets after migration
```

---

# 20. Performance Requirements for the Simulator

Avoid implementation choices that unnecessarily make UVM simulations unusably slow.

In particular:

- Do not advance 20 us one GPU cycle at a time.
- Use Akita scheduled events/time jumps.
- Coalesce duplicate page faults.
- Batch migration at TBN granularity.
- Do not create per-byte transfer events.
- Prefer one migration transaction with a page list/range.
- Keep residency lookup O(1) or close to O(1).
- Avoid scanning all GPU-resident pages on every memory access.
- Maintain an explicit replacement structure for eviction.

For evaluation workloads, the simulator should spend work proportional to:

```text
memory requests + faults + migration regions
```

rather than:

```text
20 us * GPU frequency * number of faults
```

in host execution time.

---

# 21. Non-Goals

Do not add unrelated UVM optimizations in this task.

Unless already required by MGPUSim, do not implement:

- NVIDIA-specific proprietary fault-buffer packet formats
- exact kernel-driver interrupt timing
- exact Linux UVM lock contention
- multi-GPU UVM coherence
- ATS/HMM/IOMMU redesign
- speculative migration policies unrelated to TBN
- ARIADNE-specific Sharing Degree/WCSS policies
- page-size promotion

The target is a clean research-grade demand-paging model with the six required mechanisms.

---

# 22. Agent Workflow

Before editing:

1. Trace the existing allocation path from benchmark to physical page allocation.
2. Trace the current TLB miss/page-fault path.
3. Identify whether current MGPUSim already has:
   - managed allocations
   - remote mappings
   - page migration messages
   - a PageMigrationController
   - DMA/copy timing
   - eviction support
4. Produce a short implementation map showing which components will be modified.

While implementing:

- Keep commits/changes logically separated by the phases above.
- Reuse existing messages/components whenever possible.
- Avoid large refactors unrelated to UVM.
- Add unit tests or small executable tests for each new mechanism.
- Run `go test` on every modified package.
- Run representative existing non-UVM benchmarks to check regressions.

After implementing, report:

```text
1. Modified files
2. New structs/messages
3. Exact state machine
4. Flag semantics
5. Fault latency implementation
6. TBN algorithm
7. Access Counter algorithm
8. Oversubscription/eviction policy
9. Statistics added
10. Tests executed and results
11. Known limitations
```

---

# 23. Acceptance Criteria

The implementation is complete only if all of the following hold.

- [ ] `-uvm=false` preserves legacy allocation/execution.
- [ ] `-uvm=true` uses `AllocateManaged` for managed benchmark allocations.
- [ ] Non-resident GPU access produces a demand page fault.
- [ ] Duplicate accesses to an outstanding page fault are coalesced.
- [ ] Fault handling incurs a fixed 20 us delay in normal UVM mode.
- [ ] The fixed delay is not charged separately to every waiting request.
- [ ] TBN migrates at least one aligned 64 KB region per demand fault.
- [ ] TBN can represent larger hierarchical neighborhoods within a 2 MB VA block.
- [ ] Access Counter state is maintained at 64 KB granularity.
- [ ] Access Counter can trigger CPU->GPU migration.
- [ ] GPU memory capacity is strictly enforced.
- [ ] Managed allocations can exceed GPU physical memory.
- [ ] Eviction makes room for new migrations.
- [ ] Evicted pages can later migrate back to GPU.
- [ ] `-ideal-uvm` sets fault and migration timing to zero.
- [ ] `-ideal-uvm` still reports page faults, migrations, prefetches, and evictions.
- [ ] UVM statistics are printed at the end of execution.
- [ ] Non-UVM regression tests pass.
- [ ] Dedicated UVM microbenchmarks pass.
- [ ] No cycle-by-cycle busy waiting is used to emulate the 20 us host delay.

---

# 24. Important Design Principle

Treat UVM as a **stateful memory-management mechanism**, not simply as an extra latency attached to a TLB miss.

The functional sequence:

```text
fault
-> residency decision
-> TBN selection
-> capacity check
-> eviction
-> migration
-> mapping update
-> replay
```

must exist in both normal and ideal modes.

Only timing changes:

```text
Normal UVM:
    functional UVM + 20 us fault cost + migration/interconnect cost

Ideal UVM:
    identical functional UVM + zero fault cost + zero migration cost
```

This distinction is required so that `-ideal-uvm` remains useful as a controlled experimental upper bound while preserving the same number of faults, migrations, and capacity-management decisions.
