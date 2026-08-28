/*
 * OpenCL kernels for PolyBench MVT (gfx803 / GCN3)
 * Translated from polybench_mvt.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Computes:
 *   x1 = A   * y1  (mvt_kernel1: x1[i] = sum_j A[i,j] * y1[j])
 *   x2 = A^T * y2  (mvt_kernel2: x2[j] = sum_i A[i,j] * y2[i])
 * for an N x N matrix A and N-vectors x1, x2, y1, y2.
 *
 * Each kernel is 1D: one thread per output element. A constant BLOCK_SIZE
 * is used (not get_local_size(0)) so the compiler emits no hidden ABI
 * arguments (kernarg_segment_size = 28).
 */
#define BLOCK_SIZE 256

__kernel void mvt_kernel1(
    __global const float* __restrict__ A,
    __global const float* __restrict__ y1,
    __global float* __restrict__ x1,
    int n)
{
    int i = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (i >= n) return;
    float sum = 0.0f;
    for (int j = 0; j < n; j++)
        sum += A[i * n + j] * y1[j];
    x1[i] = sum;
}

__kernel void mvt_kernel2(
    __global const float* __restrict__ A,
    __global const float* __restrict__ y2,
    __global float* __restrict__ x2,
    int n)
{
    int j = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (j >= n) return;
    float sum = 0.0f;
    for (int i = 0; i < n; i++)
        sum += A[i * n + j] * y2[i];
    x2[j] = sum;
}
