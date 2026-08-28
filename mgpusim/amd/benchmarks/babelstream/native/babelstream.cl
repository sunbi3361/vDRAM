/*
 * OpenCL kernels for BabelStream (gfx803 / GCN3)
 * Translated from babelstream.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * BabelStream measures memory bandwidth via four elementwise vector
 * operations over float arrays of length n:
 *   copy:   c[i] = a[i]
 *   scale:  b[i] = s * c[i]
 *   add:    c[i] = a[i] + b[i]
 *   triad:  a[i] = b[i] + s * c[i]
 *
 * Each kernel is a simple 1D elementwise map with a constant block size of
 * 256. Kernels read only get_group_id(0) / get_local_id(0), so no hidden
 * ABI arguments are emitted and the kernarg layout matches the HIP source.
 */
#define BLOCK_SIZE 256

__kernel void copy_kernel(
    __global const float* __restrict__ a,
    __global float* __restrict__ c,
    int n)
{
    int i = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (i < n) c[i] = a[i];
}

__kernel void scale_kernel(
    __global const float* __restrict__ c,
    __global float* __restrict__ b,
    float s,
    int n)
{
    int i = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (i < n) b[i] = s * c[i];
}

__kernel void add_kernel(
    __global const float* __restrict__ a,
    __global const float* __restrict__ b,
    __global float* __restrict__ c,
    int n)
{
    int i = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (i < n) c[i] = a[i] + b[i];
}

__kernel void triad_kernel(
    __global const float* __restrict__ b,
    __global const float* __restrict__ c,
    __global float* __restrict__ a,
    float s,
    int n)
{
    int i = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (i < n) a[i] = b[i] + s * c[i];
}
