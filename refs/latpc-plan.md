# LATPC Implementation Plan (MICRO'25)

Locality-aware TLB prefetching and MSHR compression, on top of the r9nano
baseline. Reference: "LATPC: Accelerating GPU Address Translation Using
Locality-Aware TLB Prefetching and MSHR Compression", MICRO '25
(refs/MICRO25_LATPC.pdf).
Marker: `sbin_claude_latpc`. Selector: `-gpu=latpc`. **Non-UVM only.**

## 0. What LATPC is, and how it maps onto this simulator

LATPC has three cooperating mechanisms (paper §5, Figure 10):

1. **Regularity Detector (RD)** — per SM, sits after the coalescer. Processes
   the warp instruction's unique VPNs in thread-index order and classifies
   each as a *demand* `<VPN, 0, 0>` or a *prefetch* `<VPN, Stride, Index>`
   triple. Strides are 9-bit, so every group stays inside one 512-page
   region, which means all its L4 PTEs live in the same 4KB page-table page
   (= one DRAM row).
2. **LATC (compressed L1 TLB MSHR)** — an L1 TLB MSHR entry is extended with
   `<Base VPN, 9-bit Stride, 32-bit Valid Mask>` and can track up to 32
   outstanding misses of one group, eliminating MSHR reservation failures
   (Figure 14, Algorithm 1).
3. **LATP (batched page walks)** — the GMMU's walk buffer coalesces the
   group's translations into one entry: L1–L3 of the radix walk (and the
   page-walk cache) are traversed once, then the L4 PTEs are fetched
   serially, each a DRAM row-buffer hit (Figure 15, §5.4).

Mapping onto this codebase (all paths verified on the current tree):

| paper | here |
|---|---|
| coalescer → RD (per SM) | `cu.defaultCoalescer` output, wrapped per CU |
| L1 TLB + MSHR (per SM) | `ShaderArray.L1VTLB[i]` (`akita/mem/vm/tlb`), per CU, 32-entry, MSHR 64 |
| L2 TLB + MSHR (shared) | `GPU.L2TLB`, 512-entry, MSHR 64 |
| PW Queue + 16 PTWs + PWC | `GPU.GMMU`: top-port queue, `maxNumReqInFlight` (default **16** — matches the paper's 16 PTWs), `pagewalkcache` |
| translation request | `vm.TranslationReq`, issued per cache-line transaction by the per-CU AT |

Request path (baseline data path, `shaderarray/data_path_topology.go`):
`CU.ToVectorMem → ROB → AT → (AT.Translation → L1VTLB) → L1V cache`, and
`L1VTLB.Bottom → L2TLB.Top`, `L2TLB.Bottom → GMMU.Top`.

## 1. Design Decisions

### 1.1 The Regularity Detector runs in the coalescer, not as a component

The paper's RD is a streaming FSM that consumes the coalescer's unique VPNs
one per cycle and emits triples. `defaultCoalescer.generateMemTransactions`
already has the entire warp instruction's addresses in lane order, so the RD
here is a pure function over that ordered sequence, producing exactly the
same triples the hardware FSM would. A wrapper coalescer
(`latpcCoalescer{inner: defaultCoalescer}`) runs the RD per instruction and
annotates each generated `mem.ReadReq`/`WriteReq` with its triple.

Why not detect at the AT: the AT sees an interleaved stream from many
wavefronts with no instruction boundary, so it would need the same
warp-instruction tag anyway, plus a fragile streaming reset. Doing it at the
coalescer is exact and needs no new component, no new ports.

The RD state machine (paper Fig. 12, §5.2), over consecutive unique VPNs of
one instruction (consecutive duplicates reuse the previous triple):

- first VPN, or broken stride, or 9-LSB region mismatch
  (`vpn>>9 != base>>9`) → demand `<vpn, 0, 0>`, new group (fresh GroupID),
  base := vpn.
- previous was demand and `s = vpn - prev` fits the same 512-page region →
  prefetch `<vpn, s, 1>`, group stride := s.
- `s == prevStride` → prefetch `<vpn, s, index+1>`.
- stride change → demand, new group (matches Fig. 12b: 0x1000 D; 0x1004,
  0x1008, 0x100c P stride 4; 0x1011 D; 0x1028 P stride 23; 0x1025 D; 0x1022
  P stride −3).

The RD's 1-cycle hardware latency (paper §5.5, −0.45% effect) is not
modeled; the annotation is free. Recorded as a fidelity gap (1.7).

### 1.2 Group metadata plumbing: hint fields end-to-end, default nil/zero

A triple must travel: coalescer → ROB → AT → L1 TLB → L2 TLB → GMMU.

- `akita/mem/mem/protocol.go`: `ReadReq`/`WriteReq` get
  `TranslationHint *vm.TranslationGroupHint` (nil for every existing
  producer). `vm.TranslationGroupHint{GroupID string; StridePages int64;
  Index int}`.
- `mgpusim/amd/timing/rob`: `duplicateReadReq`/`duplicateWriteReq` rebuild
  the request field-by-field and would drop the hint — copy it.
- `akita/mem/vm/addresstranslator`: `translate()` copies the hint from the
  incoming `mem.AccessReq` onto the `TranslationReq` it builds.
- `akita/mem/vm/protocol.go`: `TranslationReq` gets `GroupID string`,
  `GroupStride int64` (pages, signed), `GroupIndex int` (+ builder setters).
  Zero values = demand = today's behavior.
- `akita/mem/vm/tlb`: `fetchBottom` copies the three fields to the rebuilt
  bottom request (same place `IsWrite` is already propagated), so the L2 TLB
  and the GMMU see them.

Every hop defaults to "no hint", so all non-latpc configurations are
bit-identical.

### 1.3 LATC: a flag-gated compressed MSHR inside `akita/mem/vm/tlb`

`WithCompressedMSHR(true)` (default false) makes the TLB use a compressed
MSHR implementation next to the untouched `mshrImpl`:

- Entry: `pid`, `baseVPN` (page-aligned vAddr), `stridePages int64`,
  `validMask uint32`, per-subentry `Requests []*vm.TranslationReq` and
  `reqToBottom`. `baseVPN = vAddr − stride×index×pageSize` from the
  request's own annotation; demand ⇒ base = vAddr, stride 0.
- Lookup-miss handling (Algorithm 1 semantics):
  1. any entry whose set subentry covers this vAddr → **MSHR hit**: append
     the request to that subentry, no bottom send.
  2. an entry with the same `(pid, baseVPN)` and matching stride (an entry
     with stride 0 adopts the first non-zero member's stride, per Algorithm
     1 line 4) and bit `Index` clear → **miss-under-miss**: set the bit,
     send one bottom request for this vAddr.
  3. otherwise allocate a new entry if capacity allows; else **reservation
     failure** (stall, exactly today's `IsFull` behavior).
- Fill (`parseBottom`): find the entry whose set subentry matches the
  returned page's vAddr, answer that subentry's waiters through the
  existing responding-entry register, clear the bit; free the entry when
  the mask reaches zero. Insert order guarantees at most one entry covers a
  given in-flight vAddr (rule 1 always coalesces before rule 3 allocates).
- Capacity is counted in *entries*, as in the paper; one entry covers up to
  32 VPNs.

A `reservationFailureCount` counter is added to the TLB for **both** MSHR
implementations (the baseline currently stalls silently), so the paper's
Figure-18b metric is reportable for every configuration.

Scope: the latpc configuration enables this only on the per-CU L1V TLBs.
The L2 TLB stays uncompressed — the paper allocates L2 MSHR entries per
request regardless of type (§5.1 ❸). L1S/L1I TLBs keep the classic MSHR
(the paper only covers the LD/ST path); their requests carry no hint and
would behave identically anyway.

MSHR entry count stays at the baseline's 64 by default so the *only*
difference between `-gpu=latpc` and `-gpu=r9nano` is the mechanism, not the
capacity. A new `-l1tlb-mshr <n>` knob (applies to any GPU type) allows
reproducing the paper's contention regime (8 entries) on both
configurations symmetrically.

### 1.4 LATP: group-batched walks inside the GMMU, flag-gated

`WithLATPBatching(true, l4RowHitLatency)` (default false) adds one
coalescing step and one drain state to the existing walk machinery — the
same "mode of the GMMU walk, not a new component" shape that worked for
HPT:

- `parseFromTop`: a request whose `GroupID` matches an in-flight walking
  transaction *that has not finished its L1–L3 traversal* attaches to that
  transaction's member list instead of taking a walker slot ("the page
  table walks for these coalesced translations are reserved until the
  traversal completes", §5.4). No PWC lookup, no additional walker slot.
- The lead transaction walks exactly as today (PWC aggregate lookup,
  per-level countdown, PWC fills).
- When the lead reaches `pageWalkComplete`, it responds as today, then
  enters a `batchDraining` state: each member costs `l4RowHitLatency`
  cycles (its L4 PTE load hits the open DRAM row), then its
  `TranslationRsp` is sent with its own page from the functional page
  table. The walker slot is held until the last member drains — this is the
  paper's "reserved until completion" plus serial L4 issue.
- Members arriving during the drain still attach and are drained in turn;
  after the entry retires, a late group member simply starts a normal walk
  (it usually hits the L2 TLB by then, because every earlier member's rsp
  filled it — the paper's step ❻).
- Because each member is answered with a rsp to *its own* request ID
  carrying *its own* page, the L2 TLB fill path (`parseBottom` keyed on
  `rsp.Page.VAddr`) works unchanged, and each L1 TLB subentry gets its
  fill. No protocol change beyond 1.2.

`l4RowHitLatency` default: 20 cycles, knob `-latpc-l4-row-hit-latency`. The
baseline charges `WithPageWalkingLatency(100)` per uncached level (one
modeled memory access); a row-buffer hit is a CAS-only access, so ~1/5 of a
full access is the honest scale, and the knob makes it sweepable.

Non-UVM guard: the drain path answers members straight from the functional
page table and must never hit the managed-page fault path. `-gpu=latpc`
therefore rejects `-uvm` at flag validation (this is also the experiment
scope the study wants). A defensive panic in the drain path documents the
constraint.

Stats: `latp_batch_count`, `latp_batched_member_count` (average batch size
falls out), plus the existing walk counters.

### 1.5 What "prefetching" means in this model

In mgpusim every VPN of a warp instruction has a real transaction behind it
— the coalescer emits them all, back to back. So LATPC's "prefetch"
requests are not speculative here; they are the group's own demand stream,
annotated. Consequences, all consistent with the paper:

- Prefetch accuracy is 100% by construction (the paper measures 100% too,
  §6.4 — RD only ever prefetches VPNs the instruction actually needs).
- The modeled benefits are exactly the paper's three claims: fewer L1 MSHR
  reservation failures (LATC), fewer PTW invocations and queueing delays
  (LATP coalescing), and cheaper L4 accesses (row-hit drain).
- What is *not* modeled: an L1 probe rate of one VPN per cycle (transactions
  trickle through ROB/AT port bandwidth instead), and L2-TLB-hit prefetches
  arriving before their demand transaction. Both timing-level, both noted
  in 1.7.

### 1.6 Sizing relative to the paper

| structure | paper (Table 2) | here (baseline, kept) |
|---|---|---|
| L1 TLB | 32-entry FA, 8 MSHR | 32-entry FA, **64** MSHR (`-l1tlb-mshr` to sweep) |
| L2 TLB | 1024-entry 16-way, 128 MSHR | 512-entry 16-way, 64 MSHR |
| PTWs | 16 | GMMU `maxNumReqInFlight` = 16 |
| PWC | L1/L2/L3 16/16/16 | aggregate PWC (as-is) |
| pages | 4KB | 4KB |

The comparison target is `-gpu=r9nano` on identical sizes, so absolute
paper numbers are not reproduced — the mechanism's delta is. The L1 MSHR
knob exists precisely to also measure the paper's contention regime
(8 entries) fairly on both sides.

### 1.7 Fidelity gaps (accepted, documented)

- RD latency (1 cycle) and LATC tag-logic latency (1 cycle) not charged;
  paper measures the combined effect at −0.45% GMean (§5.5).
- No real PTE memory traffic: the drain charges cycles, not DRAM requests —
  identical modeling level to the baseline GMMU walk, HPT, and Utopia.
  Row-buffer locality is a flat per-member latency, not DRAM row state.
- Transactions probe the L1 TLB at data-path bandwidth, not 1 VPN/cycle.
- L1S / L1I translation paths are untouched (paper covers LD/ST only).
- Sorting of VPNs (paper §7 discussion) is not done — same as the paper's
  evaluated configuration (they evaluate unsorted).
- L2 TLB MSHR is not compressed — same as the paper.

## 2. Code Changes

### 2.1 Modified: `akita/mem/vm` — protocol plumbing (P1)

- `protocol.go`: `TranslationReq.{GroupID, GroupStride, GroupIndex}` +
  builder setters. `TranslationGroupHint` type.
- `mem/mem/protocol.go`: `ReadReq.TranslationHint`,
  `WriteReq.TranslationHint` (pointer, nil default).
- `addresstranslator/addresstranslator.go`: `translate()` (and
  `retranslate()`) copy the hint onto the built `TranslationReq`.

### 2.2 Modified: `mgpusim/amd/timing/rob` (P1)

- `duplicateReadReq` / `duplicateWriteReq`: carry `TranslationHint` over.

### 2.3 New: RD — `mgpusim/amd/timing/cu/latpccoalescer.go` (P2)

- `latpcCoalescer{inner coalescer; log2PageSize uint64}` implementing
  `coalescer`: runs `inner.generateMemTransactions`, derives the ordered
  unique-VPN sequence from the transactions' addresses, runs the RD state
  machine (1.1), stamps each transaction's request with its triple.
- `cubuilder.go`: `WithLATPCRegularityDetector(log2PageSize uint64)`
  option; `equipVectorMemoryUnit` wraps the default coalescer when set.

### 2.4 Modified: `akita/mem/vm/tlb` — LATC (P3)

- `tlbmshr.go`: compressed MSHR implementation (1.3) beside `mshrImpl`.
- `builder.go`: `WithCompressedMSHR(bool)`.
- `tlbMiddleware.go`: miss path branches (hit / miss-under-miss / allocate
  / reservation-failure) and fill path keyed by covering subentry; classic
  path untouched when the flag is off. `reservationFailureCount` counter +
  exported accessor for both modes.

### 2.5 Modified: `akita/mem/vm/gmmu` — LATP (P4)

- `builder.go`: `WithLATPBatching(enabled bool)`,
  `WithLATPL4RowHitLatency(cycles int)`.
- `gmmu.go`: member list on `transaction`, group index
  `map[string]int`-style lookup helper, `batchDraining` state, stats
  fields + accessors (`LATPStats{Batches, BatchedMembers uint64}`).
- `gmmuMiddleware.go`: attach-in-`parseFromTop`, drain state in
  `walkPageTable`, defensive non-UVM guard in the drain.

### 2.6 Modified: timing configuration (P5)

- `shaderarray/builder.go`: `WithLATPCSettings(...)` — enables the CU
  coalescer wrapper (2.3) and builds L1V TLBs with
  `WithCompressedMSHR(true)` and the `-l1tlb-mshr` entry count.
  A plain `WithL1TLBMSHRSize(n)` (default 64) is honored for every config.
- `r9nano/builder.go`: `LATPCSettings{Enabled bool; L4RowHitLatency int;
  L1TLBMSHREntries int}` + `WithLATPCSettings`; `buildGMMU()` adds the two
  LATP options when enabled; passes the settings to the shader-array
  builder. No topology change — translation topology stays
  `baselineTranslationTopology` (the GMMU keeps serving L2 TLB misses).
- `timingconfig/builder.go`: `case "latpc":` returning
  `r9nano.MakeBuilder().WithLATPCSettings(...)` — no wrapper package, same
  reasoning as HPT (only builder values change).

### 2.7 Modified: `samples/runner` (P5)

- `flag.go`: `-gpu` help text += `latpc`; flags
  `-latpc-l4-row-hit-latency` (default 20), `-l1tlb-mshr` (default 64,
  any GPU type); `validateLATPCFlags()`: requires `-timing`, single GPU,
  **rejects `-uvm`** (non-UVM only), latency ≥ 1.
- `runner.go`: `if r.GPUType == "latpc" { b = b.WithLATPC(...) }` beside
  the hpt/utopia/avatar blocks.
- `report.go`: report `l1tlb_reservation_failure_count` (sum over L1V
  TLBs; emitted for every configuration now that the counter exists),
  `latc_*` coalescing counters, and per-GPU `latp_batch_count` /
  `latp_batched_member_count` when the GMMU has batching enabled.

### 2.8 Scripts (`/home/sbin/vdram_v4/scripts`) (P6)

- `2_copy_benchmarks.sh`: `mkdir benchmarks/latpc`.
- `3_gen_runners.py`: `'latpc'` in configs, `-gpu=latpc` arm.
- `4_run_benchmarks.sh`: commented `configs=(latpc)` line.
- `5_collect_metrics.py`: `'latpc'` config + the new counters.
- `6_plot_normalized.py`: `'latpc'` + a fresh color. (Non-UVM only, so
  `7_plot_normalized_uvm.py` is not touched.)

## 3. Tests

- **RD unit test** (`cu/latpccoalescer_test.go`): drive the paper's
  Figure-12b sequence (0x1000, 0x1004, 0x1008, 0x100c, 0x1011, 0x1028,
  0x1025, 0x1022 as page addresses) through the wrapper and assert the
  exact demand/prefetch triples, including the negative stride and the
  512-page-boundary reset; duplicate consecutive VPNs reuse the triple;
  a hint-free path (LDS, scalar) stays nil.
- **LATC unit tests** (`tlb/` — Ginkgo, alongside `tlb_test.go`): the
  Figure-14c walkthrough — misses 0x8, 0xa, 0xc, 0xe with stride 2 end in
  one entry with mask 0b1111 and four bottom requests; a repeat of a
  covered VPN is an MSHR hit (no bottom send); fills clear bits one at a
  time, waiters are answered, the entry frees at mask 0; a demand entry
  adopts the stride of its first prefetch member; reservation failure only
  when a new entry is needed and the MSHR is at entry capacity; the
  reservation-failure counter increments; flag off ⇒ classic behavior
  (regression).
- **LATP unit tests** (`gmmu/gmmu_latp_test.go`, plain Go like
  `gmmu_hpt_test.go`): four same-GroupID requests occupy one walker slot;
  the PWC is consulted once; member rsps are spaced `l4RowHitLatency`
  apart after the lead completes; a different-GroupID request takes its
  own slot; a member arriving after retirement walks normally; stats
  count 1 batch / 3 members; flag off ⇒ existing tests unchanged.
- **Plumbing test**: hint survives `rob.duplicateReq` and appears on the
  AT's `TranslationReq`; `tlb.fetchBottom` propagates the three fields.
- **Topology test** (`timingconfig/latpc_topology_test.go`): `-gpu=latpc`
  builds; L1V TLBs report compressed MSHR, GMMU reports batching enabled;
  `-gpu=r9nano` reports both disabled.
- **End-to-end**: `matrixtranspose -timing -parallel -gpu=latpc -arch=gcn3
  -report-all -verify` passes (a passing verify proves every batched/
  compressed translation resolved correctly, since the GMMU still panics
  on a missing page). `-gpu=latpc -uvm` is rejected with a clear message.
- **Sanity comparison**: one benchmark at `-l1tlb-mshr=8` on both r9nano
  and latpc — latpc must show fewer reservation failures and fewer GMMU
  walks; at the default 64, kernel time should be ≤ baseline with
  `latp_batch_count > 0` on a divergent workload.

## 4. Verification

```bash
export GOROOT=/home/sbin/tools/go1.26
export GOPATH=/home/sbin/tools/go1.26/gopath
export PATH=$PATH:$GOROOT/bin:$GOPATH/bin

( cd akita && go build ./... ) && ( cd mgpusim && go build ./... )
cd akita && go test ./mem/vm/... && ginkgo -r mem/vm
cd mgpusim && ginkgo -r amd/timing/cu amd/timing/rob amd/samples/runner/timingconfig
cd mgpusim/amd/samples/matrixtranspose && go build && \
  ./matrixtranspose -timing -parallel -gpu=latpc -arch=gcn3 -report-all \
    -verify -width=256
cd mgpusim && golangci-lint run ./amd/... --timeout=10m
```

Because this edits shared akita components (mem protocol, tlb, gmmu, AT)
and the ROB, re-verify the untouched configurations:

```bash
./matrixtranspose -timing -parallel -gpu=r9nano -arch=gcn3 -report-all -verify -width=256
./matrixtranspose -timing -parallel -gpu=virtual-caching -arch=gcn3 -report-all -verify -width=256
./matrixtranspose -timing -parallel -gpu=hpt -arch=gcn3 -report-all -verify -width=256
```

Known pre-existing failures on a clean tree (see hpt-plan.md §4/§7): the
`cp/internal/dispatching` stderr specs, `idealmemcontroller` test build,
and the listed lint/gofmt items.

## 5. Phasing

| phase | content | status |
|---|---|---|
| P1 | hint plumbing: mem reqs, ROB duplication, AT, TranslationReq fields, TLB fetchBottom propagation + plumbing tests | planned |
| P2 | Regularity Detector coalescer wrapper + cubuilder option + Fig-12b unit test | planned |
| P3 | LATC compressed MSHR in `tlb` (flag-gated) + reservation-failure counter + Fig-14c unit tests | planned |
| P4 | LATP batching in `gmmu` (flag-gated) + drain state + unit tests | planned |
| P5 | config wiring (`shaderarray`, `r9nano`, `timingconfig`), runner flags (`-gpu=latpc`, `-latpc-l4-row-hit-latency`, `-l1tlb-mshr`, reject `-uvm`), report counters, topology test | planned |
| P6 | scripts 2/3/4/5/6, end-to-end verify, sanity comparison at `-l1tlb-mshr=8` and default | planned |
| P7 (optional) | VPN sorting study (§7), L2 TLB MSHR subentry study, real PTE memory traffic | not planned |

Suggested checkpoints: build + full unit tests after every phase; the
untouched-config regression runs after P1 (highest blast radius: shared
protocol structs) and again after P5.
