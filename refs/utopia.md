# 4. Utopia
**Utopia** - *Fast and Efficient Address Translation via Hybrid Restrictive & Flexible Virtual-to-Physical

## 4.1 Architectural Goal

Utopia divides physical memory into two mapping domains:

```text
+----------------------+----------------------+
|       RestSeg        |       FlexSeg        |
| restrictive mapping  | flexible mapping     |
| hash + tag lookup    | normal page table    |
+----------------------+----------------------+
```

A **Restrictive Segment (RestSeg)** constrains each VPN to a small set of possible physical frames.

A **Flexible Segment (FlexSeg)** retains conventional arbitrary virtual-to-physical mapping.

The main performance benefit comes from replacing expensive multi-level page walks for selected pages with a much smaller **RestSeg Walk (RSW)**.

---

## 4.2 Mandatory Feature 1: Physical-Memory Segmentation

The GPU physical-memory allocator should explicitly reserve one or more contiguous RestSeg regions and the remaining FlexSeg region.

Each RestSeg needs configurable:

```text
BasePhysicalAddress
SegmentSize
PageSize
Associativity
NumberOfSets
```

For a segment with `N` physical page frames and `M` ways:

```text
NumberOfSets = N / M
```

A VPN hashes to one set and may occupy one of the `M` ways in that set.

The implementation should support at least one 4 KB RestSeg.

Support for a second RestSeg for a larger page size can be added after the single-page-size version is validated.

---

## 4.3 Mandatory Feature 2: Tag Array (TAR)

The Tag Array stores the virtual-page identity for every occupied RestSeg way.

Conceptually:

```text
TAR[set][way] = {
    valid,
    VPN_tag,
    permission,
    replacement_metadata
}
```

The TAR is translation metadata, not a normal page table.

For timing fidelity, TAR accesses should not be implemented as an instantaneous Go map. The final model should represent:

- TAR cache hit
- TAR cache miss
- memory-hierarchy access for the TAR entry
- finite bandwidth/ports

---

## 4.4 Mandatory Feature 3: Set Filter (SF)

The Set Filter tells the RestSeg walker whether the hashed set contains any valid entry.

Conceptually:

```text
SF[set] = number_of_valid_ways
```

Translation behavior:

```text
SF[set] == 0
    => requested page cannot be in this RestSeg
    => skip expensive TAR tag matching

SF[set] > 0
    => compare requested VPN tag against TAR ways
```

The SF is important because it avoids many unnecessary TAR accesses/tag comparisons.

---

## 4.5 Mandatory Feature 4: RestSeg Walk (RSW)

On an RSW:

1. hash the VPN;
2. compute:
   - set index,
   - virtual-page tag;
3. lookup SF;
4. lookup TAR;
5. if TAR tag matches one way:
   - derive the exact PFN from RestSeg base, set, and way;
6. otherwise report `NotInRestSeg`.

The SF and TAR accesses should be modeled as parallel where appropriate.

A conceptual address derivation is:

```text
set = Hash(VPN) % NumSets
way = MatchedWay

PFN = RestSegBasePFN + set * Associativity + way
```

Use the exact layout selected by the simulator implementation consistently in allocation and lookup.

---

## 4.6 Mandatory Feature 5: TAR and SF Caches

The paper adds small GMMU-side caches for recently used TAR and SF entries.

For MGPUSim, model:

```text
TARCache
SFCache
```

with finite:

- capacity
- associativity
- lookup latency
- port count
- replacement policy

The original work models 2 KB TAR and SF caches, but the important requirement is to make their accesses explicit and configurable.

On a cache miss, fetch TAR/SF metadata through the normal cache/memory hierarchy so that translation metadata can contend with application data.

---

## 4.7 Mandatory Feature 6: Utopia Translation Flow

The critical ordering after an L1 TLB miss is:

```text
                     +--> L2 TLB lookup --------+
                     |                          |
L1 TLB miss ---------+--> RestSeg Walk(s) ------+--> translation found
                     |
                     +------------------------------> if all miss:
                                                     FlexSeg Walk
```

More precisely:

1. L1 TLB misses.
2. Start L2 TLB lookup.
3. Start RSW for each relevant RestSeg in parallel.
4. If L2 TLB or an RSW returns a translation, terminate the translation request.
5. Only if all RSWs report `NotInRestSeg` and L2 misses, start a conventional FlexSeg page-table walk.

This ordering matters.

Do not start the normal PTW at the same time as the RSW unless explicitly evaluating a non-paper variant, because doing so changes both latency and page-table traffic.

---

## 4.8 Mandatory Feature 7: RestSeg-Aware Page Allocation

Allocation requires the driver to compute the RestSeg set from the VPN and inspect available ways.

Conceptual allocation:

```text
set = Hash(VPN)

if free way exists in set:
    allocate RestSeg frame
    update TAR
    increment SF
else:
    choose RestSeg victim
    or allocate in FlexSeg
```

The allocator needs a global view of RestSeg occupancy.

A practical MGPUSim implementation can maintain this global ownership state in the driver while preserving separate timed TAR/SF structures for GPU translation.

---

## 4.9 Mandatory Feature 8: FlexSeg Page Mapping

FlexSeg pages use the baseline page-table mechanism without restriction.

A page must be resident in exactly one mapping domain:

```text
RestSeg XOR FlexSeg
```

Do not retain two simultaneously valid mappings for the same page unless a transient migration state explicitly requires them.

---

## 4.10 Mandatory Feature 9: Costly-to-Translate Page Detection

Utopia places translation-expensive pages into RestSegs.

For a faithful model, track at least:

```text
PTW frequency per page
PTW cost per page
```

The cost can be represented using the number of expensive memory accesses during a page walk, or accumulated page-walk latency if that is easier to instrument.

When both metrics cross configured thresholds, mark the page as a RestSeg migration candidate.

The paper also considers allocating pages into RestSegs directly on page faults.

A good implementation strategy is to support two modes:

```text
Mode A: Fault-based RestSeg allocation
Mode B: PTW-tracking-based migration
```

Mode A is simpler and is useful for initial validation.

Mode B should be implemented for a paper-faithful evaluation.

---

## 4.11 Mandatory Feature 10: RestSeg Replacement

When the target RestSeg set has no free way:

1. select a victim;
2. migrate the victim to FlexSeg;
3. update TAR/SF;
4. place the incoming page in the released RestSeg frame.

The paper uses SRRIP-like replacement behavior to retain high-reuse pages.

At minimum, the final implementation should avoid arbitrary random replacement because RestSeg residency policy materially affects Utopia performance.

---

## 4.12 Mandatory Feature 11: Page Migration

RestSeg-to-FlexSeg and FlexSeg-to-RestSeg migration must have non-zero timing cost.

A page migration should model:

```text
1. lock relevant translation state
2. TLB invalidation
3. TAR/SF cache invalidation
4. cache flush/writeback for the page
5. page copy
6. update PT/TAR/SF/global ownership metadata
7. unlock mapping
```

Requests to the page while migration is active should stall or be replayed according to the existing MGPUSim migration framework.

If MGPUSim already contains UVM/page-migration machinery, reuse the page-copy and fault-handling infrastructure instead of implementing an independent copy engine.

---

## 4.13 MGPUSim-Specific Adaptation: Driver as the OS

Utopia relies heavily on operating-system support.

MGPUSim does not need to emulate a complete Linux VM subsystem for this study.

Instead, map Utopia's OS responsibilities to:

- GPU driver
- memory allocator
- page-fault handler
- migration manager

The driver should be responsible for:

```text
RestSeg creation
RestSeg/FlexSeg physical-frame ownership
global RestSeg occupancy
page migration decisions
TAR/SF updates
PTE updates
shootdowns
```

The GPU-side MMU should remain responsible for the timed RSW and FSW behavior.

This separation is important: **policy belongs in the driver; translation latency belongs in the GPU timing model.**

---

## 4.14 Suggested MGPUSim Touchpoints

Likely subsystems:

```text
akita/mem/vm/tlb/*
akita/mem/vm/addresstranslator/*
mgpusim/amd/driver/internal/memoryallocator.go
mgpusim/amd/driver/pagefault.go
mgpusim/amd/protocol/*
mgpusim/amd/samples/runner/timingconfig/*
```

Expected changes:

| Subsystem | Required change |
|---|---|
| Driver allocator | Reserve and manage RestSeg/FlexSeg frames |
| Page table | Preserve normal FlexSeg mappings |
| MMU/GMMU | Add RestSeg walker |
| Translation request path | Parallel L2-TLB + RSW behavior |
| Metadata subsystem | TAR, SF, TAR cache, SF cache |
| Migration | RestSeg/FlexSeg movement |
| Shootdown | TLB + TAR/SF cache invalidation |
| Statistics | RSW hit/miss and migration behavior |

---

## 4.15 Utopia Validation Tests

1. **RestSeg lookup**
   - allocate a page in a known set/way;
   - clear TLBs;
   - verify RSW resolves the correct PFN without PTW.

2. **Empty-set filter**
   - access a VPN hashing to an empty RestSeg set;
   - verify SF prevents TAR tag matching.

3. **FlexSeg fallback**
   - place page only in FlexSeg;
   - verify RSW reports not found before FSW begins.

4. **Parallel L2-TLB and RSW**
   - construct an L2-TLB hit;
   - verify RSW can be canceled/ignored after the translation is resolved.

5. **RestSeg conflict**
   - map more VPNs to one set than its associativity;
   - verify victim selection and RestSeg-to-FlexSeg migration.

6. **Migration consistency**
   - issue requests while a page is migrating;
   - verify stale TAR/PTE entries cannot be consumed.

---