# Avatar Implementation Plan (MICRO 2024)

Speculative address translation with rapid validation, on top of the r9nano
baseline. Reference: `refs/avatar.md`. Marker: `sbin_claude_avatar`.

## 1. Design Decisions

### 1.1 Where Avatar lives: an interposer above the shared L2 TLB (ASU)

A new **Avatar Speculation Unit (ASU)** component sits between all L1 TLB
bottom ports and the shared L2 TLB Top port (one ASU per GPU), mirroring how
the Utopia UTU (`timing/utopia/rsw`) interposes below the L2 TLB.

Rationale: a `TranslationReq` leaving an L1 TLB *is* the L1-TLB-miss event
that triggers Avatar (refs 5.3). The ASU can observe every miss, run the MOD,
launch speculation concurrently with the normal L2-TLB/page-walk path, and
deliver an early (validated) `TranslationRsp` back to the L1 TLB — which
naturally models **Early TLB Fill** (refs 5.9): the L1 TLB fills the entry
and releases its MSHR as with any translation response. No akita changes and
no L1/L2 TLB component changes are required.

### 1.2 MOD is indexed by 2MB virtual region, not PC (documented deviation)

`vm.TranslationReq` carries no instruction PC, and plumbing PC through
CU -> AT -> TLB crosses the akita mem protocol. The paper's PC index is a
hardware proxy for "accesses streaming inside one contiguously-mapped
region"; with a 2MB-region-granular physical placement model (1.4) the
V2POffset is constant exactly within a 2MB virtual region. The MOD is
therefore indexed by `VPN >> 9` (2MB region ID). Everything else follows the
paper: 32 entries, fully associative, LRU, confidence +1 on match / -2 on
mismatch, offset replaced only at confidence <= 0, speculate at
confidence >= 2, new entries start at confidence 1. Per-requester (per-L1TLB,
i.e. per-CU) MOD tables keyed by the requesting port.

### 1.3 Compression: Level-1 parameterized model (refs 5.5 fidelity Level 1)

CAVA only needs "does this physical sector carry embedded page info that
matches the requested (PID, VPN)". No functional compressor is modeled:

- A shared authoritative **avatar metadata registry** (like
  `restseg.Registry`) maps physical frame -> {PID, VPN, valid}. The driver
  installs metadata when a page is mapped and invalidates it when the page is
  freed/remapped, so a stale frame can never validate a mis-speculation
  (refs 5.11, validation test 7).
- Compressibility is a deterministic pseudo-random draw per physical frame:
  `hash64(frameID ^ seed) < ratio * 2^64`, ratio from
  `-avatar-compress-ratio` (**default 0.8**). Deterministic => reproducible
  and identical across configs. Page-granular (the translation path cannot
  see the intra-page sector offset; documented simplification).

### 1.4 Fragmentation: 2MB-region randomized physical placement

The stock allocator hands out physical frames in ascending order
(`popNextAvailablePAddrs`), making PPN-VPN globally constant — MOD would be
~100% accurate and Avatar would degenerate into ideal-l1tlb. When avatar is
enabled (default, disable with `-avatar-frag=false`), GPU-device page
allocation goes through a region allocator in the registry:

- each 2MB-aligned *virtual* region (per PID) is bound on first touch to a
  pseudo-randomly chosen free 2MB *physical* region of the device;
- a page maps to `regionBase + pageIndexInVirtualRegion * 4KB`, so the
  offset is constant within a region and differs across regions — exactly
  the contiguity structure Avatar exploits.

### 1.5 Speculation timing model

On a confident L1 miss the ASU forwards the request to the L2 TLB as usual
AND starts a speculative validation with latency
`-avatar-validation-latency` (default 200 cycles ~= speculative L2/DRAM
sector fetch + CAVA check):

- **CAVA pass** (frame compressible + metadata matches (PID, VPN)): if the
  real response has not returned when the timer expires, re-verify against
  the GPU page table and respond early to the L1 TLB (EAF). The real
  response arriving later is swallowed (trains the MOD; no duplicate
  completion — refs 5.12, validation test 5).
- **CAVA fail** — frame incompressible (Case B, refs 5.6) or metadata
  mismatch (mis-speculation, refs 5.8): no early response; the transaction
  waits for the conventional translation. Counted separately in stats.
- Real response first: relay up, train the MOD, cancel pending speculation.

Wrong speculation can never produce a response: an early response is sent
only when registry metadata matches AND the current page table maps the VPN
to the speculated frame.

### 1.6 Fidelity gaps (accepted, documented)

- Speculative fetches do not issue real data requests: no cache pollution,
  no L1/L2 guarantee (G/C) bits, no speculative-MSHR merges (refs 5.6 Case B
  overlap, 5.7, 5.10). Case B is conservative (no overlap credit); pollution
  omission is optimistic. The validation-latency knob absorbs calibration.
- EAF does not cancel the in-flight L2-TLB/page-walk (conservative traffic;
  the late response is swallowed at the ASU).
- v1 scope: single GPU, no `-uvm`, not combinable with `-gpu=utopia`.

## 2. Code Changes

### 2.1 New: `mgpusim/amd/timing/avatar/meta` — authoritative registry

`Registry` (mutex-guarded, shared driver <-> ASU):
- `RegisterDevice(deviceID int, base, size, pageSize uint64)` — carve 2MB
  region pool.
- `AllocateFrame(deviceID int, pid PID, vAddr uint64) (pAddr, ok)` — region
  allocator (1.4); `FreeFrame(pAddr)`.
- `Install(pAddr uint64, pid PID, vAddr uint64)` / `Invalidate(pAddr)` —
  frame metadata.
- `Validate(specPAddr uint64, pid PID, vAddr uint64) meta.Verdict`
  (Pass / Incompressible / Mismatch / NoMetadata) using the deterministic
  compressibility hash (`ratio`, `seed` set at construction).
- Occupancy stats for reporting.

### 2.2 New: `mgpusim/amd/timing/avatar/asu` — the speculation unit

- `mod.go`: MOD table (1.2) + unit tests.
- `asu.go`: `Comp` (TickingComponent + MiddlewareHolder, topPort/bottomPort,
  per-source MODs, transaction table, `Stats`).
- `middleware.go`: rsw-style tick: parseFromTop (admit + MOD probe + forward
  + maybe start speculation), advanceSpeculations (count down; early respond
  on CAVA pass), parseFromBottom (relay or swallow + MOD train).
- `builder.go`: rsw-style builder (engine, freq, registry, pageTable,
  deviceID, validation latency, MOD params, maxInFlight).
- Stats: speculations, cavaPass, cavaIncompressible, cavaMismatch,
  earlyCompletions, realResponseFirst, swallowedRsps, forwarded.

### 2.3 Modified: `timingconfig/r9nano`

- `speculation_topology.go` (new): `SpeculationTopology` strategy interface
  (mirrors `TranslationTopology`): `buildSpeculationUnit`,
  `l1TranslationProviderPort`, `connectSpeculation`. Baseline: no-op /
  L2TLB Top. Avatar: builds the ASU, retargets `l1TLBAddressMapper.Port`,
  wires ASU Bottom <-> L2TLB Top with a direct connection.
- `builder.go`: `speculationTopology` field (baseline default),
  `WithSpeculationTopology`, calls in `Build()` after `buildL2TLB()` and
  after `connectL1TLBToL2TLB()`.
- `data_path_topology.go`: both `connectTranslation` variants plug
  `speculationTopology.l1TranslationProviderPort(b)` instead of the
  hardcoded L2TLB Top port.

### 2.4 New: `timingconfig/avatar` — GPU config wrapper

Embeds `r9nano.Builder` (utopia wrapper pattern) +
`WithAvatarSettings(r9nano.AvatarSettings{Registry, ValidationLatency, ...})`.

### 2.5 Modified: `timingconfig/builder.go`

`AvatarPlatformConfig` + `WithAvatar`, `avatarEnabled()`, validation
(1 GPU, no uvm), registry construction in `Build()`, `case "avatar"` in
`createGPUBuilder`, pass registry to the driver builder.

### 2.6 Modified: `driver` + `driver/internal`

- `driver.AvatarConfig{Enabled, Registry, FragEnabled}` + `WithAvatar` on the
  driver builder; allocator gets `SetAvatarRegistry(registry, frag)`.
- `memoryallocator.go`: GPU-device frame allocation routes through
  `registry.AllocateFrame` when frag is on; `insertPage`/`updatePage`/
  `removePage` maintain frame metadata (install new frame, invalidate old
  frame on remap/free — refs 5.11); freed avatar frames return to the region
  pool, not the default pool.
- `driver.go RegisterGPU`: register the device's memory range with the
  registry (mirrors the utopia RestSeg reservation site).

### 2.7 Modified: `samples/runner`

- `flag.go`: `-gpu` help text; new flags `-avatar-compress-ratio` (0.8),
  `-avatar-validation-latency` (200), `-avatar-mod-entries` (32),
  `-avatar-confidence-threshold` (2), `-avatar-frag` (true); validation:
  avatar requires `-timing`, 1 GPU, no `-uvm`.
- `runner.go`: `AvatarConfig` fields, pass to platform builder when
  `GPUType == "avatar"`.
- `report.go`: collect `*asu.Comp` from simulation components, emit
  `avatar_*` metrics (speculation/cava/early-completion counts).

### 2.8 Scripts

- `3_gen_runners.py`: add `'avatar'` config + `elif config == 'avatar':`
  writing `-gpu=avatar `.
- `4_run_benchmarks.sh`: add an `avatar` configs line (commented toggle,
  like utopia).

## 3. Tests

- `meta`: compressibility determinism + ratio convergence; region allocator
  (offset constant within a 2MB region, differs across regions, free/reuse);
  invalidation prevents stale validation (refs test 7).
- `asu/mod`: training, confidence saturation, replacement (refs 5.2).
- `timingconfig/avatar_topology_test.go`: build `-gpu=avatar` platform;
  assert ASU exists, `l1TLBAddressMapper` targets the ASU, ASU bottom wired
  to the L2 TLB (mirrors `utopia_topology_test.go`).
- Behavioral checks via stats on a sample run: refs tests 1/2/3/5 map to
  cavaPass+earlyCompletions, cavaIncompressible (no early completion),
  cavaMismatch (never responded early), swallowedRsps == earlyCompletions.

## 4. Verification

```
( cd akita && go build ./... ) && ( cd mgpusim && go build ./... )
cd mgpusim && ginkgo -r amd/timing/avatar amd/samples/runner/timingconfig amd/driver
cd mgpusim/amd/samples/matrixtranspose && go build && \
  ./matrixtranspose -timing -parallel -gpu=avatar -arch=gcn3 -report-all \
    -verify -width=256
golangci-lint run ./amd/... --timeout=10m
```
