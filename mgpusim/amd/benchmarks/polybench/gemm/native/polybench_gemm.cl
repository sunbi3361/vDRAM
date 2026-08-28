/*
 * OpenCL kernel for PolyBench GEMM (gfx803 / GCN3)
 * Translated from polybench_gemm.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Dense matrix multiply: C = alpha*A*B + beta*C for NxN square matrices.
 * Tiled with local memory (TILE_SIZE x TILE_SIZE), one thread per C element.
 */
#define TILE_SIZE 16

__kernel void polybench_gemm_kernel(
    __global const float* __restrict__ A,
    __global const float* __restrict__ B,
    __global float* __restrict__ C,
    int N,
    float alpha,
    float beta)
{
    __local float sA[TILE_SIZE][TILE_SIZE];
    __local float sB[TILE_SIZE][TILE_SIZE];

    int row = get_group_id(1) * TILE_SIZE + get_local_id(1);
    int col = get_group_id(0) * TILE_SIZE + get_local_id(0);

    float sum = 0.0f;
    int num_tiles = (N + TILE_SIZE - 1) / TILE_SIZE;

    for (int t = 0; t < num_tiles; ++t) {
        int aCol = t * TILE_SIZE + get_local_id(0);
        int bRow = t * TILE_SIZE + get_local_id(1);

        sA[get_local_id(1)][get_local_id(0)] =
            (row < N && aCol < N) ? A[row * N + aCol] : 0.0f;
        sB[get_local_id(1)][get_local_id(0)] =
            (bRow < N && col < N) ? B[bRow * N + col] : 0.0f;

        barrier(CLK_LOCAL_MEM_FENCE);

        for (int k = 0; k < TILE_SIZE; ++k) {
            sum += sA[get_local_id(1)][k] * sB[k][get_local_id(0)];
        }

        barrier(CLK_LOCAL_MEM_FENCE);
    }

    if (row < N && col < N) {
        C[row * N + col] = alpha * sum + beta * C[row * N + col];
    }
}
