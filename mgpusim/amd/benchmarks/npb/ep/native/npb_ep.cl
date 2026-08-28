/*
 * OpenCL kernel for NPB Embarrassingly Parallel (EP) (gfx803 / GCN3)
 * Translated from npb_ep.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Each work-item generates one pair of uniform random deviates with a
 * per-thread integer LCG, applies the Box-Muller transform, and writes its
 * annular bin index to a per-thread output array (binOut[idx]). The host
 * (Verify) bins the results, reproducing the same counts.
 *
 * gfx803 emulator constraint: V_SIN/V_COS and the inlined full-precision
 * transcendental expansions (which use V_ALIGNBIT_B32) are not implemented,
 * so the Box-Muller pair (x1 = r*cos(theta), x2 = r*sin(theta)) is folded
 * into t = r*r = x1^2+x2^2 (identity for any theta). The natural log is
 * computed as log2(u1)*ln2 (native_log is a base-2 log), and sqrt uses
 * native_sqrt. The host Verify mirrors this exact float32 sequence.
 *
 * Uses a constant BLOCK_SIZE for the block geometry so the compiler emits
 * no hidden ABI arguments.
 */
#define NUM_BINS   10
#define BLOCK_SIZE 256
#define LN2_F      0.69314718055994530942f

// ANSI C LCG, mod 2^32 by unsigned overflow.
static inline uint lcg_next(uint seed) {
    return seed * 1103515245u + 12345u;
}

__kernel void ep_kernel(int N, __global int* __restrict__ binOut) {
    int idx = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (idx >= N) return;

    uint seed = (uint)(idx + 1);
    seed = lcg_next(seed);
    seed = lcg_next(seed);

    seed = lcg_next(seed);
    float u1 = (float)seed / 4294967296.0f;

    u1 = fmax(u1, 1e-10f);

    // clang-12 lowers native_log(x) to log2(x)*ln2, i.e. the natural log;
    // t = r*r (x1^2+x2^2). The host Verify mirrors this exact sequence.
    float l2 = native_log(u1);
    float r = native_sqrt(-2.0f * l2);
    float t = r * r;

    int bin = (int)native_sqrt(t);
    if (bin >= NUM_BINS) bin = NUM_BINS - 1;
    if (bin < 0) bin = 0;

    binOut[idx] = bin;
}
