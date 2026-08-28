/*
 * OpenCL kernels for the Rodinia Gaussian elimination benchmark (gfx803 / GCN3)
 * Translated from rodinia_gaussian.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Two kernels perform GPU forward elimination of a dense NxN system Ax=b:
 *   fan1: compute multipliers m[i][k] = a[i][k] / a[k][k] for pivot column t
 *   fan2: update submatrix below pivot row t, and the rhs vector b
 *
 * Block dimensions are CONSTANT literals (BLOCK1D for fan1, BLOCK2D x BLOCK2D
 * for fan2). Kernel argument order/types match the HIP source exactly.
 */
#define BLOCK1D 256
#define BLOCK2D 16

// fan1 - compute multipliers for pivot column t.
// Each thread handles one row below the pivot.
__kernel void fan1(__global float* __restrict__ m,
                   __global const float* __restrict__ a,
                   int Size, int t)
{
    int tid = get_group_id(0) * BLOCK1D + get_local_id(0);
    if (tid < Size - 1 - t) {
        int row = t + 1 + tid;
        m[row * Size + t] = a[row * Size + t] / a[t * Size + t];
    }
}

// fan2 - eliminate pivot column from submatrix below pivot row t.
// Each (col, row) thread updates one submatrix element; col==0 also updates b.
__kernel void fan2(__global const float* __restrict__ m,
                   __global float* __restrict__ a,
                   __global float* __restrict__ b,
                   int Size, int t)
{
    int col = get_group_id(0) * BLOCK2D + get_local_id(0); // relative column index
    int row = get_group_id(1) * BLOCK2D + get_local_id(1); // relative row index
    int remaining = Size - t - 1;

    if (col < remaining && row < remaining) {
        int abs_row = t + 1 + row;
        int abs_col = t + 1 + col;
        a[abs_row * Size + abs_col] -= m[abs_row * Size + t] * a[t * Size + abs_col];
        if (col == 0) {
            b[abs_row] -= m[abs_row * Size + t] * b[t];
        }
    }
}
