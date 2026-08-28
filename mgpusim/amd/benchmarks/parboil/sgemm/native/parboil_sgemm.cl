/*
 * OpenCL kernel for Parboil SGEMM (gfx803 / GCN3)
 * Translated from parboil_sgemm.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Tiled single-precision GEMM: C = alpha*A*B + beta*C for NxN square
 * matrices, one thread per C element with shared-memory tiles (TILE x TILE).
 *
 * Uses a constant TILE for the block geometry so the compiler emits no
 * hidden ABI arguments (kernarg_segment_size = 36).
 */
#define TILE 16

__kernel void sgemm_kernel(
    __global const float* __restrict__ A,
    __global const float* __restrict__ B,
    __global float* __restrict__ C,
    int N,
    float alpha,
    float beta)
{
    __local float tA[TILE][TILE];
    __local float tB[TILE][TILE];

    int row = get_group_id(1) * TILE + get_local_id(1);
    int col = get_group_id(0) * TILE + get_local_id(0);

    float sum = 0.0f;
    int num_tiles = (N + TILE - 1) / TILE;

    for (int t = 0; t < num_tiles; ++t) {
        int aCol = t * TILE + get_local_id(0);
        int bRow = t * TILE + get_local_id(1);

        tA[get_local_id(1)][get_local_id(0)] = (row < N && aCol < N) ? A[row * N + aCol] : 0.0f;
        tB[get_local_id(1)][get_local_id(0)] = (bRow < N && col < N) ? B[bRow * N + col] : 0.0f;

        barrier(CLK_LOCAL_MEM_FENCE);

        for (int k = 0; k < TILE; ++k) {
            sum += tA[get_local_id(1)][k] * tB[k][get_local_id(0)];
        }

        barrier(CLK_LOCAL_MEM_FENCE);
    }

    if (row < N && col < N) {
        C[row * N + col] = alpha * sum + beta * C[row * N + col];
    }
}
