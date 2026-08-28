/*
 * OpenCL kernels for PolyBench Correlation (gfx803 / GCN3)
 * Translated from polybench_correlation.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Computes a correlation matrix from an MxN data matrix in four steps:
 *   1. mean_kernel        - column means
 *   2. stddev_kernel      - column standard deviations
 *   3. normalize_kernel   - normalize each element
 *   4. correlation_kernel - tiled (TILE_SIZE x TILE_SIZE) matmul of
 *                           normalized^T * normalized
 *
 * The 1D kernels use a CONSTANT block size (BLOCK_SIZE) and the tiled
 * kernel a CONSTANT TILE_SIZE instead of blockDim.x/y so the compiler
 * emits no hidden ABI arguments.
 */
#define BLOCK_SIZE 256
#define TILE_SIZE 16

__kernel void mean_kernel(
    __global const float* __restrict__ data,
    __global float* __restrict__ mean,
    int M,
    int N)
{
    int j = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (j >= N) return;

    float sum = 0.0f;
    for (int i = 0; i < M; ++i) {
        sum += data[i * N + j];
    }
    mean[j] = sum / (float)M;
}

__kernel void stddev_kernel(
    __global const float* __restrict__ data,
    __global const float* __restrict__ mean,
    __global float* __restrict__ stddev,
    int M,
    int N)
{
    int j = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (j >= N) return;

    float m = mean[j];
    float sum = 0.0f;
    for (int i = 0; i < M; ++i) {
        float diff = data[i * N + j] - m;
        sum += diff * diff;
    }
    float s = sqrt(sum / (float)M);
    stddev[j] = (s < 1e-12f) ? 1.0f : s;
}

__kernel void normalize_kernel(
    __global float* __restrict__ data,
    __global const float* __restrict__ mean,
    __global const float* __restrict__ stddev,
    int M,
    int N)
{
    int idx = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (idx >= M * N) return;

    int j = idx % N;
    float sqrt_m = sqrt((float)M);
    data[idx] = (data[idx] - mean[j]) / (sqrt_m * stddev[j]);
}

__kernel void correlation_kernel(
    __global const float* __restrict__ data,
    __global float* __restrict__ corr,
    int M,
    int N)
{
    __local float sA[TILE_SIZE][TILE_SIZE];
    __local float sB[TILE_SIZE][TILE_SIZE];

    int row = get_group_id(1) * TILE_SIZE + get_local_id(1);
    int col = get_group_id(0) * TILE_SIZE + get_local_id(0);

    float sum = 0.0f;
    int num_tiles = (M + TILE_SIZE - 1) / TILE_SIZE;

    for (int t = 0; t < num_tiles; ++t) {
        int k_a = t * TILE_SIZE + get_local_id(0);
        int k_b = t * TILE_SIZE + get_local_id(1);

        sA[get_local_id(1)][get_local_id(0)] = (row < N && k_a < M) ? data[k_a * N + row] : 0.0f;
        sB[get_local_id(1)][get_local_id(0)] = (k_b < M && col < N) ? data[k_b * N + col] : 0.0f;

        barrier(CLK_LOCAL_MEM_FENCE);

        for (int k = 0; k < TILE_SIZE; ++k) {
            sum += sA[get_local_id(1)][k] * sB[k][get_local_id(0)];
        }

        barrier(CLK_LOCAL_MEM_FENCE);
    }

    if (row < N && col < N) {
        if (row == col)
            corr[row * N + col] = 1.0f;
        else
            corr[row * N + col] = sum;
    }
}
