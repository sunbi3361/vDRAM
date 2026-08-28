/*
 * OpenCL kernel for PolyBench 2D Convolution (gfx803 / GCN3)
 * Translated from polybench_2dconv.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Applies a fixed 3x3 PolyBench coefficient stencil to an NI x NJ matrix A,
 * producing output B. One work-item per output element. BLOCK_SIZE is a
 * compile-time constant so the compiler emits no hidden ABI arguments.
 */
#define BLOCK_SIZE 16

__kernel void convolution2D_kernel(
    __global const float* __restrict__ A,
    __global float* __restrict__ B,
    int NI,
    int NJ)
{
    int j = (int)(get_group_id(0) * BLOCK_SIZE + get_local_id(0));
    int i = (int)(get_group_id(1) * BLOCK_SIZE + get_local_id(1));

    if (i < 1 || i >= NI - 1 || j < 1 || j >= NJ - 1)
        return;

    // PolyBench fixed coefficients
    const float c00 = 0.8f, c01 = 0.2f, c02 = 0.3f;
    const float c10 = 0.2f, c11 = 0.7f, c12 = 0.4f;
    const float c20 = 0.1f, c21 = 0.2f, c22 = 0.5f;

    B[i * NJ + j] =
        c00 * A[(i - 1) * NJ + (j - 1)] +
        c01 * A[(i - 1) * NJ +  j     ] +
        c02 * A[(i - 1) * NJ + (j + 1)] +
        c10 * A[ i      * NJ + (j - 1)] +
        c11 * A[ i      * NJ +  j     ] +
        c12 * A[ i      * NJ + (j + 1)] +
        c20 * A[(i + 1) * NJ + (j - 1)] +
        c21 * A[(i + 1) * NJ +  j     ] +
        c22 * A[(i + 1) * NJ + (j + 1)];
}
