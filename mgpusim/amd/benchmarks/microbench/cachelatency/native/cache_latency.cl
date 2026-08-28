/*
 * OpenCL kernel for the cache_latency microbenchmark (gfx803 / GCN3)
 * Translated from cache_latency.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Single-work-item pointer-chasing latency measurement: one thread walks a
 * linked-list-style index chain where each array element holds the index of
 * the next element to visit. Because each load depends on the previous result
 * (a true data dependency), the loads cannot overlap, directly exposing the
 * per-access memory latency rather than bandwidth.
 *
 * Launched with a single work-item (grid 1, block 1), matching the HIP
 * source. The chase index is derived from get_local_id(0) so the compiler
 * keeps the loads in VECTOR registers (FLAT loads) instead of scalarizing
 * the loop into SMEM loads; with a work-group of 1 only lane 0 runs, so
 * lane 0 chases from start_idx and its result is stored under a
 * get_local_id(0) == 0 guard.
 */
__kernel void pointer_chase_kernel(
    __global const uint* __restrict__ arr,
    uint start_idx,
    uint num_accesses,
    __global uint* result)
{
    if (get_group_id(0) != 0) return;

    uint idx = start_idx + get_local_id(0);

    for (uint i = 0; i < num_accesses; ++i) {
        idx = arr[idx];
    }

    if (get_local_id(0) == 0) {
        *result = idx;
    }
}
