# 5. Avatar
**Avatar** - *A Case for Speculative Address Translation with Rapid Validation for GPUs*, MICRO 2024.

## 5.1 Architectural Goal

Avatar overlaps translation and data access using a speculative physical address.

Its high-level path is:

```text
                     conventional translation
                    --------------------------->
L1 TLB miss
    |
    +--> predict PPN --> speculative data access
                            |
                            v
                     rapid validation
```

The implementation must therefore support **two concurrent operations for one memory instruction**:

1. background address translation;
2. speculative cache/memory access.

A model that simply reduces TLB-miss latency does not reproduce Avatar.

---

## 5.2 Mandatory Feature 1: Mapping Offset Detection (MOD) Table

CAST uses a PC-indexed Mapping Offset Detection table near each CU's L1 TLB.

Each logical entry contains:

```text
InstructionPC
V2POffset
ConfidenceCounter
Valid
ReplacementState
```

where:

```text
V2POffset = PPN - VPN
```

The table is fully associative in the paper and uses LRU replacement.

A 32-entry table is a reasonable paper-derived configuration.

### MOD training

When a real translation completes for a load instruction:

```text
new_offset = actual_PPN - VPN
```

If a matching PC entry exists:

```text
if new_offset == stored_offset:
    confidence += 1
else:
    confidence -= 2
```

The stored offset is replaced only when confidence reaches zero.

When a new offset is installed, initialize the confidence to the paper's starting state.

A new PC creates a new MOD entry.

The exact saturation behavior should be encoded explicitly rather than approximated by a generic branch predictor.

---

## 5.3 Mandatory Feature 2: CAST Speculative Translation

On an L1 TLB miss:

1. launch the normal L2-TLB/page-walk path;
2. probe MOD using the memory instruction PC;
3. if the MOD entry is sufficiently confident:
   - compute speculative PPN;
   - append the original page offset;
   - issue an immediate speculative data-cache request.

Conceptually:

```text
SpeculatedPPN = VPN + V2POffset
SpeculatedPA  = SpeculatedPPN || PageOffset
```

The paper uses a confidence threshold of 2.

The normal translation must continue in parallel until another mechanism explicitly resolves or cancels it.

---

## 5.4 Mandatory Feature 3: Pending Speculation Tracking

The load/store pipeline needs a table that binds together:

```text
original memory request
requested VPN
instruction PC
speculated PPN
speculation status
background translation request
speculative cache request
```

At minimum, add a speculation flag and speculative PPN to the existing pending memory-request state.

This state is required to avoid:

- returning unvalidated data
- completing the same instruction twice
- leaking a late page-walk response into a completed request
- losing a speculative cache response that arrives before translation

---

## 5.5 Mandatory Feature 4: Sector-Level Compression State

CAVA operates on 32-byte sectors.

The original Avatar design assumes a 128-byte cache line containing four 32-byte sectors.

A compressed sector is conceptually laid out as:

```text
32 B sector
+--------------------------------------------------------------+
| 22 B compressed data | 8 B page information | 2 B signature |
+--------------------------------------------------------------+
```

The page information includes:

- VPN
- permission bits

The signature identifies whether the sector is encoded/compressed.

### MGPUSim requirement

The simulator does **not necessarily need to execute a bit-exact BPC algorithm** for every access if that would make simulation impractically slow.

However, it does need an explicit per-sector model of:

```text
compressible / incompressible
embedded VPN
embedded permissions
compressed state
validation/guarantee state
```

Possible implementation fidelity levels:

**Level 1 - Trace/profile-driven compression**
- precompute whether each sector is compressible;
- use the resulting state during simulation.

**Level 2 - Functional compressor**
- run a simplified or actual BPC implementation on sector contents.

For performance comparison, Level 1 is usually sufficient if the compressibility input is realistic and held constant across configurations.

Do not assume that every sector is compressible. Avatar's fallback behavior is a central part of the proposal.

---

## 5.6 Mandatory Feature 5: CAVA Rapid Validation

When a speculative data request returns:

### Case A: sector contains embedded page information

1. extract/decompress page information;
2. compare embedded VPN with requested VPN;
3. verify permission;
4. if they match:
   - mark speculation validated;
   - make data visible to the requesting CU;
   - complete the memory request;
5. if they mismatch:
   - invalidate/discard the speculative sector;
   - wait for or use the conventional translation result.

### Case B: sector is uncompressed

The speculative data cannot be used immediately.

Keep the line/sector in a state equivalent to:

```text
present but unguaranteed
```

The background translation later determines whether the speculative PPN was correct.

- correct PPN -> mark guaranteed and use data;
- wrong PPN -> invalidate speculative data and issue/replay the correct access.

This behavior is essential. Treating every speculative fetch as usable before validation would model an unrealistically idealized Avatar.

---

## 5.7 Mandatory Feature 6: Cache Guarantee State

Avatar requires the cache to distinguish:

```text
valid and usable
valid but speculative/unverified
invalid
```

The paper adds:

```text
C bit = compression state
G bit = guarantee/validation state
```

to L2 sectors.

The L1 cache needs a guarantee state for data that has not yet been validated.

An unguaranteed line must be invisible to unrelated demand accesses.

This is a correctness requirement, not just a performance optimization.

---

## 5.8 Mandatory Feature 7: Mis-Speculation Recovery

When the true translation returns:

```text
if ActualPPN == SpeculatedPPN:
    validate speculative data if not already validated
else:
    invalidate speculative sector/line
    send data request to ActualPPN
```

No instruction should architecturally consume data from an incorrect PPN.

Because the GPU pipeline does not rely on CPU-style speculative rollback, validation must happen **before the data result becomes architecturally visible**.

---

## 5.9 Mandatory Feature 8: Early TLB Fill (EAF)

For a faithful Avatar implementation, include Early TLB Fill.

When CAVA validates the speculative translation from the embedded page metadata:

1. construct a complete TLB entry using:
   - VPN,
   - validated PPN,
   - permission information;
2. fill the local L1 TLB;
3. release the corresponding L1 TLB MSHR;
4. locate matching state in:
   - shared/L2 TLB MSHR,
   - page-walk buffer;
5. fill the shared/L2 TLB;
6. release/cancel redundant translation resources;
7. forward the translation to other CUs waiting for the same VPN when the simulator supports such merged requests.

The page walk may already have generated some traffic before EAF resolves the translation.

The implementation should therefore distinguish:

```text
page walk never launched
page walk launched but canceled
page walk completed before EAF
```

for accurate traffic accounting.

---

## 5.10 Mandatory Feature 9: Cache MSHR Interaction

Avatar's speculative data fetch can interact with an ordinary demand access or replay.

The simulator should correctly handle cases equivalent to:

- speculative request returns and becomes an L1 hit later;
- background translation finishes first and the demand request merges with the in-flight speculative cache MSHR;
- speculative line is evicted before it becomes useful.

These cases affect both performance and measured cache pollution.

At minimum, cache MSHR matching must understand whether a request refers to:

```text
same physical cache line
same speculative transaction
same original memory instruction
```

without causing duplicate completion.

---

## 5.11 Mandatory Feature 10: Page Migration and Embedded Metadata

When a page is installed in GPU memory, Avatar associates its sectors with page information.

For simulation:

1. page migration or GPU-page allocation creates/updates sector metadata;
2. each sector receives the correct VPN and permissions when compressible;
3. the old physical location must not retain valid embedded metadata after the page is migrated away.

The original proposal zeroes or invalidates the old location so that stale page information cannot validate a future mis-speculation.

If MGPUSim uses UVM page migration, hook this bookkeeping into the existing page-fault/page-copy completion path.

---

## 5.12 MGPUSim-Specific Concurrency Requirement

Avatar is especially sensitive to event ordering.

A single L1-TLB miss may produce:

```text
Translation request T
Speculative cache request S
```

Either can finish first.

The implementation should use a transaction object or request ID so that all event handlers can test whether the original request is still live.

Recommended logical state machine:

```text
                +--------------------+
                | L1 TLB miss        |
                +----------+---------+
                           |
              +------------+------------+
              |                         |
              v                         v
       Background T              Speculative S
              |                         |
              |                   data returns
              |                         |
              |                +--------+--------+
              |                |                 |
              |             CAVA pass         CAVA fail/
              |                |              unavailable
              |                v                 |
              |           complete request       |
              |           + optional EAF         |
              |                                  |
              +----------------+-----------------+
                               |
                      true translation arrives
                               |
                     compare ActualPPN
                               |
                     validate or replay
```

All late events must be safely ignored or converted into cache/TLB fill side effects without completing the instruction twice.

---

## 5.13 Suggested MGPUSim Touchpoints

Likely subsystems:

```text
akita/mem/vm/tlb/*
akita/mem/vm/addresstranslator/*
akita/mem/cache/*
mgpusim/amd/protocol/*
mgpusim/amd/driver/pagefault.go
mgpusim/amd/samples/runner/timingconfig/*
```

Expected changes:

| Subsystem | Required change |
|---|---|
| L1 TLB | Trigger MOD on miss; train MOD on resolved translation |
| CU LD/ST pipeline | Preserve PC and speculative request state |
| Translation MSHR | Support early resolution/cancellation |
| Cache | Guarantee/compression state and speculative MSHR behavior |
| Memory controller | Sector compression/metadata model |
| Driver/UVM | Install and invalidate embedded page metadata |
| Page-walk subsystem | Handle EAF cancellation/release |
| Statistics | Accuracy, coverage, CAVA success, EAF effects |

---

## 5.14 Avatar Validation Tests

1. **Correct speculation + compressed sector**
   - MOD predicts correct PPN;
   - sector contains matching VPN;
   - verify data returns before the normal translation.

2. **Correct speculation + uncompressed sector**
   - speculative data returns first;
   - verify data remains unusable until actual translation confirms PPN.

3. **Wrong speculation**
   - verify speculative data is invalidated and never consumed.

4. **EAF**
   - CAVA validates before PTW completion;
   - verify L1/L2 TLB state is filled and redundant translation resources are released.

5. **Late translation**
   - complete request through CAVA;
   - later deliver original PTW response;
   - verify no duplicate instruction completion.

6. **Speculative MSHR merge**
   - translation finishes while the speculative data fetch is in flight;
   - verify demand access merges with the existing cache request.

7. **Migration**
   - move a page to a new PPN;
   - verify old location cannot validate a speculation using stale metadata.

---