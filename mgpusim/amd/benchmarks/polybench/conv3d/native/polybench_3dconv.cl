/*
 * OpenCL kernel for PolyBench 3D Convolution (gfx803 / GCN3)
 * Translated from polybench_3dconv.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * 3D convolution over an NxNxN volume with a small 3D filter
 * (filter_size x filter_size x filter_size). Each output point is the
 * weighted sum of its neighborhood, one work-item per output element.
 * BLOCK_SIZE is a compile-time constant so the compiler emits no hidden
 * ABI arguments.
 */
#define BLOCK_SIZE 8

__kernel void conv3d_kernel(
    __global const float* __restrict__ input,
    __global const float* __restrict__ filter,
    __global float* __restrict__ output,
    int N,
    int filter_size)
{
    int k = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int j = get_group_id(1) * BLOCK_SIZE + get_local_id(1);
    int i = get_group_id(2) * BLOCK_SIZE + get_local_id(2);

    if (i >= N || j >= N || k >= N) return;

    int half_f = filter_size / 2;
    float sum = 0.0f;

    for (int fi = 0; fi < filter_size; ++fi) {
        int ii = i - half_f + fi;
        if (ii < 0 || ii >= N) continue;
        for (int fj = 0; fj < filter_size; ++fj) {
            int jj = j - half_f + fj;
            if (jj < 0 || jj >= N) continue;
            for (int fk = 0; fk < filter_size; ++fk) {
                int kk = k - half_f + fk;
                if (kk < 0 || kk >= N) continue;
                sum += input[((size_t)ii * N + jj) * N + kk]
                     * filter[((size_t)fi * filter_size + fj) * filter_size + fk];
            }
        }
    }

    output[((size_t)i * N + j) * N + k] = sum;
}
