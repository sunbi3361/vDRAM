/*
 * OpenCL kernel for PolyBench SYR2K (gfx803 / GCN3)
 * Translated from polybench_syr2k.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Symmetric rank-2k update: C = alpha*A*B^T + alpha*B*A^T + beta*C
 * A and B are [N x M] matrices, C is [N x N].
 * Tiled with local memory (TILE_SIZE x TILE_SIZE), one thread per C element.
 *
 * Uses a constant TILE_SIZE for the block geometry so the compiler emits no
 * hidden ABI arguments (kernarg_segment_size = 40).
 */
#define TILE_SIZE 16

__kernel void polybench_syr2k_kernel(
    __global const float* __restrict__ A,
    __global const float* __restrict__ B,
    __global float* __restrict__ C,
    int N,
    int M,
    float alpha,
    float beta)
{
    __local float sA_row[TILE_SIZE][TILE_SIZE];
    __local float sB_row[TILE_SIZE][TILE_SIZE];
    __local float sA_col[TILE_SIZE][TILE_SIZE];
    __local float sB_col[TILE_SIZE][TILE_SIZE];

    int row = get_group_id(1) * TILE_SIZE + get_local_id(1);
    int col = get_group_id(0) * TILE_SIZE + get_local_id(0);

    float sum = 0.0f;
    int num_tiles = (M + TILE_SIZE - 1) / TILE_SIZE;

    for (int t = 0; t < num_tiles; ++t) {
        int k = t * TILE_SIZE + get_local_id(0);

        // Load A and B rows for the row-block
        sA_row[get_local_id(1)][get_local_id(0)] = (row < N && k < M) ? A[row * M + k] : 0.0f;
        sB_row[get_local_id(1)][get_local_id(0)] = (row < N && k < M) ? B[row * M + k] : 0.0f;

        // Load A and B rows for the col-block
        int col_row = get_group_id(0) * TILE_SIZE + get_local_id(1);
        int k2 = t * TILE_SIZE + get_local_id(0);
        sA_col[get_local_id(1)][get_local_id(0)] = (col_row < N && k2 < M) ? A[col_row * M + k2] : 0.0f;
        sB_col[get_local_id(1)][get_local_id(0)] = (col_row < N && k2 < M) ? B[col_row * M + k2] : 0.0f;

        barrier(CLK_LOCAL_MEM_FENCE);

        for (int kk = 0; kk < TILE_SIZE; ++kk) {
            // A*B^T: A[row][k]*B[col][k]  +  B*A^T: B[row][k]*A[col][k]
            sum += sA_row[get_local_id(1)][kk] * sB_col[get_local_id(0)][kk]
                 + sB_row[get_local_id(1)][kk] * sA_col[get_local_id(0)][kk];
        }

        barrier(CLK_LOCAL_MEM_FENCE);
    }

    if (row < N && col < N) {
        C[row * N + col] = alpha * sum + beta * C[row * N + col];
    }
}
