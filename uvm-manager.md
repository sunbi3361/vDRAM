# MGPUSim v4 UVM Driver Specification (Draft v0.10)

## 1. Purpose

This document specifies a Unified Virtual Memory (UVM) driver model for **MGPUSim v4**.

The implementation targets a system in which:

- MGPUSim already has a GPU-side **GMMU**.
- The UVM implementation is added to the existing `driver` package.
- GPU–driver control traffic is modeled through the GPU **Command Processor (CP)** and **PCIe**.
- GPU memory can be oversubscribed.
- Managed memory uses demand paging with optional prefetching and access-counter-based remote access/migration.
- The basic migration and residency-management unit is a **2 MB VA Block**, while faulting, prefetching, access counting, and migration may operate at finer sub-block granularity.

The initial feature set is:

1. Demand paging
2. Tree-Based Neighborhood (TBN) prefetching
3. Access Counter based remote access / migration
4. Oversubscription with LRU eviction
5. 2 MB VA Block management

This is a behavioral and architectural specification first. Exact MGPUSim file names and component APIs may be refined during implementation.

---


## 1.1 Timing-Modeling Principle

The UVM model uses an **event-based timing model**.

UVM software overheads are represented by scheduled simulator events rather than cycle-by-cycle execution of CPU-side driver code.

The page-fault service time is decomposed into independent latency components:

```text
Fault service latency
=
GPU -> Driver control-path latency
+ fixed UVM fault-handling latency
+ eviction latency, if required
+ migration DMA / PCIe latency
+ page-table update / 64 KB range-invalidation latency
+ fault-replay control latency
```

The fixed software fault-handling latency must **not** absorb PCIe migration time.

This follows the modeling direction previously discussed from UVMSmart/HMS:

- software/page-fault handling overhead is modeled explicitly,
- PCIe data movement is modeled separately as a size-dependent transfer,
- duplicate faults are merged before service,
- simulator time advances through discrete events rather than emulating driver instructions.

---

## 1.2 Experimental Modes

Two primary modes are required:

```text
Normal UVM:
    -uvm

Ideal UVM:
    -uvm -uvm-ideal
```

Both modes execute the **same UVM functional state machine**.

`-uvm-ideal` changes only timing, not behavior or statistics.

In ideal mode, **all UVM-specific control-path latency is also zeroed**, including:

```text
UVMDriver <-> CP control-message latency
PTE update latency
64 KB cache range writeback/invalidate control latency
64 KB TLB range invalidation latency
fault-replay control latency
```

Functional ordering constraints are still preserved; only elapsed simulator time for these control operations becomes zero.


In particular, ideal mode still performs logically:

```text
page-fault detection
fault coalescing
TBN region selection
residency update
migration accounting
oversubscription detection
LRU victim selection
eviction accounting
PTE state transition
TLB-state transition
fault completion / replay
```

but makes the following latency components zero:

```text
fixed UVM fault-handling latency = 0
PCIe remote-access latency       = 0
PCIe migration-transfer latency  = 0
```

The model must still count the amount of logically transferred data.

Therefore `-uvm-ideal` is intended to measure the **intrinsic number of UVM events and data movement decisions without PCIe/fault timing penalty**, rather than bypassing UVM.

At minimum, the following statistics must remain identical in definition between normal and ideal modes:

```text
page fault requests
unique/coalesced fault services
CPU -> GPU migration count
GPU -> CPU migration count
migration bytes
TBN-prefetched bytes
Access Counter migrations
eviction count
eviction bytes
```

# 2. High-Level Architecture

## 2.1 Components

The UVM model consists of the following logical components.

### UVMDriver

Implemented inside the existing MGPUSim `driver` package.

Responsibilities:

- Managed virtual-memory allocation
- Managed-page metadata
- 2 MB VA Block creation and tracking
- GPU residency tracking
- Page-fault handling
- TBN prefetch decisions
- Access-counter handling
- GPU page migration
- Oversubscription detection
- LRU victim selection
- Eviction to host memory
- GPU page-table updates
- Remote PTE installation
- TLB range invalidation requests
- Migration/fault statistics

### GPU GMMU

Already implemented.

Responsibilities:

- GPU-side address translation
- Page-table walk
- Detection of non-resident / invalid GPU mappings
- Generation of page-fault requests
- Use of local-GPU and remote mappings
- Interaction with GPU TLB hierarchy

### GPU Command Processor (CP)

Acts as the GPU-side control endpoint for UVM operations.

Responsibilities:

- Receive page-fault notifications generated inside the GPU
- Forward page-fault requests to the UVMDriver through PCIe
- Receive UVM control commands from the driver
- Forward migration requests to the DMA Engine
- Forward UVM-specific range invalidation commands to the translation/cache hierarchy
- Trigger replay of serviced replayable faults

UVM must **not** reuse the existing heavyweight GPU-wide `ShootDownCommand` / `GPURestartReq` path for normal page migration.

Normal UVM migration must not flush CU pipelines or globally stop/restart the GPU.

### PCIe

Models CPU/GPU communication and remote memory transport.

PCIe is used for:

- GPU -> Driver page-fault requests
- Driver -> GPU fault responses
- Driver -> GPU range TLB invalidation requests
- Driver -> GPU range cache writeback/invalidate requests for eviction
- Driver -> GPU page-table/mapping control messages
- Driver -> GPU fault replay requests
- GPU remote memory accesses to host-resident managed pages
- Page migration traffic between CPU memory and GPU memory
- Page eviction traffic from GPU memory to CPU memory

---

# 3. Managed Memory Allocation

## 3.1 Allocation API

Managed memory uses the existing driver-visible managed allocation API:

```go
AllocateManaged(pid, byteSize)
```

The existing non-UVM allocation APIs and their benchmark-specific calling conventions remain unchanged.

MGPUSim v4 benchmarks do not share one uniform allocation wrapper/API shape. Therefore the UVM integration must be performed **at each benchmark's existing allocation call sites**.

Required behavior:

```text
-uvm disabled:
    keep the benchmark's existing allocation call exactly as before

-uvm enabled:
    replace/select that allocation with AllocateManaged(...)
```

The implementation must not introduce a synthetic common `Runner.allocateMemory()` abstraction merely for UVM unless such an abstraction already exists in that benchmark.

Example:

```text
-uvm
```

When UVM is disabled:

- benchmark code calls the existing `Allocate(...)` path,
- existing MGPUSim allocation behavior is used,
- no UVM page fault, migration, Access Counter, TBN, or oversubscription behavior is introduced.

When UVM is enabled:

- benchmark code calls `AllocateManaged(...)`,
- each managed allocation is registered with the UVMDriver,
- CPU backing pages are created initially,
- GPU physical pages are allocated lazily,
- the allocation is partitioned into 2 MB VA Blocks,
- GPU PTE state is initialized according to the selected UVM mode.

Conceptual benchmark pattern:

```go
var ptr uint64

if *uvm {
    ptr = driver.AllocateManaged(pid, size)
} else {
    ptr = driver.Allocate(pid, size)
}
```

If MGPUSim uses benchmark-specific allocator wrappers rather than calling the driver directly, the same `-uvm` switch must be implemented in that wrapper.

---


## 3.2 Benchmark Integration Requirement

UVM support is added **benchmark-by-benchmark**.

For every allocation in a benchmark that represents memory intended to participate in UVM, the benchmark must explicitly select the allocation API based on the `-uvm` flag.

Conceptually:

```go
if useUVM {
    ptr = driver.AllocateManaged(pid, size)
} else {
    ptr = <the benchmark's original allocation call>
}
```

The `<the benchmark's original allocation call>` is intentionally unspecified because MGPUSim v4 benchmarks may use different allocation APIs, helper functions, unified-memory helpers, device-selection arguments, or runner-specific conventions.

Implementation rules:

- preserve the benchmark's current allocation path when `-uvm` is disabled,
- use `AllocateManaged` only for buffers intended to be managed when `-uvm` is enabled,
- modify each relevant benchmark directly,
- do not require a repository-wide common allocation helper,
- do not change unrelated allocation semantics merely to support UVM,
- keep benchmark-specific device/GPU selection behavior intact where applicable.

Typical modification pattern:

```go
// Existing benchmark-specific code
ptr := <existing allocation expression>

// Becomes conceptually
if useUVM {
    ptr = driver.AllocateManaged(pid, size)
} else {
    ptr = <existing allocation expression>
}
```

The actual Go form must follow the APIs already used by that benchmark.

### Scope

This applies to:

```text
benchmark input buffers
benchmark output buffers
intermediate/application buffers
other allocations whose GPU accesses should participate in UVM
```

Driver-internal allocations, page-table storage, command buffers, and other simulator infrastructure allocations must not be converted to `AllocateManaged` unless explicitly required.

### Invariant

```text
-uvm enabled
=> every benchmark buffer intended to participate in UVM
   was allocated through AllocateManaged
```

The UVMDriver may assert or report an error if a managed-memory page fault targets a virtual address that is not registered as a managed allocation.

### Required Benchmark Coverage

The first implementation must audit **all application-buffer allocation sites under `amd/benchmarks/**` that can be exercised by the timing-mode samples/runners used for evaluation**.

At each such allocation site:

```text
if -uvm:
    AllocateManaged(...)
else:
    preserve the benchmark's original allocation call
```

The implementation must also propagate the `-uvm` flag from the relevant sample/runner into each benchmark that it launches.

No repository-wide allocation wrapper is required.

Acceptance requirement:

```text
For every benchmark enabled for UVM evaluation,
all GPU-accessed application buffers are registered managed allocations
when -uvm is enabled.
```

Benchmarks not included in the timing-mode evaluation build do not need to be refactored merely for code-style uniformity.

---

# 4. Address Granularity

The UVM model uses the following fixed hierarchy.

| Concept | Granularity |
|---|---:|
| Base page | **4 KB** |
| GPU page fault identity | **4 KB page** |
| TBN minimum prefetch / fetch node | **64 KB = 16 base pages** |
| VA Block | **2 MB = 512 base pages = 32 TBN leaf nodes** |
| Remote GPU memory request | GPU cache-line size |
| Access Counter accounting region | **64 KB** |
| Eviction unit | **64 KB = 16 base pages** |

The implementation must not conflate the 4 KB base page with the TBN migration granularity.

A GPU fault is identified at 4 KB page granularity, but when the TBN prefetcher is active, servicing that fault selects at least the containing 64 KB TBN leaf node.

Therefore, on a cold 4 KB fault:

```text
1 x 4 KB fault
    ->
1 x 64 KB TBN leaf selected
    ->
16 x 4 KB pages become migration candidates
```

Already-resident pages inside the selected 64 KB range must not be transferred again. Thus, "64 KB migration" means that the prefetch/migration mask covers the entire 64 KB node, while the actual copy operation may skip pages already resident at the destination.

---

## 4.1 2 MB VA Block

The UVMDriver follows the NVIDIA UVM-style **2 MB VA Block** organization.

Each VA Block contains:

```text
2 MB / 4 KB = 512 base pages
2 MB / 64 KB = 32 minimum TBN nodes
```

Conceptually:

```text
2 MB VA Block
├── 64 KB node 0  -> pages   0..15
├── 64 KB node 1  -> pages  16..31
├── ...
└── 64 KB node 31 -> pages 496..511
```

The VA Block is the maximum region considered by the TBN prefetch logic in the initial MGPUSim implementation.

TBN must not expand across a 2 MB VA Block boundary.

---

## 4.2 Per-4KB Page State

Residency and mapping state remain **4 KB-granular** even though TBN fetch decisions begin at 64 KB.

Each of the 512 base pages in a VA Block independently tracks at least:

```text
residency
GPU physical page
CPU physical page
dirty state
remote-mapped state
migration state
last-access metadata
```

This is required because:

- page faults are 4 KB-granular,
- GPU PTEs are modeled at 4 KB granularity,
- a 64 KB TBN node can be partially resident,
- a prefetch operation must skip already-resident 4 KB pages,
- future eviction/access-counter policies may require finer state than the TBN node.

---

## 4.3 TBN Tree Geometry

For this MGPUSim model, the **minimum logical TBN node is 64 KB**.

The tree for a full 2 MB VA Block is:

```text
                         2 MB
                    /             \
                 1 MB             1 MB
               /     \           /     \
            512 KB  512 KB    512 KB  512 KB
             ...      ...       ...      ...
             128 KB regions
              /    \
           64 KB  64 KB
```

Tree levels are therefore:

```text
64 KB
128 KB
256 KB
512 KB
1 MB
2 MB
```

There are 32 leaf nodes per full VA Block.

The tree is used to determine how far a fault-triggered migration may expand. Physical residency is still tracked using 4 KB page masks.

---

# 5. Core Metadata

## 5.1 Managed Allocation

Conceptual structure:

```go
type ManagedAllocation struct {
    PID        vm.PID
    VAddr      uint64
    Size       uint64
    VABlocks   []*VABlock
}
```

## 5.2 VA Block

Conceptual structure:

```go
type VABlock struct {
    StartVA uint64
    Size    uint64 // 2 MB

    SubBlocks []SubBlockState

    LastAccessTime sim.VTimeInSec

    // Optional aggregate metadata
    ResidentBytesGPU uint64
    ResidentBytesCPU uint64
}
```

## 5.3 Sub-Block State

Conceptual structure:

```go
type SubBlockState struct {
    VA uint64

    Residency ResidencyState

    GPUPhysicalAddress uint64
    CPUPhysicalAddress uint64

    DirtyOnGPU bool
    DirtyOnCPU bool

    RemoteMapped bool

    AccessCounter uint64

    LastAccessTime sim.VTimeInSec

    State MigrationState
}
```

Possible residency states:

```text
CPU_RESIDENT
GPU_RESIDENT
MIGRATING_CPU_TO_GPU
MIGRATING_GPU_TO_CPU
REMOTE_ACCESSIBLE
INVALID
```

---

# 6. GPU Page Table Semantics

The GPU page table must distinguish at least:

1. GPU-local mapping
2. CPU-remote mapping
3. non-resident / invalid mapping

Conceptually:

```text
GPU_LOCAL:
    VA -> GPU PA

REMOTE:
    VA -> CPU PA reachable over PCIe

INVALID:
    triggers GPU page fault
```

## 6.1 Remote PTE

When Access Counter mode is enabled, a CPU-resident managed page may have a valid remote GPU PTE.

A remote PTE:

- points to CPU physical memory
- permits GPU access over PCIe
- can be cached in the GPU **L2 TLB**
- does not imply migration

The L2 TLB therefore may cache both:

```text
VA -> GPU-local PA
VA -> CPU-remote PA
```

The TLB entry must preserve enough information to distinguish local and remote mappings.


Invalid/non-resident translations are **not negative-cached** in the GPU TLB hierarchy.

Therefore:

```text
INVALID PTE
 -> page fault
 -> no INVALID entry inserted into L1/L2 TLB
```

This makes `INVALID -> GPU_LOCAL` installation replay-safe without requiring a TLB invalidate. `REMOTE -> GPU_LOCAL` still requires a 64 KB range invalidation because a valid remote translation may be cached.

Suggested conceptual field:

```go
type Translation struct {
    PAddr    uint64
    Location MemoryLocation // GPU / CPU_REMOTE
}
```

### Required Behavior

A remote PTE cached in the L2 TLB must not bypass Access Counter accounting.

Access Counter observation therefore occurs on the **memory-access path after translation**, not only on GMMU page-table walks.

---

# 7. Demand Paging

## 7.1 Initial State

For a newly allocated managed region:

- CPU backing storage exists.
- No GPU-local physical page is allocated initially.
- GPU mapping is either:
  - INVALID, when Access Counter remote access is disabled
  - REMOTE, when Access Counter remote access is enabled

---

# 8. Page Fault Path

## 8.1 Fault Trigger

A page fault occurs when a GPU memory request reaches translation and the requested VA does not have a usable GPU mapping.

Typical case:

```text
GPU access
 -> L1/L2 TLB miss
 -> GMMU page-table walk
 -> PTE invalid / non-resident
 -> page fault
```

## 8.2 Fault Request Flow

Required control path:

```text
CU / memory request
    |
    v
GMMU detects page fault
    |
    v
GPU CP
    |
    | PCIe
    v
UVMDriver
```

The page-fault request must contain at least:

```go
type PageFaultReq struct {
    PID        vm.PID
    GPU        int
    VAddr      uint64
    AccessType AccessType // Read / Write
    SourceCU   int        // optional
}
```

## 8.3 Fault Coalescing

Multiple outstanding GPU requests may fault on the same migration unit.

The driver should coalesce faults for the same:

```text
(PID, GPU, migration-region)
```

Only one migration should be issued.

Other requests wait on the same fault transaction.


The coalescing key for the initial implementation is the aligned **64 KB fault-service region**:

```text
faultRegionBase = alignDown(faultVA, 64 KB)

key = (PID, GPU, faultRegionBase)
```

All 4 KB faults that target the same 64 KB region while a transaction is pending join that transaction.

A transaction remains coalescible through:

```text
FAULT_PENDING
FAULT_HANDLING
MIGRATING_TO_GPU
PTE_UPDATE
SHOOTDOWN
```

and leaves the coalescing table only after the region becomes replayable.

This avoids repeated software latency and duplicate DMA requests for the same fault region.


## 8.4 Fault-Service Serialization

The UVMDriver services **exactly one 64 KB fault-service region at a time**.

Different 64 KB regions are not batched and do not overlap in the initial implementation.

Conceptually:

```text
Fault Service Queue
    |
    +--> 64 KB region A  <- ACTIVE
    |
    +--> 64 KB region B  <- WAITING
    |
    +--> 64 KB region C  <- WAITING
```

Region B cannot begin its software fault-handling delay, TBN decision, DMA migration, PTE update, or shootdown sequence until region A's fault transaction is complete and replayable.

The scheduling policy is initially FIFO by transaction creation time.

Therefore:

```text
max_active_fault_service_transactions = 1
```

Duplicate 4 KB faults targeting the currently active or queued 64 KB region are still coalesced into that region's existing transaction.

This serialization is specific to **fault-service transactions**. Other non-fault DMA operations should not be assumed serialized unless explicitly specified elsewhere.

### Required Statistic

Track:

```text
raw_page_fault_count
coalesced_page_fault_count
unique_fault_service_count
```

---

# 9. Demand-Fault Handling State Machine

For a CPU-resident page accessed without a usable remote mapping:

```text
FAULT_RECEIVED
    |
    v
COALESCE_FAULTS
    |
    v
SELECT_FETCH_REGION
    |
    v
CHECK_GPU_CAPACITY
    |
    +--> enough capacity --------------------+
    |                                        |
    +--> insufficient capacity               |
             |                               |
             v                               |
         EVICT_LRU                           |
             |                               |
             +-------------------------------+
                                             |
                                             v
                                  MIGRATE_CPU_TO_GPU
                                             |
                                             v
                                   UPDATE_GPU_PTE
                                             |
                                             v
                                    TLB_SHOOTDOWN
                                             |
                                             v
                                    COMPLETE_FAULT
                                             |
                                             v
                                     REPLAY_REQUEST
```

---

# 10. Fault Service Latency

The fixed UVM software/page-fault handling latency is:

```text
default = 20 us
```

Configuration:

```text
-uvm-fault-handling-latency=20us
```

This latency represents driver/software fault-management overhead.

It is explicitly separate from:

- GPU -> Driver PCIe/control-message latency,
- CPU -> GPU migration DMA latency,
- GPU -> CPU eviction DMA latency,
- PCIe bandwidth serialization,
- page-table update latency,
- TLB/cache shootdown latency,
- GPU restart/replay latency.

## 10.1 Charging Rule

The 20 us latency is charged **once per unique fault-service transaction after duplicate-fault coalescing**.

It is **not** charged:

- once per individual 4 KB page,
- once for every faulting wavefront/request that maps to the same service transaction,
- once per each of the 16 pages in the mandatory 64 KB TBN leaf.

Required sequence:

```text
raw fault requests
    |
    v
coalesce duplicate faults
    |
    v
create unique fault-service transaction
    |
    v
schedule +20 us UVM fault-handling event
    |
    v
TBN / capacity / migration service
```

A service transaction is keyed by the region currently being fault-serviced, initially:

```text
(PID, GPU, 64KB fault region)
```

If several raw 4 KB faults within the same 64 KB region arrive while that region already has a pending fault transaction, they are attached to the same transaction and do not incur another 20 us software delay.

## 10.2 Interaction with TBN

TBN may expand a fault-triggered 64 KB region to a larger region inside the same 2 MB VA Block.

The expansion does **not** create a new 20 us software fault charge for every newly prefetched 64 KB node.

Conceptually:

```text
one unique demand-fault service
    + 20 us software overhead
    + TBN expands migration mask
    + DMA transfers all missing selected pages
```

The extra TBN pages contribute additional migration bytes and therefore additional DMA/PCIe transfer time, but not additional software fault-handling latency.

Independent demand faults which are not coalesced into the same service transaction each incur their own 20 us latency.

Because only one 64 KB fault-service transaction is active at a time, these 20 us software delays **do not overlap** between different fault regions in the initial model.

## 10.3 Event-Based Implementation

The 20 us delay should be implemented as a scheduled simulation event.

Example conceptual flow:

```go
func (d *UVMDriver) startFaultService(txn *FaultTransaction) {
    txn.State = FaultHandling

    delay := d.faultHandlingLatency

    if d.idealUVM {
        delay = 0
    }

    d.engine.Schedule(
        FaultHandlingCompleteEvent{
            Transaction: txn,
        },
        now + delay,
    )
}
```

No CPU instruction-level simulation or busy waiting is required.

## 10.4 Ideal-UVM Behavior

Under:

```text
-uvm-ideal
```

the fault transaction still exists and is coalesced/accounted normally, but:

```text
faultHandlingLatency = 0
```

The transaction immediately proceeds to the next functional stage.

Fault counters must not be suppressed.

---

# 11. TBN Prefetcher

## 11.1 Fixed Parameters

The TBN prefetcher follows the NVIDIA UVM-style hierarchical neighborhood policy with the following fixed parameters:

```text
Base page                   = 4 KB
Minimum TBN node            = 64 KB
Pages per minimum node      = 16
Maximum TBN region          = 2 MB VA Block
Default expansion threshold = 51
Threshold comparison        = strictly greater than 51%
```

The candidate hierarchy is:

```text
64 KB
 -> 128 KB
 -> 256 KB
 -> 512 KB
 -> 1 MB
 -> 2 MB
```

---

## 11.2 Mandatory 64 KB Fault Expansion

A GPU fault is originally identified at 4 KB granularity.

For TBN occupancy calculation, the 4 KB fault is expanded to the containing aligned 64 KB leaf node.

Example:

```text
faultVA = X
leafBase = alignDown(X, 64KB)

CurrentFaultExpanded64KBMask =
    all valid 4 KB pages in [leafBase, leafBase + 64KB)
```

Thus:

```text
1 x 4 KB demand fault
    ->
16 x 4 KB pages marked occupied in the TBN tree
```

for a full 64 KB leaf.

This occupancy expansion does not mean that all 16 pages are classified as demand-fault pages. It only defines the TBN tree state used to choose the prefetch extent.

---

## 11.3 NVIDIA-Style TBN Occupancy Mask

The TBN occupancy mask is fixed as:

```text
TBNOccupancyMask
=
GPUResidentMask
OR
CurrentFaultExpanded64KBMask
```

where:

- `GPUResidentMask` contains 4 KB pages already resident on the destination GPU,
- `CurrentFaultExpanded64KBMask` contains the 64 KB leaf associated with the currently serviced fault.

The following masks are **not** included in the 51% occupancy numerator:

```text
PrefetchInFlightMask
MigratingToGPUMask
```

These masks are used only later to suppress duplicate migration requests.

The initial implementation does not model NVIDIA thrashing detection, therefore:

```text
ThrashingMask = empty
```

and no TBN pages are excluded for thrashing in v0.8.

---

## 11.4 51% Expansion Rule

For each ancestor node containing the current 64 KB fault leaf:

```text
occupied_pages =
    popcount(TBNOccupancyMask within candidate)

total_pages =
    number of valid 4 KB pages in candidate
```

Expand to the candidate only when:

```text
occupied_pages * 100 > total_pages * 51
```

The comparison is strictly `>`.

Pseudo-code:

```go
region := containing64KBLeaf(faultVA)

occupancy := GPUResidentMask |
             CurrentFaultExpanded64KBMask

for parent := region.Parent(); parent != nil; parent = parent.Parent() {
    occupied := popcount(occupancy & parent.ValidPageMask)
    total := popcount(parent.ValidPageMask)

    if occupied*100 <= total*51 {
        break
    }

    region = parent

    if region.Size == 2*MB {
        break
    }
}
```

The search terminates when:

- the next ancestor fails the threshold, or
- the 2 MB VA Block root is reached.

---

## 11.5 Example: First Fault

Assume a 128 KB candidate containing two 64 KB leaves:

```text
128 KB
├── leaf A: one 4 KB demand fault
└── leaf B: empty
```

The demand fault expands to all 16 pages of leaf A in the TBN occupancy tree.

Therefore:

```text
occupied = 64 KB
total    = 128 KB

occupancy = 50%
```

Since:

```text
50 > 51
```

is false, TBN does not expand to 128 KB.

---

## 11.6 Example: Resident Pages Increase Occupancy

Assume a 256 KB candidate:

```text
256 KB
├── 64 KB already GPU resident
├── 64 KB already GPU resident
├── 64 KB current fault leaf
└── 64 KB empty
```

Then:

```text
TBNOccupancyMask = 192 KB occupied

192 / 256 = 75%
```

Since `75 > 51`, the 256 KB node becomes the selected TBN region.

---

## 11.7 Demand Pages vs Prefetch Pages

The TBN occupancy calculation and final migration classification are separate.

The current demand-fault pages contribute to occupancy, but must not later be counted as prefetched pages.

Conceptually:

```text
SelectedTBNRegionMask
    = all valid 4 KB pages in selected TBN region

DemandMigrationMask
    = current demand-fault pages requiring migration

PrefetchCandidateMask
    = SelectedTBNRegionMask
      AND NOT DemandFaultMask
```

The exact demand-fault mask remains 4 KB granular even though the occupancy tree expands the fault to a 64 KB leaf.

---

## 11.8 Duplicate-Migration Suppression

Because MGPUSim v0.8 allows concurrent DMA activity, a newly selected TBN region may overlap pages already being migrated by another transaction.

These pages must not generate duplicate DMA requests.

The final actual prefetch-migration mask is:

```text
PrefetchMigrationMask
=
SelectedTBNRegionMask
AND NOT GPUResidentMask
AND NOT DemandFaultMask
AND NOT MigratingToGPUMask
AND NOT PrefetchInFlightMask
```

Therefore:

```text
PrefetchInFlightMask:
    not counted as TBN occupancy
    but excluded from new DMA requests

MigratingToGPUMask:
    not counted as TBN occupancy
    but excluded from new DMA requests
```

This preserves the NVIDIA-like occupancy rule while remaining safe under simulator DMA concurrency.

---

## 11.9 Actual Transfer Bytes

TBN-selected bytes and actual migration bytes must be tracked separately.

Example:

```text
Selected TBN region       = 256 KB
Already GPU resident      = 64 KB
Demand-fault pages        = 4 KB
Already prefetching       = 64 KB

Actual new prefetch bytes =
selected pages
- resident pages
- demand pages
- in-flight pages
```

Only actual missing 4 KB pages generate DMA traffic.

---

## 11.10 VA Block Boundary

TBN expansion never crosses the current 2 MB VA Block.

For partial first/last VA Blocks, the allocation boundary further constrains the valid-page mask.

```text
TBN candidate
AND
VABlock.ValidAllocationMask
```

must be used for both threshold denominator and final migration selection.

---

## 11.11 Recommended Bitmap State

Each 2 MB VA Block contains 512 x 4 KB page bits.

At minimum:

```go
GPUResidentMask       PageMask512
DemandFaultMask       PageMask512
MigratingToGPUMask    PageMask512
PrefetchInFlightMask  PageMask512
PrefetchedMask        PageMask512
```

The tree itself does not need to duplicate page state.

Ancestor occupancy may be computed using bitmap population counts.

If this becomes a simulation-performance bottleneck, cached subtree population counters may be added without changing semantics.

---

## 11.12 TBN Statistics

Track:

```text
num_tbn_fault_events

num_tbn_64kb_selections
num_tbn_128kb_expansions
num_tbn_256kb_expansions
num_tbn_512kb_expansions
num_tbn_1mb_expansions
num_tbn_2mb_expansions

tbn_selected_bytes
tbn_demand_bytes
tbn_prefetch_candidate_bytes
tbn_actual_prefetch_dma_bytes

tbn_prefetch_suppressed_resident_bytes
tbn_prefetch_suppressed_inflight_bytes

tbn_useful_prefetched_4kb_pages
tbn_unused_prefetched_4kb_pages
```

---

## 11.13 Fixed TBN Decisions

The following are fixed as of v0.8:

```text
Base page                         = 4 KB
VA Block                          = 2 MB
Minimum TBN leaf                  = 64 KB
Maximum TBN region                = 2 MB
Threshold                         = 51
Comparison                        = > 51%

OccupancyMask =
    GPUResidentMask
    OR CurrentFaultExpanded64KBMask

PrefetchInFlightMask in occupancy = NO
MigratingToGPUMask in occupancy   = NO

In-flight masks are used only for duplicate-DMA suppression.

Thrashing detector                = not modeled initially
```

---

# 12. Access Counter Mode

## 12.1 Goal

Access Counter mode allows the GPU to access host-resident managed memory remotely instead of immediately migrating it on the first read.

Enable with a configuration flag, conceptually:

```text
-uvm-access-counter
```

When disabled:

```text
CPU-resident GPU access
 -> GPU page fault
 -> migration
```

When enabled:

```text
CPU-resident read
 -> remote PTE
 -> PCIe cache-line access
 -> increment access counter
 -> migrate when threshold reached
```

---

# 13. Remote GPU Memory Access

## 13.1 Read

For a CPU-resident page with a remote PTE:

```text
GPU memory request
 -> TLB lookup / GMMU translation
 -> remote CPU PA
 -> PCIe cache-line request
 -> CPU memory
 -> PCIe response
 -> GPU requester
```

The GPU may issue remote accesses at **cache-line granularity**.

## 13.2 No GPU Caching

Remote host-memory data accessed through the Access Counter path is **non-cacheable in GPU data caches**.

Therefore:

- remote data must not be inserted into L1
- remote data must not be inserted into L2 data cache
- each remote memory access generates PCIe traffic

This restriction applies to data caching only.

The corresponding remote address translation **may remain cached in L2 TLB**.

This distinction is mandatory:

```text
Remote PTE:
    cacheable in L2 TLB

Remote data:
    non-cacheable in GPU L1/L2 data caches
```

---

# 14. Access Counter Accounting

Access counting occurs for every remote GPU memory access.

The observation point must be after translation has identified a request as CPU-remote.

Conceptual sequence:

```text
GPU request
 -> translation
 -> location == CPU_REMOTE
 -> access-counter update
 -> PCIe memory access
```

The counter should be associated with a fixed accounting region.

Fixed accounting region:

```text
64 KB region = 16 x 4 KB base pages
```

Conceptual key:

```text
(PID, VA_block, 64KB_region)
```

Example:

```go
counter.Increment(pid, regionBase)
```

## 14.1 Threshold Migration

For reads:

```text
if accessCounter >= threshold:
    trigger CPU -> GPU migration
```

The threshold is fixed to **8 remote accesses** by default and remains configurable.

```text
uvm-access-counter-threshold = 8
```

Counter semantics are:

- one counter per 64 KB region,
- increment once for each remote GPU memory request serviced for that region,
- monotonically increase during a kernel,
- no sliding time window,
- immediately generate an Access Counter notification when the threshold is reached,
- reset all Access Counter values at each kernel launch.

The default migration rule is:

```text
if accessCounter >= 8:
    immediately notify UVMDriver
    trigger CPU -> GPU migration
```

A region which receives fewer than 8 remote accesses during a kernel remains CPU-resident and remotely accessible for the remainder of that kernel unless another event, such as a normal write, explicitly requires migration.

---


## 14.2 Kernel-Launch Counter Reset

At every kernel launch, the UVMDriver resets all Access Counter values associated with the process/GPU before the new kernel begins execution.

Conceptually:

```text
LaunchKernelReq
    |
    v
UVMDriver.resetAccessCounters(PID, GPU)
    |
    v
CP dispatches kernel
```

The reset occurs at the **kernel boundary**, not when a 64 KB region is migrated or evicted.

For a region that stays CPU-resident across kernels:

```text
kernel N:   5 remote accesses -> no migration
kernel N+1: counter starts again from 0
```

Therefore accesses are not accumulated across kernel launches.

# 15. Write Semantics

A GPU write to a CPU-resident remote-mapped managed page must trigger immediate migration.

Required behavior:

```text
GPU write to CPU_REMOTE mapping
    |
    v
Access Counter / remote-access detector
    |
    v
stall write
    |
    v
request migration
    |
    v
CPU -> GPU migration
    |
    v
update PTE to GPU_LOCAL
    |
    v
TLB range invalidation
    |
    v
replay write
```

The write must **not** be directly committed to host memory through the normal remote-access path.

This gives the policy:

```text
Remote read  -> allowed
Remote write -> immediate migration
```


## 15.1 Atomic Operations

Atomic operations are **not classified as normal writes for the immediate-migration rule**.

Therefore:

```text
Remote normal write -> immediate migration
Remote atomic       -> remote non-cacheable atomic operation
```

A remote atomic increments the 64 KB Access Counter once per atomic memory request and may therefore cause migration through the normal threshold mechanism.

The simulator must preserve atomicity at the remote host-memory endpoint. If the selected MGPUSim memory-request protocol cannot represent remote atomic semantics, the implementation must explicitly reject unsupported remote atomics rather than silently treating them as ordinary writes.

---

# 16. Access-Counter Migration

When a read counter reaches the migration threshold:

```text
ACCESS_COUNTER_THRESHOLD
    |
    v
notify CP / UVM control path
    |
    | PCIe
    v
UVMDriver
    |
    v
CHECK_GPU_CAPACITY
    |
    +--> EVICT if necessary
    |
    v
MIGRATE_CPU_TO_GPU
    |
    v
UPDATE_PTE: REMOTE -> GPU_LOCAL
    |
    v
TLB_SHOOTDOWN
    |
    v
resume local accesses
```

Access Counter notifications are generated **immediately** when the counter reaches the threshold.

Required path:

```text
GPU Access Counter
 -> CP
 -> PCIe
 -> UVMDriver
```

No periodic batching is modeled in the initial implementation.

A threshold notification is generated only on the threshold-crossing event for the current residency episode. Repeated accesses while a migration notification is already pending must not generate duplicate migrations.


If the target 64 KB region is already being brought to the GPU by demand migration or TBN prefetch, an Access Counter notification for that region is ignored.

Required rule:

```text
if region.state in {
    FAULT_PENDING,
    FAULT_HANDLING,
    MIGRATING_TO_GPU,
    PREFETCHING_TO_GPU,
}:
    ignore AccessCounterNotification(region)
```

No additional migration transaction is created and no duplicate DMA request is issued.

The existing migration/prefetch transaction becomes authoritative for that region.

---

# 17. Oversubscription

## 17.1 Definition

Oversubscription occurs when managed GPU-resident memory demand exceeds the configured GPU memory capacity available to UVM.

The UVMDriver owns GPU managed-memory capacity accounting.

Conceptual fields:

```go
GPUCapacityBytes
GPUResidentManagedBytes
```

Before migration:

```text
required = bytes_to_migrate

if resident + required > capacity:
    evict victims
```

---


## 17.1 Proactive Pre-Eviction

The UVMDriver maintains a **64 KB free-capacity headroom** using proactive LRU eviction.

There is no percentage low-watermark/high-watermark policy in the initial model.

### Trigger

Before admitting an incoming H2D migration, the driver computes projected occupancy including all already-reserved incoming GPU pages.

Let:

```text
C = configured UVM GPU capacity
R = bytes currently backed by allocated GPU physical pages
I = bytes already reserved for incoming H2D migrations
N = bytes required by the new H2D migration
E = bytes whose pre-eviction is already in flight
H = 64 KB target free headroom
```

The driver tries to ensure:

```text
C - (R + I + N) + E >= H
```

If this is not satisfied, it proactively selects 64 KB LRU victims.

Required bytes to schedule for eviction:

```text
NeedToEvict =
max(0, H - (C - (R + I + N) + E))
```

Number of new 64 KB victims:

```text
NumVictims = ceil(NeedToEvict / 64KB)
```

This means pre-eviction can begin **while the current H2D migration is running**, as long as that H2D migration already has enough physical GPU capacity reserved to start.

If the incoming migration itself cannot reserve enough GPU physical pages, it waits until sufficient in-flight pre-evictions complete and actually free pages.

### Multiple Pre-Evictions

The driver may have **multiple 64 KB pre-eviction victims in flight concurrently**.

There is no fixed UVM-side queue-depth limit.

The driver schedules only as many additional victims as required by the formula above; it does not evict arbitrary extra pages.

Each selected victim is immediately marked:

```text
EVICTING
```

and removed from the eligible LRU victim set so it cannot be selected twice.

### DMA Concurrency

Pre-eviction D2H transfers may overlap with:

```text
demand H2D
TBN-prefetch H2D
Access-Counter H2D
write-triggered H2D
other pre-eviction D2H transfers
```

Bandwidth contention is handled by the existing DMA/PCIe model.

### Capacity Accounting

An `EVICTING` region continues to occupy GPU physical memory until:

```text
range cache WB+INV
 -> TLB invalidate
 -> D2H DMA completion
 -> GPU physical pages freed
```

Therefore it must not be counted as immediately reusable free capacity.

Track:

```text
GPUResidentManagedBytes
GPUIncomingReservedBytes
GPUBytesEvicting
GPUFreeManagedBytes
```

A page reservation for an incoming migration prevents another migration from claiming the same capacity even before the H2D DMA completes.

### Statistics

Track:

```text
num_pre_evictions
bytes_pre_evicted

num_concurrent_pre_evictions
max_concurrent_pre_evictions

num_pre_evictions_overlapped_with_h2d
migration_wait_cycles_for_capacity
```

# 18. LRU Eviction

## 18.1 Victim Selection

The initial eviction policy is **LRU**.

The same 64 KB LRU structure is used for both reactive eviction and proactive pre-eviction.

Eviction unit:

```text
64 KB = 16 x 4 KB base pages
```

Each GPU-resident 64 KB region tracks a recency timestamp owned by the UVM manager:

```text
lastMigrationTime
```

The timestamp is updated **only when page migration/admission occurs**.

Ordinary GPU accesses to an already GPU-resident page do not update the timestamp.

Therefore, the first implementation is technically a **migration-recency LRU approximation**, rather than a strict access-recency LRU. This behavior is intentional for the initial model and avoids adding UVM-manager updates to every GPU memory access.

## 18.2 Eligibility

A victim must:

- be GPU resident
- not currently be migrating
- not be pinned
- not belong to an in-progress fault transaction that requires residency
- not be selected twice by concurrent eviction operations

Possible transient marker:

```text
evictionPending
```


Prefetched and demand-migrated 64 KB regions participate in the same LRU ordering.

No special eviction penalty or priority is assigned to a prefetched-but-unused region in the initial implementation. Its recency is determined by the migration timestamp in the same way as any other newly admitted region.

## 18.3 Dirty Eviction

If GPU-resident data is dirty:

```text
GPU HBM -> PCIe -> CPU memory
```

All evictions perform GPU-to-CPU data migration.

Even if a region is believed clean, the initial model does not optimize away the D2H copy.

Required policy:

```text
every 64 KB eviction
 -> cache range WB+INV
 -> D2H copy of all 16 x 4 KB pages
 -> final CPU residency
```

This keeps eviction accounting deterministic and avoids introducing a separate CPU-copy-validity optimization.

This avoids ambiguity in the first implementation.

---

# 19. Eviction State Machine

Eviction operates on one **64 KB region**.

Unlike the previous draft, normal UVM eviction must **not** reuse MGPUSim's existing global `ShootDownCommand`.

The UVM control path uses range-scoped invalidation commands.

Required sequence:

```text
SELECT_64KB_LRU_VICTIM
    |
    v
MARK REGION = EVICTING
    |
    v
BLOCK NEW ACCESSES TO THE 64KB REGION
    |
    v
CACHE RANGE WRITEBACK + INVALIDATE (64KB)
    |
    v
PTE: GPU_LOCAL -> TRANSITION/INVALID
    |
    v
TLB RANGE INVALIDATE (64KB)
    |
    v
WAIT UNTIL REGION-SCOPED INVALIDATION COMPLETES
    |
    v
DMA D2H: GPU HBM -> CPU MEMORY
    |
    v
WAIT FOR DMA COMPLETION
    |
    v
INSTALL FINAL GPU PTE
    |
    +--> Access Counter OFF: INVALID
    |
    +--> Access Counter ON:  REMOTE
    |
    v
FREE GPU PHYSICAL PAGES
    |
    v
UPDATE RESIDENCY = CPU_RESIDENT
    |
    v
UNBLOCK REGION
```

No GPU-wide restart is required.

---

## 19.1 Range Cache Operation

The UVM implementation uses a **dedicated UVM range-cache control message** rather than extending the existing global cache-flush command.

Required request:

```go
type UVMCacheRangeFlushReq struct {
    PID        vm.PID
    StartVA    uint64
    StartPAddr uint64
    Size       uint64 // 64 KB
    Writeback  bool
    Invalidate bool
}
```

For eviction:

```text
Writeback  = true
Invalidate = true
Size       = 64 KB
```

The CP broadcasts the operation to all GPU data caches that may contain lines from the victim range.

Each cache must:

```text
1. identify cache lines belonging to the 64 KB physical range,
2. prevent new matching cache transactions from being admitted,
3. drain already-accepted matching cache/MSHR transactions,
4. write back dirty matching lines,
5. invalidate matching lines,
6. return an acknowledgement.
```

Unrelated cache lines and unrelated memory traffic continue normally.

The CP returns `UVMCacheRangeFlushRsp` only after all relevant caches acknowledge completion.

For cache levels that are architecturally write-through and therefore cannot hold dirty data, `Writeback=true` degenerates to drain + invalidate.

This range operation is required only for transitions that remove GPU-local residency:

```text
GPU_LOCAL -> REMOTE
GPU_LOCAL -> INVALID
```

It is not required for:

```text
INVALID -> GPU_LOCAL
REMOTE  -> GPU_LOCAL
```

because CPU-remote data is non-cacheable in GPU data caches.

---

## 19.2 Region Blocking During Eviction

Once a region becomes `EVICTING`, new accesses to that 64 KB region must not proceed using the old GPU-local mapping.

Affected requests are held/retried by the replay/fault machinery.

This blocking is **region-scoped**, not GPU-wide.

Other CUs and memory requests targeting unrelated virtual addresses continue execution.

After final PTE installation:

```text
Access Counter OFF:
    retried access -> INVALID -> demand fault

Access Counter ON:
    retried read  -> REMOTE access
    retried write -> write-triggered migration
```

---

# 20. GPU Memory Capacity and Reservation

The UVMDriver must not assume all configured HBM is available to managed memory.

Expose a configurable UVM capacity:

```text
uvm-gpu-memory-capacity
```

or:

```text
uvm-gpu-memory-capacity-ratio
```

This allows controlled oversubscription experiments.

Example:

```text
Working Set = 24 GB
UVM GPU Capacity = 16 GB
Oversubscription Ratio = 150%
```

Define:

```text
Oversubscription Ratio =
Managed Working-Set Size / UVM GPU Capacity
```

---

# 21. UVM Mapping Invalidation and Synchronization

Normal UVM migration uses **range-scoped translation invalidation**, not MGPUSim's existing GPU-wide shootdown sequence.

The required UVM-specific control messages are conceptually:

```go
type UVMTLBInvalidateReq struct {
    PID     vm.PID
    StartVA uint64
    Size    uint64
}

type UVMCacheRangeFlushReq struct {
    PID        vm.PID
    StartVA    uint64
    Size       uint64
    Writeback  bool
    Invalidate bool
}

type UVMFaultReplayReq struct {
    PID        vm.PID
    StartVA    uint64
    Size       uint64
}
```

The CP forwards these requests to the relevant GPU components.

A **finer 64 KB range-based cache/TLB invalidation model is required** for UVM. Global GPU-wide cache or TLB invalidation is not an acceptable substitute.

No normal UVM transition should:

```text
flush the entire CU pipeline
globally flush all GPU caches
globally flush all TLB entries
stop all CUs
restart the whole GPU
```

---

## 21.1 Range TLB Invalidation Routing

`UVMTLBInvalidateReq` is routed as:

```text
UVMDriver
    |
    v
CP
    |
    v
GMMU
    |
    +--> shared L2 TLB
    |
    +--> all private L1 TLBs
```

The request is scoped by:

```text
PID / ASID
StartVA
Size = 64 KB
```

The GMMU is the **invalidation coordinator**.

It broadcasts the range invalidation to every GPU TLB level that may cache the mapping, collects acknowledgements, and returns one completion response through the CP to the UVMDriver.

Each TLB invalidates every entry for the matching PID/ASID whose covered VA range overlaps the requested 64 KB region.

Unrelated TLB entries and unrelated translation requests remain active.

No full-TLB flush is permitted for ordinary UVM migration.

---

## 21.2 INVALID -> GPU_LOCAL

Demand-fault/TBN migration:

```text
1. mark region MIGRATING_TO_GPU
2. allocate GPU physical pages
3. DMA H2D copies missing 4 KB pages
4. wait for DMA completion
5. update PTEs to GPU_LOCAL
6. mark region GPU_RESIDENT
7. update migration-recency timestamp
8. replay faulting requests
```

No TLB invalidation is issued for `INVALID -> GPU_LOCAL` because invalid/non-resident translations are not cached in the TLB hierarchy.

Required path:

```text
DMA H2D
 -> PTE install
 -> Fault Replay
```

No data-cache flush is required.

---

## 21.3 REMOTE -> GPU_LOCAL

Access Counter migration or normal-write-triggered migration:

```text
1. mark region MIGRATING_TO_GPU
2. prevent creation of duplicate migration transactions for the region
3. already-issued remote reads are allowed to complete concurrently
4. DMA H2D copies the 64 KB region
5. wait for DMA completion
6. update PTEs: REMOTE -> GPU_LOCAL
7. issue UVMTLBInvalidateReq for the affected 64 KB VA range
8. wait for invalidation completion
9. mark region GPU_RESIDENT
10. update migration-recency timestamp
11. release/replay blocked accesses
```

Data-cache flush is not required because remote CPU-resident managed data is non-cacheable in GPU L1/L2 data caches.

The TLB invalidate is mandatory because the previous `REMOTE` translation may be cached in L2 TLB.


Already-issued remote reads do **not** need to be drained before H2D migration begins.

They may complete concurrently with the migration because:

- they are read-only,
- CPU/GPU execution is phase-separated,
- remote data is non-cacheable on the GPU,
- the destination copy does not modify the CPU source contents.

The PTE transition and 64 KB TLB invalidation determine where **subsequent** translations are directed.

A remote read that has already resolved to a CPU physical address before the mapping transition is allowed to finish using that remote address.

---

## 21.4 GPU_LOCAL -> REMOTE / INVALID

Eviction:

```text
1. select 64 KB victim
2. mark region EVICTING
3. block new accesses to the victim region
4. UVMCacheRangeFlushReq:
       writeback=true
       invalidate=true
       range=64KB
5. wait for range cache operation completion
6. change old GPU_LOCAL PTEs to transition/invalid state
7. UVMTLBInvalidateReq(range=64KB)
8. wait for TLB invalidation completion
9. DMA D2H copies victim data to CPU memory
10. wait for DMA completion
11. install final REMOTE or INVALID PTEs
12. free GPU physical pages
13. mark region CPU_RESIDENT
14. unblock/retry affected requests
```

The ordering guarantees that:

- dirty GPU cache data reaches HBM before D2H DMA,
- no stale GPU-local TLB translation remains while the victim is copied,
- unrelated GPU execution continues.

---

## 21.5 Transition Summary

| Mapping Transition | DMA | Cache Operation | TLB Operation | CU Pipeline Flush |
|---|---|---|---|---|
| `INVALID -> GPU_LOCAL` | H2D | none | **none** | **none** |
| `REMOTE -> GPU_LOCAL` | H2D | none | **64 KB invalidate** | **none** |
| `GPU_LOCAL -> REMOTE` | D2H | **64 KB WB+INV** | **64 KB invalidate** | **none** |
| `GPU_LOCAL -> INVALID` | **D2H** | **64 KB WB+INV** | **64 KB invalidate** | **none** |

---

## 21.6 Control-Path Architecture

```text
                        UVMDriver
                            |
                            | PCIe control
                            v
                     Command Processor
                       /      |       \
                      /       |        \
                     v        v         v
               DMA Engine   GMMU/TLB   Fault Replay
                              |
                              +--> range TLB invalidate
                              |
                              +--> range cache WB/INV
                                   (eviction only)
```

The CP is a dispatcher/control endpoint. It does not globally quiesce the GPU for ordinary UVM migration.

# 22. Fault Replay

A memory request that encounters an invalid/non-resident mapping or is blocked by a UVM mapping transition must not complete until the target region becomes usable.

Replay is **region/request scoped**, not a GPU-wide restart.

Required behavior:

```text
faulting/blocked request
    |
    v
retained in GPU fault/replay structure
    |
    v
UVMDriver completes migration + PTE update + required range invalidation
    |
    v
UVMDriver -> CP: UVMFaultReplayReq
    |
    v
CP/GMMU replays affected translation/memory requests
```

The UVMDriver should manage page-level fault transactions, while the GPU-side replay structure owns individual stalled memory requests.

Fixed ownership:

```text
GMMU:
    owns replayable/stalled memory requests
    owns the per-request replay queue

UVMDriver:
    owns 64 KB UVM service transactions
```

The CP delivers `UVMFaultReplayReq` to the GMMU after the corresponding mapping becomes usable.

The GMMU is responsible for:

```text
matching replayable requests to the serviced VA range
re-running translation
re-injecting the request into the memory path
retiring the replay-queue entry after successful completion
```

No `GPURestartReq` is required for normal UVM migration.

---

# 23. Concurrency

The simulator may receive concurrent requests for:

- page faults
- prefetch migrations
- access-counter migrations
- write-triggered migrations
- evictions

However, **demand page-fault service transactions are serialized at 64 KB granularity**:

```text
one active 64 KB fault-service transaction at a time
```

Operations targeting the same sub-block must be serialized or coalesced.

Suggested state:

```text
IDLE
FAULT_PENDING
MIGRATING_TO_GPU
GPU_RESIDENT
EVICT_PENDING
MIGRATING_TO_CPU
CPU_RESIDENT
```

Invalid transitions must be asserted or rejected.

Example:

```text
MIGRATING_TO_GPU + second fault
 -> coalesce

MIGRATING_TO_GPU + access-counter migration
 -> ignore/coalesce

MIGRATING_TO_CPU + GPU access
 -> stall and resolve after eviction state is known
```

---


## 23.1 Migration Transport Ownership

CPU <-> GPU UVM page migration uses the existing **DMA Engine**, not the Page Migration Controller (PMC).

Required command path:

```text
UVMDriver
    |
    | MemCopyH2DReq / MemCopyD2HReq
    | through Driver <-> CP PCIe/control connection
    v
GPU Command Processor
    |
    v
GPU DMA Engine
    |
    v
Host Memory <---- PCIe data traffic ----> GPU HBM
```

Ownership is:

- **UVMDriver**: chooses pages, source/destination addresses, and migration order.
- **CP**: receives migration commands from the driver and forwards memory-copy requests.
- **DMA Engine**: physically performs the timed H2D/D2H data transfer.
- **PMC**: not used in the single-GPU UVM design.

This reuses MGPUSim v4's existing `MemCopyH2DReq` and `MemCopyD2HReq` flow.


## 23.1.1 DMA Timing Semantics

Migration transfer time is derived from the actual number of bytes copied by the DMA Engine and the modeled PCIe/memory path.

For example, if TBN selects 256 KB but 64 KB is already GPU-resident:

```text
TBN-selected bytes     = 256 KB
actual H2D copy bytes  = 192 KB
```

Only the actual 192 KB transfer is charged to DMA/PCIe timing.

This migration time is independent of the fixed 20 us fault-handling delay.

Normal mode:

```text
total service includes:
    20 us software overhead
    + actual DMA/PCIe transfer latency
```

Ideal mode:

```text
20 us overhead          -> 0
DMA/PCIe transfer delay -> 0

logical migration bytes -> still 192 KB
```




## 23.1.2 DMA Concurrency and Resource Policy

The UVMDriver imposes **no artificial limit on the number of outstanding migration DMA transactions**.

Concurrency and throughput limits are delegated entirely to the existing MGPUSim DMA Engine, PCIe/interconnect ports, and their backpressure/bandwidth model.

Therefore:

```text
UVM policy:
    no software serialization of independent DMA transfers

hardware/simulator model:
    determines how many requests can actually progress concurrently
```

The following transfers may overlap when they target independent regions:

```text
demand-fault H2D migration
TBN-prefetch H2D migration
Access-Counter H2D migration
write-triggered H2D migration
reactive-eviction D2H
pre-eviction D2H
```

The serialized 64 KB **fault-service control path** does not imply serialized DMA activity.

Example:

```text
fault-service A control:   [20us][TBN][PTE/replay]
                                  \
H2D transfer A:                    [================]
D2H pre-eviction X:                  [==============]
D2H pre-eviction Y:                    [============]
```

If the DMA Engine or PCIe model becomes saturated, requests remain queued/backpressured there. The UVMDriver must not invent an independent per-UVM bandwidth or channel count.

### DMA Transfer Chunking

Residency is 4 KB-granular, so migration must never copy an already-resident page merely to make a DMA request larger.

For a migration mask:

1. inspect the selected 4 KB pages,
2. remove resident/in-flight pages according to the migration policy,
3. form maximal runs whose source and destination physical addresses are both contiguous,
4. emit one DMA request per contiguous run.

Thus a 64 KB TBN leaf may generate:

```text
1 x 64 KB DMA request
```

when all 16 source/destination pages are contiguous, or multiple smaller DMA requests when holes or non-contiguous PFNs exist.

Minimum accounting granularity remains 4 KB.

For a 64 KB eviction, all 16 pages are logically evicted. D2H DMA is likewise emitted as one or more contiguous physical runs, but the total logical eviction size is always 64 KB.

## 23.2 Scope Constraints

The initial UVMDriver model is explicitly limited to:

```text
single GPU
GPU execution phase-separated from CPU accesses
```

CPU and GPU are not modeled as concurrently modifying the same managed pages.

Therefore the initial implementation does not require general bidirectional CPU/GPU cache coherence.

CPU backing memory remains the migration/remote-access destination, but CPU-side accesses are assumed not to race with GPU execution.

# 24. Proposed Message Types

The exact MGPUSim message base types should follow existing conventions.

Possible messages:

```text
PageFaultReq
PageFaultRsp

AccessCounterNotification

MigrationReq
MigrationRsp

UVMTLBInvalidateReq
UVMTLBInvalidateRsp

UVMCacheRangeFlushReq
UVMCacheRangeFlushRsp

UVMFaultReplayReq
UVMFaultReplayRsp
```

Not all of these need to be externally visible messages.

Driver-internal migration bookkeeping can remain internal events.

---

# 25. Proposed UVMDriver Internal Modules

A possible decomposition:

```text
driver/
├── uvm_driver.go
├── uvm_fault.go
├── uvm_vablock.go
├── uvm_prefetch.go
├── uvm_access_counter.go
├── uvm_migration.go
├── uvm_eviction.go
├── uvm_lru.go
├── uvm_stats.go
└── ...
```

Conceptual ownership:

### `uvm_driver.go`

- global UVM configuration
- managed allocations
- event dispatch
- GPU registration
- capacity tracking

### `uvm_vablock.go`

- 2 MB VA Block metadata
- sub-block state
- residency lookup

### `uvm_fault.go`

- page-fault queue
- fault coalescing
- service sequencing
- fault latency

### `uvm_prefetch.go`

- TBN implementation
- migration-range selection

### `uvm_access_counter.go`

- remote-access counter metadata
- threshold handling
- write-trigger migration policy

### `uvm_migration.go`

- CPU <-> GPU DMA/PCIe copy
- migration state
- PTE updates

### `uvm_eviction.go`

- oversubscription handling
- victim eviction sequence

### `uvm_lru.go`

- LRU metadata and victim selection

---

# 26. Configuration Flags

Initial configuration knobs:

```text
-uvm

-uvm-ideal

-uvm-access-counter

-uvm-fault-handling-latency=<time>

-uvm-access-counter-threshold=8

-uvm-vablock-size=2MB

-uvm-tbn-min-node-size=64KB

-uvm-gpu-memory-capacity=<bytes>

-uvm-prefetcher=tbn
```

Potential experiment/debug knobs:

```text
-uvm-disable-prefetch
-uvm-disable-eviction
-uvm-disable-remote-access
```

`-uvm-ideal` is the canonical zero-latency experiment mode.

Do not implement `-uvm-zero-pcie-latency` or `-uvm-zero-fault-latency` as independent public experiment modes unless they are later needed for sensitivity studies.

Internally, `-uvm-ideal` is equivalent to enabling both zero-latency behaviors while retaining all UVM functional events and counters.

---

# 27. Statistics

The implementation should expose at least the following statistics.

## Faults

```text
num_gpu_page_fault_requests
num_unique_fault_services
num_coalesced_faults
fault_service_latency_total
fault_service_latency_avg
```

## Migration

```text
num_cpu_to_gpu_migrations
bytes_cpu_to_gpu

num_gpu_to_cpu_migrations
bytes_gpu_to_cpu

num_prefetch_migrations
bytes_prefetched

num_demand_migrations
bytes_demand_migrated

num_access_counter_migrations
bytes_access_counter_migrated

num_write_triggered_migrations
```

## Remote Access

```text
num_remote_reads
num_remote_writes_detected
bytes_remote_read
pcie_remote_read_transactions
```

Remote writes should normally become migration-trigger events rather than completed PCIe stores.

## Access Counter

```text
num_access_counter_increments
num_access_counter_notifications
num_access_counter_threshold_hits
```

## Eviction

```text
num_evictions
bytes_evicted
num_dirty_evictions
```

## TBN

```text
num_tbn_prefetch_events
tbn_prefetch_bytes
tbn_useful_prefetch_bytes
tbn_unused_prefetch_bytes
```

## TLB / Mapping

```text
num_remote_pte_installs
num_local_pte_installs
num_uvm_tlb_range_invalidations
```

---


## Ideal-UVM

```text
ideal_uvm_mode_enabled

ideal_num_page_fault_requests
ideal_num_unique_fault_services

ideal_num_cpu_to_gpu_migrations
ideal_bytes_cpu_to_gpu

ideal_num_gpu_to_cpu_migrations
ideal_bytes_gpu_to_cpu

ideal_num_evictions
ideal_bytes_evicted
```

The implementation does not need duplicate "ideal_" counters if the same standard counters are used unchanged in both modes. The key requirement is that enabling `-uvm-ideal` must **not remove functional UVM events from statistics**.

# 28. Required Invariants

The following invariants should be asserted during development.

### Residency

A sub-block must not simultaneously have two authoritative GPU/CPU states unless explicitly modeled.

### GPU Physical Allocation

```text
GPU_RESIDENT => valid GPU physical page exists
```

### Remote Mapping

```text
REMOTE mapping => CPU backing page exists
```

### Access Counter

```text
Access counter is incremented only for completed/issued CPU-remote GPU accesses.
```

### Remote Cacheability

```text
CPU_REMOTE data must never be inserted into GPU data caches.
```

### L2 TLB

```text
REMOTE PTE may be cached in L2 TLB.
```

### Write

```text
GPU write to CPU_REMOTE must not complete before migration to GPU-local memory.
```

### Oversubscription

```text
GPU managed resident bytes <= configured UVM GPU capacity
```

except transiently while an explicitly modeled atomic migration/eviction transaction is in progress.

---

# 29. Initial End-to-End Scenarios

## Scenario A — Demand Read Fault

Access Counter disabled.

```text
1. GPU reads managed VA.
2. TLB miss.
3. GMMU walk finds INVALID PTE.
4. GMMU sends fault to CP.
5. CP forwards PageFaultReq through PCIe.
6. UVMDriver coalesces the fault.
7. TBN chooses >=64 KB fetch region.
8. Driver checks capacity.
9. Driver evicts LRU regions if necessary.
10. CPU -> GPU migration occurs.
11. GPU PTE becomes GPU_LOCAL.
12. Driver requests TLB range invalidation.
13. Fault completion reaches GPU.
14. GPU replays access.
15. Read completes from GPU memory.
```

## Scenario B — Remote Read Below Threshold

Access Counter enabled.

```text
1. GPU reads managed VA.
2. L2 TLB or GMMU returns REMOTE PTE.
3. Access is marked CPU_REMOTE.
4. Access counter increments.
5. Cache-line request is sent through PCIe.
6. CPU memory returns data.
7. Data bypasses GPU L1/L2 data-cache insertion.
8. Request completes.
9. Page remains CPU resident.
```

## Scenario C — Remote Read Reaches Threshold

```text
1. Remote reads increment counter.
2. Counter reaches threshold.
3. GPU Access Counter notifies CP.
4. CP sends notification to driver through PCIe.
5. Driver checks GPU capacity.
6. LRU eviction occurs if required.
7. Region migrates CPU -> GPU.
8. PTE changes REMOTE -> GPU_LOCAL.
9. TLB range invalidation removes cached REMOTE translation.
10. Future accesses use GPU-local memory.
```

## Scenario D — Remote Write

```text
1. GPU issues write.
2. Translation returns REMOTE PTE.
3. Write is not sent as a normal remote store.
4. Write request stalls.
5. Migration request is generated.
6. Driver evicts if needed.
7. CPU -> GPU migration occurs.
8. PTE becomes GPU_LOCAL.
9. TLB range invalidation occurs.
10. Write is replayed and completes in GPU memory.
```

## Scenario E — Oversubscription

```text
1. Demand/TBN/access-counter migration requests new GPU capacity.
2. Capacity is insufficient.
3. Driver selects least-recently-used GPU-resident region.
4. Victim mapping is protected from new accesses.
5. 64 KB cache WB+INV and TLB range invalidation are issued.
6. Victim migrates GPU -> CPU.
7. GPU physical page is released.
8. Victim becomes INVALID or REMOTE depending on Access Counter mode.
9. Incoming region migrates CPU -> GPU.
10. New local mapping is installed.
```

---



## Scenario G — Concurrent Pre-Eviction

```text
1. GPU managed-memory usage reaches configured capacity.
2. UVMDriver selects the oldest eligible 64 KB LRU victim.
3. Victim is marked EVICTING.
4. 64 KB cache range WB+INV and TLB invalidation are issued.
5. D2H DMA pre-eviction begins.
6. An independent H2D migration is already in progress or begins concurrently.
7. H2D and D2H transfers overlap and contend for modeled DMA/PCIe resources.
8. The pre-eviction victim completes D2H.
9. Victim GPU physical pages are freed.
10. Final REMOTE/INVALID mapping is installed.
11. The newly freed 64 KB becomes available for future migrations.
```

Pre-eviction must not globally stall the active migration or unrelated GPU execution.

---

## Scenario F — Ideal UVM

```text
1. GPU accesses a non-resident managed page.
2. A normal page-fault request is generated.
3. Duplicate faults are coalesced normally.
4. The unique fault-service counter increments.
5. Fixed 20 us fault-handling delay is skipped because it is zero.
6. TBN selects the same region it would select in normal UVM.
7. Oversubscription/LRU decisions are performed normally.
8. Migration byte counters are updated normally.
9. DMA transfer delay is zero.
10. PTE/TLB residency state is updated normally.
11. Faulting request is replayed.
```

Thus ideal mode changes **time**, not the sequence of functional UVM decisions.

---

# 30. Implementation Phases

## Phase 1 — Basic Demand Paging

Implement:

- managed allocation
- 2 MB VA Block metadata
- CPU initial residency
- invalid GPU PTE
- GPU page fault
- CP -> PCIe -> UVMDriver fault path
- 4 KB page-state tracking with fault-triggered migration
- PTE update
- TLB range invalidation
- fault replay

No prefetch, access counter, or oversubscription yet.

## Phase 2 — TBN Prefetcher

Implement:

- mandatory 64 KB minimum fault/prefetch node (16 x 4 KB pages)
- hierarchical 64 KB -> 2 MB expansion with strict >51% occupancy threshold
- prefetch statistics

## Phase 3 — Oversubscription

Implement:

- configurable GPU UVM capacity
- access timestamps
- LRU victim selection
- GPU -> CPU eviction
- PTE remapping
- TLB range invalidation

## Phase 4 — Access Counter

Implement:

- REMOTE GPU PTE
- L2 TLB caching of remote PTE
- non-cacheable remote data accesses
- cache-line PCIe traffic
- remote-read counters
- threshold-based migration
- immediate migration on remote write

## Phase 5 — Fidelity and Optimization

Implement/refine:

- fault batching
- more accurate TBN behavior
- access-counter notification batching
- migration overlap
- precise synchronization timing
- detailed statistics
- `-uvm-ideal` zero-latency timing mode with full functional accounting

---

# 31. Fixed Design Decisions

The following decisions are fixed as of v0.3.

| Item | Decision |
|---|---|
| Access Counter region | **64 KB** |
| Access Counter threshold | **8** remote requests |
| Counter model | monotonically increasing within a kernel |
| Counter reset | every kernel launch |
| Notification | immediate at threshold crossing |
| Migration engine | existing GPU **DMA Engine** |
| Migration data path | UVMDriver -> CP -> DMA Engine -> PCIe -> memory |
| Eviction unit | **64 KB** |
| LRU metadata update | UVM manager updates timestamp on migration/admission |
| Prefetched-page LRU | same ordering as demand-migrated pages |
| Unused prefetch priority | no special treatment |
| Below-threshold remote reads | remain remote |
| Atomics | do not force write-triggered migration |
| CPU/GPU concurrent access | not modeled |
| Multi-GPU UVM | not modeled; single GPU only |
| Fault handling latency | **20 us per unique/coalesced 64 KB fault-service transaction** |
| Fault latency vs migration | separate; DMA/PCIe transfer time is modeled independently |
| Fault implementation | event-based scheduled delay |
| Duplicate faults | coalesced by `(PID, GPU, 64KB region)` |
| Ideal UVM | same UVM state machine; fault + PCIe transfer latency = 0 |
| Ideal UVM statistics | fault/migration/eviction counts and bytes remain enabled |
| Fault-region batching | **disabled**; one 64 KB fault-service region at a time |
| Fault-service queue policy | FIFO |
| Normal UVM control path | **range-scoped**, no GPU-wide shootdown/restart |
| Remote->Local invalidation | 64 KB TLB invalidate only |
| Local->Remote eviction | 64 KB cache WB+INV + 64 KB TLB invalidate |
| CU pipeline flush on migration | **disabled** |
| Fault completion | region/request-scoped fault replay |
| DMA concurrency | **guaranteed** for independent migrations/evictions |
| Access Counter during prefetch/migration | notification ignored |
| In-flight remote reads during Remote->Local | complete concurrently |
| Cache/TLB invalidation | **required 64 KB range-based model** |
| Fault replay owner | **GMMU** |
| `-uvm-ideal` control latency | all UVM control latency also zero |
| Pre-eviction | enabled; 64 KB LRU victim |
| Pre-eviction concurrency | may overlap with active H2D migration |
| Benchmark allocation under `-uvm` | modify each benchmark's relevant allocation call sites to use **`AllocateManaged`** |
| Benchmark allocation without `-uvm` | use existing `Allocate` |
| TBN occupancy | `GPUResidentMask OR CurrentFaultExpanded64KBMask` |
| Prefetch-in-flight in TBN occupancy | **excluded** |
| Migrating-to-GPU in TBN occupancy | **excluded** |
| In-flight migration masks | used only to suppress duplicate DMA |
| UVM-side DMA concurrency cap | **none** |
| DMA request unit | maximal contiguous run of selected 4 KB pages |
| Range cache command | dedicated UVM request/response |
| TLB invalidate coordinator | **GMMU** |
| Invalid PTE negative caching | **disabled** |
| Pre-eviction free-headroom target | **64 KB** |
| Pre-eviction in-flight depth | dynamic, no fixed UVM-side cap |
| Eviction copy policy | always D2H 64 KB logical victim |

## 31.1 Access Counter Lifetime

Counters are per-64 KB region and reset on kernel launch.

```text
counter[region] = 0 at kernel launch
```

During the kernel:

```text
remote read/atomic request -> counter++

counter == 8
 -> immediate notification
 -> migration transaction
```

Once a region becomes GPU-resident, its remote counter no longer affects accesses until it later becomes CPU-resident again.

## 31.2 LRU Interpretation

The requested policy updates recency only on migration/admission:

```text
lastMigrationTime = now
```

This is deliberately not a strict access-based LRU.

A long-lived, heavily accessed GPU-resident page can therefore become older than a recently prefetched but unused page.

This behavior is part of the initial model. If strict LRU is later required, a residency-hit update path must be added.

---

# 31.3 Remaining Implementation Decisions — CLOSED

There are no remaining architectural open questions in this draft.

The previously open items are fixed as follows.

| Topic | Fixed Decision |
|---|---|
| DMA outstanding limit | no UVM-side limit; use existing DMA/PCIe backpressure |
| DMA transfer chunking | maximal contiguous runs of selected 4 KB pages |
| Range cache control | dedicated `UVMCacheRangeFlushReq/Rsp` |
| Cache invalidation scope | exact 64 KB physical range; drain matching requests only |
| TLB invalidation routing | `UVMDriver -> CP -> GMMU -> all L1/L2 TLBs` |
| TLB invalidation scope | PID/ASID + 64 KB VA range |
| Negative TLB caching | disabled for invalid/non-resident PTEs |
| `INVALID -> GPU_LOCAL` invalidate | none |
| Eviction D2H copy | always performed |
| Pre-eviction trigger | projected occupancy plus one 64 KB free-headroom target |
| Pre-eviction depth | dynamic; enough 64 KB victims to satisfy headroom |
| Concurrent pre-evictions | allowed |
| Benchmark integration | benchmark-by-benchmark at existing allocation sites |
| Benchmark coverage | all timing-mode evaluation benchmarks and their GPU application buffers |

Any later change to these behaviors should be treated as a new specification revision rather than an implementation choice.

# 32. Implementation-Ready Next Step

The architecture is now sufficiently specified to begin code-level implementation.

The implementation agent should next map the specification into concrete MGPUSim v4 code in this order:

1. **Benchmark/API plumbing**
   - add/propagate `-uvm` and `-uvm-ideal`,
   - update each evaluation benchmark allocation site to select `AllocateManaged`.

2. **Managed allocation and VA Block metadata**
   - 4 KB page state,
   - 2 MB VA Blocks,
   - 64 KB region state and masks.

3. **GMMU fault/replay support**
   - replayable fault queue owned by GMMU,
   - no negative TLB caching,
   - GMMU region blocking during mapping transitions.

4. **Driver fault-service engine**
   - FIFO one-active-64KB fault service,
   - duplicate-fault coalescing,
   - 20 us scheduled software latency.

5. **DMA migration**
   - contiguous-run transfer generation,
   - H2D/D2H concurrency delegated to existing DMA Engine.

6. **TBN**
   - NVIDIA-style occupancy mask,
   - mandatory 64 KB leaf,
   - strict `>51%` ancestor expansion.

7. **64 KB UVM control operations**
   - dedicated cache range WB+INV,
   - CP -> GMMU -> all-TLB range invalidation,
   - region-scoped fault replay.

8. **Access Counter**
   - 64 KB counters,
   - threshold 8,
   - kernel-launch reset,
   - remote non-cacheable reads,
   - notification suppression during existing migration/prefetch.

9. **Oversubscription and pre-eviction**
   - migration-recency LRU,
   - 64 KB victims,
   - dynamic concurrent pre-evictions,
   - one-64KB free-headroom policy.

10. **Statistics and validation**
    - normal vs `-uvm-ideal`,
    - fault/migration/prefetch/remote-access/eviction counters,
    - assertions for residency, capacity reservation, and duplicate migration.

The implementation should preserve the functional UVM state machine in `-uvm-ideal`; only UVM timing costs become zero.
