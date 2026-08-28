/*
 * OpenCL kernels for PolyBench FDTD-2D (gfx803 / GCN3)
 * Translated from polybench_fdtd2d.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * 2D Finite Difference Time Domain electromagnetic simulation over three
 * NX x NY field arrays: ex, ey, hz.  Per time step:
 *   ex[0][j]  = 0;  ex[i][j] += 0.5*(hz[i][j] - hz[i-1][j])   (i>=1)
 *   ey[i][0]  = 0;  ey[i][j] += 0.5*(hz[i][j] - hz[i][j-1])   (j>=1)
 *   hz[i][j] -= 0.7*(ex[i][j+1]-ex[i][j] + ey[i+1][j]-ey[i][j]) (i<NX-1,j<NY-1)
 *
 * The block dimension is a constant (BLOCK = 16) instead of blockDim.x/y so
 * the compiler emits no hidden ABI arguments.
 */
#define BLOCK 16

// Kernel 1: Update ex
__kernel void fdtd_update_ex(
    __global float* __restrict__ ex,
    __global const float* __restrict__ hz,
    int NX, int NY)
{
    int j = get_group_id(0) * BLOCK + get_local_id(0);
    int i = get_group_id(1) * BLOCK + get_local_id(1);

    if (i >= NX || j >= NY) return;

    if (i == 0) {
        ex[0 * NY + j] = 0.0f;
    } else {
        ex[i * NY + j] += 0.5f * (hz[i * NY + j] - hz[(i - 1) * NY + j]);
    }
}

// Kernel 2: Update ey
__kernel void fdtd_update_ey(
    __global float* __restrict__ ey,
    __global const float* __restrict__ hz,
    int NX, int NY)
{
    int j = get_group_id(0) * BLOCK + get_local_id(0);
    int i = get_group_id(1) * BLOCK + get_local_id(1);

    if (i >= NX || j >= NY) return;

    if (j == 0) {
        ey[i * NY + 0] = 0.0f;
    } else {
        ey[i * NY + j] += 0.5f * (hz[i * NY + j] - hz[i * NY + (j - 1)]);
    }
}

// Kernel 3: Update hz
__kernel void fdtd_update_hz(
    __global const float* __restrict__ ex,
    __global const float* __restrict__ ey,
    __global float* __restrict__ hz,
    int NX, int NY)
{
    int j = get_group_id(0) * BLOCK + get_local_id(0);
    int i = get_group_id(1) * BLOCK + get_local_id(1);

    if (i >= NX - 1 || j >= NY - 1) return;

    hz[i * NY + j] -= 0.7f * (ex[i * NY + (j + 1)] - ex[i * NY + j] +
                               ey[(i + 1) * NY + j] - ey[i * NY + j]);
}
