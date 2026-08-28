/*
 * OpenCL kernel for PolyBench Jacobi-2D stencil (gfx803 / GCN3)
 * Translated from polybench_jacobi2d.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * One time-step of the 2D Jacobi stencil on an NxN grid:
 *   B[i][j] = (A[i-1][j] + A[i+1][j] + A[i][j-1] + A[i][j+1] + A[i][j]) * 0.2
 * Only interior points (i=1..N-2, j=1..N-2) are updated; boundaries stay 0.
 *
 * Uses a constant BLOCK_DIM for the block geometry so the compiler emits no
 * hidden ABI arguments (kernarg_segment_size = 20).
 */
#define BLOCK_DIM 16

__kernel void jacobi2d_kernel(
    __global const float* __restrict__ A,
    __global float* __restrict__ B,
    int N)
{
    int i = get_group_id(1) * BLOCK_DIM + get_local_id(1) + 1;  // interior row
    int j = get_group_id(0) * BLOCK_DIM + get_local_id(0) + 1;  // interior col
    if (i >= N - 1 || j >= N - 1) return;
    B[i * N + j] = (A[(i - 1) * N + j] + A[(i + 1) * N + j] +
                    A[i * N + (j - 1)] + A[i * N + (j + 1)] +
                    A[i * N + j]) * 0.2f;
}
