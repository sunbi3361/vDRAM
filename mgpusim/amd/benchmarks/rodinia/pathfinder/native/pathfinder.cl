/*
 * OpenCL kernel for Rodinia PathFinder (gfx803 / GCN3)
 * Translated from rodinia_pathfinder.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Dynamic-programming sweep: one kernel launch processes one row, using the
 * previous row's costs (gpuSrc) to produce the current row (gpuDst).
 *
 * The out-of-bounds neighbors use a bounds-checked select instead of the
 * INT_MAX sentinel: clang materializes INT_MAX as v_bfrev_b32_e32 (VOP1
 * opcode 44), which the GCN3 emulator does not implement. The select is
 * semantically identical to min(INT_MAX, ...) because every cell has at
 * least the in-bounds "above" neighbor.
 */
#define BLOCK_SIZE 256

__kernel void dynproc_kernel(
    __global const int* __restrict__ gpuWall,
    __global const int* __restrict__ gpuSrc,
    __global int*       __restrict__ gpuDst,
    int cols,
    int t)
{
    int col = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (col >= cols) return;

    int above = gpuSrc[col];
    int min3 = above;

    if (col > 0) {
        int left = gpuSrc[col - 1];
        if (left < min3) min3 = left;
    }
    if (col < cols - 1) {
        int right = gpuSrc[col + 1];
        if (right < min3) min3 = right;
    }

    gpuDst[col] = gpuWall[t * cols + col] + min3;
}
