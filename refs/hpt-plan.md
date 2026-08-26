# Ideal-HPT Implementation Plan (PACT'24 FS-HPT)

Hashed page table for the GPU-side page walk, on top of the r9nano baseline.
Reference: "Rethinking Page Table Structure for Fast Address Translation in
GPUs: A Fixed-Size Hashed Page Table" (FS-HPT), PACT '24.
Marker: `sbin_claude_hpt`. Selector: `-gpu=hpt`.

Note: there is no `refs/hpt.md` spec document (unlike utopia.md / avatar.md).
Section 1.7 lists the paper-specific mechanisms deliberately NOT modeled in
the ideal configuration and where they would plug in later.

## 0. What "ideal-HPT" means here

FS-HPT replaces the multi-level Radix Page Table (RPT) with a fixed-size
hashed table: `hash(VPN)` indexes the table directly, so a walk is **one
memory reference when there is no hash collision**. The ideal configuration
assumes zero collisions, so every GPU page walk costs exactly one memory
access, and the intermediate-level structures (and therefore the page-walk
cache) disappear entirely.

Measured baseline for comparison (akita `mem/vm/gmmu`):

| path | modeled walk cost |
|---|---|
| baseline radix, PWC miss | `(4+1) x 100` = **500 cycles** |
| baseline radix, PWC hit at level L | `L x 100` = 100..400 cycles |
| **ideal HPT** | `1 x 100` = **100 cycles**, no PWC traffic |

The per-level constant 100 is `WithPageWalkingLatency(100)` in
`r9nano/builder.go buildGMMU()`, documented as "latency of each uncached
page-table level". One memory access therefore costs 100 cycles, matching
`rsw.defaultMissLatency` ("one modeled memory access, like the GMMU walk").

## 1. Design Decisions

### 1.1 HPT is a mode of the GMMU walk, not a new component

The GMMU already owns everything a page walk needs: the transaction table and
in-flight cap, the per-level latency countdown, the authoritative
`vm.PageTable` lookup, tracing task steps, UVM demand-fault gating, the fault
replay queue, and TLB range invalidation. A hashed page table changes exactly
one thing inside that machinery — **how many memory references the walk costs
and whether intermediate levels exist to cache** — so it is implemented as a
branch in the GMMU walk state machine, gated by a builder flag that defaults
to off.

Consequences:

- No new component, no new package, no `TranslationTopology` variant. The
  translation topology stays `baselineTranslationTopology`; the L2 TLB keeps
  targeting `GMMU.Top`.
- **UVM keeps working.** The HPT branch only changes the latency charged
  before the transaction reaches `pageWalkComplete`; `finalizePageWalk`,
  `needsUVMFault`, `sendUVMFault`, `replayRange`, and the TLB-invalidation
  broadcast all run unchanged. A separate walker component would have
  bypassed all of it (see 1.6).
- Every other configuration is bit-identical to today, because there is
  exactly one GMMU construction site (`r9nano/builder.go:781`, shared by
  r9nano / ideal-l1tlb / virtual-caching / utopia / avatar) and the flag
  defaults to false there.

Cost of this choice: it modifies `akita/mem/vm/gmmu`, a shared akita
component, rather than confining the change to mgpusim. That is acceptable —
akita is maintained source per CLAUDE.md — and the default-off flag bounds the
blast radius.

### 1.2 The branch point: one case in `walkPageTable()`

The radix path is `newTransaction -> sentToPageWalkCache -> pageWalkCacheDone
-> fillingPageWalkCache -> pageWalkComplete`. HPT enters the same countdown
machinery but skips the page-walk cache on both the lookup and the fill side:

```go
// gmmuMiddleware.go, walkPageTable()
case newTransaction:
    if m.hptEnabled { // sbin_claude_hpt
        madeProgress = m.startHashedWalk(i) || madeProgress
    } else {
        madeProgress = m.sendToPageWalkCache(i) || madeProgress
    }
```

```go
// startHashedWalk models an FS-HPT lookup: hash(VPN) indexes the fixed-size
// table directly, so the walk costs one memory reference and there are no
// intermediate levels to cache. fillLevel = -1 makes fillPageWalkCache a
// no-op, so the page-walk cache is never consulted and never filled.
// sbin_claude_hpt
func (m *middleware) startHashedWalk(i int) bool {
    trans := &m.walkingTranslations[i]
    trans.level = 0
    trans.fillLevel = -1
    trans.cycleLeft = uint64(m.hptAccessesPerWalk) * uint64(m.latency)
    trans.state = pageWalkCacheDone // countdown state; no cache was consulted
    m.hptWalks++
    m.hptMemoryAccesses += uint64(m.hptAccessesPerWalk)
    tracing.AddTaskStep(
        tracing.MsgIDAtReceiver(trans.req, m.Comp), m.Comp, "hpt-walk")
    return true
}
```

`fillPageWalkCache` loops `for trans.fillLevel >= lowestPageWalkCacheLevel`
(= 1), so `fillLevel = -1` falls straight through to `pageWalkComplete` with
no message sent. From there `finalizePageWalk` runs exactly as in the radix
path. Total: one `if` and one function.

The `pageWalkCacheDone` state name is misleading in HPT mode (no cache was
consulted); the inline comment says so. Renaming it would churn
`gmmu_test.go`, which references the state directly.

### 1.3 One knob: accesses per walk, reusing the existing per-access latency

The cost of a single memory reference is `m.latency`, the value already set by
`WithPageWalkingLatency(100)`. HPT reuses it rather than introducing a second
latency constant, so **one HPT access costs exactly what one radix level
costs** and the only variable between the two configurations is the count.
That is the clean scientific control.

`-hpt-accesses-per-walk` (default 1) is therefore the single knob:

- `=1` — ideal HPT, the configuration being measured.
- `=5` — a sanity check: should land close to baseline kernel time, since 5
  accesses is a full radix walk minus the PWC's help.
- `>1` in general — the extension point for collision chains (1.7).

### 1.4 The page-walk cache is not built in HPT mode

A hashed table has no intermediate levels, so there is nothing to cache. Two
lines make this structural rather than incidental:

- `Build()` skips `createPageWalkCache(name, gmmu)` when `hptEnabled`;
- `parseFromPageWalkCache()` returns false immediately when
  `m.pageWalkCachePort == nil`.

The `GPU[N].GMMU.PageWalkCache` component then does not exist in an HPT run,
which is visible in reports and is the honest representation of the scheme.
This is a real, reportable effect: benchmarks whose baseline gains most from
PWC hits will show the smallest HPT speedup.

### 1.5 No real PTE memory traffic

The HPT walk charges cycles and then resolves against the functional page
table; it issues no memory request for the PTE. This matches the baseline
GMMU radix walk and the Utopia UTU, which are both pure latency models.
Issuing real PTE reads only on the HPT path would make the comparison unfair
in the opposite direction — HPT would pay L2/DRAM contention that the
baseline never pays. The modeling level stays identical across all
configurations. Recorded as a fidelity gap in 1.7.

### 1.6 Scope: single GPU; `-uvm` structurally supported

`-gpu=hpt` is restricted to a single GPU, consistent with `-gpu=utopia` and
`-gpu=avatar`. Unlike those two this is a **scope choice, not a technical
constraint** — there is no shared driver-side state, each GPU builds its own
GMMU with its own page table, so lifting the cap later is a one-line change to
`validateHPTFlags`.

`-uvm` is **not** rejected. Per 1.1 the managed-page path is untouched by the
HPT branch, so `-gpu=hpt -uvm` is structurally correct. It is the one
combination none of the other translation schemes support, so the test plan
(section 3) smoke-tests it rather than assuming it.

### 1.7 Fidelity gaps (accepted, documented)

Ideal-HPT deliberately omits the FS-HPT mechanisms whose entire purpose is to
*restore* the one-access property under pressure:

- **Hash collisions** — assumed zero. Real FS-HPT walks a collision chain,
  costing extra memory references.
- **PTE eviction + victim buffer** — a fixed-size table must evict rarely-used
  PTEs; the victim buffer absorbs the resulting misses. Not modeled (nothing
  is ever evicted, because the functional page table is unbounded).
- **Step table** — the on-chip structure that makes the fast lookup fast. Not
  modeled; its benefit is folded into the flat one-access assumption.
- **Table resizing** — fundamentally avoided by FS-HPT, so nothing to model.
- **No real PTE memory traffic** (1.5): no L2 pollution from PTE reads, no
  DRAM contention, and conversely no L2-hit credit for hot PTEs.

Net direction: ideal-HPT is an **upper bound** on FS-HPT's benefit. That is
the intent, and it is the right first number to place next to ideal-l1tlb.

Extension path: `hptAccessesPerWalk` is already the per-walk access count, so
a `-hpt-collision-rate` knob turning a fraction of walks into
`1 + chainLength` accesses is a change to `startHashedWalk` alone.

## 2. Code Changes

### 2.1 Modified: `akita/mem/vm/gmmu` — the HPT walk mode

`builder.go`:
- fields `hptEnabled bool`, `hptAccessesPerWalk int`;
- `WithHashedPageTable(enabled bool) Builder` and
  `WithHPTAccessesPerWalk(n int) Builder`;
- `Build()`: validate `n >= 1` when enabled (panic like the existing negative
  latency check), skip `createPageWalkCache` when enabled, copy both fields in
  `configureInternalStates`.

`gmmu.go`:
- fields `hptEnabled bool`, `hptAccessesPerWalk int`, `hptWalks uint64`,
  `hptMemoryAccesses uint64`;
- exported accessors for the reporter and the topology test:
  `HashedPageTableEnabled() bool`, `HPTStats() HPTStats`
  (`HPTStats{Walks, MemoryAccesses uint64}`).

`gmmuMiddleware.go`:
- the `case newTransaction:` branch and `startHashedWalk` from 1.2;
- the `pageWalkCachePort == nil` guard at the top of
  `parseFromPageWalkCache`.

Roughly 60 lines added, all default-off. Pre-edit lines commented out and new
lines marked `sbin_claude_hpt`, per CLAUDE.md.

### 2.2 Modified: `timingconfig/r9nano/builder.go`

- `HPTSettings{Enabled bool; AccessesPerWalk int}` type + `hptSettings` field
  + `WithHPTSettings(HPTSettings) Builder`;
- `buildGMMU()`:

```go
if b.hptSettings.Enabled { // sbin_claude_hpt
    gmmuBuilder = gmmuBuilder.
        WithHashedPageTable(true).
        WithHPTAccessesPerWalk(b.hptSettings.AccessesPerWalk)
}
```

No topology change, no wiring change, no new file.

### 2.3 Modified: `timingconfig/builder.go`

- `HPTPlatformConfig{AccessesPerWalk int}` + `WithHPT(config)`;
- `hptEnabled() bool { return b.gpuType == "hpt" }`;
- `case "hpt":` in `createGPUBuilder` returning
  `r9nano.MakeBuilder().WithHPTSettings(...)` directly.

**No `timingconfig/hpt` wrapper package.** The `ideal-l1tlb` / `utopia` /
`avatar` wrappers exist because those types swap a component factory or a
topology and must keep the wrapper type alive through the fluent chain. HPT
only sets a builder value on `r9nano.Builder`, which already satisfies
`gpubuilder.GPUBuilder`, so a wrapper would be pure ceremony.

- Update the `WithGPUType` doc comment's selector list.

### 2.4 Modified: `samples/runner`

- `flag.go`: extend the `-gpu` help string to `... utopia, avatar, or hpt`
  (old string commented out per convention); new flag
  `-hpt-accesses-per-walk` (default 1); `validateHPTFlags()` requiring
  `-timing`, requiring a single GPU (1.6), and requiring
  `accessesPerWalk >= 1`; called next to `validateAvatarFlags()`.
- `runner.go`: `HPTAccessesPerWalk` field and
  `if r.GPUType == "hpt" { b = b.WithHPT(...) }` beside the utopia/avatar
  blocks.
- `report.go`: `hptGMMUs []*gmmu.Comp` collected by scanning
  `GPU[%d].GMMU` (same loop shape as `collectUtopiaUnits`) and keeping only
  those whose `HashedPageTableEnabled()` is true; `reportHPT()` emitting
  per-GPU `hpt_walk_count` and `hpt_memory_access_count`.

### 2.5 Scripts (`/home/sbin/vdram_v4/scripts`)

- `2_copy_benchmarks.sh`: `mkdir benchmarks/hpt`.
- `3_gen_runners.py`: `'hpt'` in `configs`; `elif config == 'hpt':` writing
  `-gpu=hpt `. An `hpt_accesses_per_walk` sweep dict mirroring
  `utopia_restseg_ratios` is the natural home for an access-count sweep, left
  commented.
- `4_run_benchmarks.sh`: commented `configs=(hpt)` line.
- `5_collect_metrics.py`: `'hpt'` in `configs` + the two `hpt_*` counters.
- `6_plot_normalized.py`: `'hpt'` in `configs` + `COLORS["hpt"]`
  (e.g. `"#DA8BC3"`, unused by the existing five).

## 3. Tests

- `akita/mem/vm/gmmu/gmmu_hpt_test.go` (plain Go tests, matching the existing
  `gmmu_test.go` style of driving the middleware directly):
  - `startHashedWalk` sets `cycleLeft = accessesPerWalk * latency`,
    `fillLevel = -1`, and state `pageWalkCacheDone`;
  - the walk reaches `pageWalkComplete` without any page-walk-cache message
    (assert `fillPageWalkCache` sends nothing with `fillLevel = -1`);
  - `accessesPerWalk = 5` charges 5x the single-access latency;
  - `parseFromPageWalkCache` returns false, not a panic, with a nil port;
  - the existing radix tests still pass with the flag off (regression guard
    that the default path is untouched).
- `timingconfig/hpt_gmmu_test.go`: build a `-gpu=hpt` platform, assert
  `GPU[1].GMMU.HashedPageTableEnabled()` is true and that
  `GPU[1].GMMU.PageWalkCache` does **not** exist in the simulation; build a
  `-gpu=r9nano` platform and assert the opposite for both.
- `timingconfig/builder_test.go`: `Entry("hpt returns r9nano.Builder", "hpt",
  r9nano.Builder{})`.
- End-to-end: `matrixtranspose -gpu=hpt -verify` must pass. The GMMU still
  panics on a missing page, so a passing verify run proves the hashed walk
  resolved every translation.
- UVM smoke test (1.6): `-gpu=hpt -uvm` on one small benchmark must complete
  and verify, confirming the managed-page fault path is untouched.
- Sanity comparison: on one benchmark, `-gpu=hpt -hpt-accesses-per-walk=5`
  should land close to `-gpu=r9nano` kernel time, while the default `=1`
  should be strictly faster. Cheapest end-to-end proof that the branch is
  actually on the critical path.

## 4. Verification

```bash
export GOROOT=/home/sbin/tools/go1.26
export GOPATH=/home/sbin/tools/go1.26/gopath
export PATH=$PATH:$GOROOT/bin:$GOPATH/bin

( cd akita && go build ./... ) && ( cd mgpusim && go build ./... )
cd akita && go test ./mem/vm/gmmu/... && ginkgo -r mem/vm
cd mgpusim && ginkgo -r amd/samples/runner/timingconfig
cd mgpusim/amd/samples/matrixtranspose && go build && \
  ./matrixtranspose -timing -parallel -gpu=hpt -arch=gcn3 -report-all \
    -verify -width=256
cd mgpusim && golangci-lint run ./amd/... --timeout=10m
```

Because this edits a shared akita component, also re-run the untouched
configurations to prove they are unchanged:

```bash
./matrixtranspose -timing -parallel -gpu=r9nano -arch=gcn3 -report-all -verify -width=256
./matrixtranspose -timing -parallel -gpu=virtual-caching -arch=gcn3 -report-all -verify -width=256
```

Known pre-existing failures (unrelated, present on a clean tree): the
`cp/internal/dispatching` kernel-info/progress stderr specs, and lint issues
in `memorycopy.go`, `go.mod` gomoddirectives, `l2_shootdown_test.go`,
`topology_validation.go`, and unformatted `uvm_migration.go`.

## 5. Phasing

| phase | content | status |
|---|---|---|
| P1 | `akita/mem/vm/gmmu` HPT walk mode + unit tests | todo |
| P2 | `r9nano` HPTSettings + `timingconfig` `case "hpt"` + topology test | todo |
| P3 | flags, runner wiring, report metrics | todo |
| P4 | scripts 2/3/4/5/6, end-to-end verify + UVM smoke + sanity comparison | todo |
| P5 (optional, beyond ideal) | `-hpt-collision-rate`, victim buffer, step table, real PTE memory traffic | not planned |
