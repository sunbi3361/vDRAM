/*
 * OpenCL kernels for PolyBench 3MM (gfx803 / GCN3)
 * Translated from polybench_3mm.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Three chained matrix multiplications:
 *   E = A * B  (NI×NK · NK×NJ → NI×NJ)
 *   F = C * D  (NJ×NM · NM×NL → NJ×NL)
 *   G = E * F  (NI×NJ · NJ×NL → NI×NL)
 *
 * Each work-item computes one output element with a simple dot-product loop.
 * Block dimensions are CONSTANT 16×16, so the compiler emits no hidden ABI
 * arguments.
 */
#define BLOCK_SIZE 16

// Kernel 1: E = A * B   (A: NI×NK, B: NK×NJ → E: NI×NJ)
__kernel void mm3_kernel1(__global const float* __restrict__ A,
                          __global const float* __restrict__ B,
                          __global float* __restrict__       E,
                          int NI, int NK, int NJ) {
    int j = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int i = get_group_id(1) * BLOCK_SIZE + get_local_id(1);
    if (i < NI && j < NJ) {
        float sum = 0.0f;
        for (int k = 0; k < NK; k++)
            sum += A[i * NK + k] * B[k * NJ + j];
        E[i * NJ + j] = sum;
    }
}

// Kernel 2: F = C * D   (C: NJ×NM, D: NM×NL → F: NJ×NL)
__kernel void mm3_kernel2(__global const float* __restrict__ C,
                          __global const float* __restrict__ D,
                          __global float* __restrict__       F,
                          int NJ, int NM, int NL) {
    int l = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int j = get_group_id(1) * BLOCK_SIZE + get_local_id(1);
    if (j < NJ && l < NL) {
        float sum = 0.0f;
        for (int m = 0; m < NM; m++)
            sum += C[j * NM + m] * D[m * NL + l];
        F[j * NL + l] = sum;
    }
}

// Kernel 3: G = E * F   (E: NI×NJ, F: NJ×NL → G: NI×NL)
__kernel void mm3_kernel3(__global const float* __restrict__ E,
                          __global const float* __restrict__ F,
                          __global float* __restrict__       G,
                          int NI, int NJ, int NL) {
    int l = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int i = get_group_id(1) * BLOCK_SIZE + get_local_id(1);
    if (i < NI && l < NL) {
        float sum = 0.0f;
        for (int j = 0; j < NJ; j++)
            sum += E[i * NJ + j] * F[j * NL + l];
        G[i * NL + l] = sum;
    }
}
