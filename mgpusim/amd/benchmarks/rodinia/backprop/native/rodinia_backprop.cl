/*
 * OpenCL kernels for the Rodinia Backpropagation benchmark (gfx803 / GCN3)
 * Translated from rodinia_backprop.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Two-layer fully-connected neural net forward + backward pass:
 *   INPUT_N -> HIDDEN_N -> OUTPUT_N
 *
 * Six kernels: forward_hidden, forward_output, backward_output_delta,
 * backward_hidden_delta, update_w1 (2D), update_w2.
 *
 * The 1D kernels use a constant block width (BLOCK_SZ) and the 2D update_w1
 * uses a constant 16x16 block. Kernel argument order/types match the HIP
 * source exactly.
 */
#define BLOCK_SZ 256
#define TILE2D   16

static inline float sigmoid_d(float x)
{
    return 1.0f / (1.0f + exp(-x));
}

// hidden[j] = sigmoid(b1[j] + sum_i input[i]*w1[i*hidden_n+j])
__kernel void forward_hidden(__global const float* __restrict__ input,
                             __global const float* __restrict__ w1,
                             __global const float* __restrict__ b1,
                             __global float* __restrict__ hidden,
                             int input_n, int hidden_n)
{
    int j = get_group_id(0) * BLOCK_SZ + get_local_id(0);
    if (j >= hidden_n) return;
    float sum = b1[j];
    for (int i = 0; i < input_n; i++)
        sum += input[i] * w1[i * hidden_n + j];
    hidden[j] = sigmoid_d(sum);
}

// output[k] = sigmoid(b2[k] + sum_j hidden[j]*w2[j*output_n+k])
__kernel void forward_output(__global const float* __restrict__ hidden,
                             __global const float* __restrict__ w2,
                             __global const float* __restrict__ b2,
                             __global float* __restrict__ output,
                             int hidden_n, int output_n)
{
    int k = get_group_id(0) * BLOCK_SZ + get_local_id(0);
    if (k >= output_n) return;
    float sum = b2[k];
    for (int j = 0; j < hidden_n; j++)
        sum += hidden[j] * w2[j * output_n + k];
    output[k] = sigmoid_d(sum);
}

// delta_out[k] = output[k]*(1-output[k])*(target[k]-output[k])
__kernel void backward_output_delta(__global const float* __restrict__ output,
                                    __global const float* __restrict__ target,
                                    __global float* __restrict__ delta_out,
                                    int output_n)
{
    int k = get_group_id(0) * BLOCK_SZ + get_local_id(0);
    if (k >= output_n) return;
    float o = output[k];
    delta_out[k] = o * (1.0f - o) * (target[k] - o);
}

// delta_hid[j] = hidden[j]*(1-hidden[j])*sum_k(w2[j*output_n+k]*delta_out[k])
__kernel void backward_hidden_delta(__global const float* __restrict__ hidden,
                                    __global const float* __restrict__ w2,
                                    __global const float* __restrict__ delta_out,
                                    __global float* __restrict__ delta_hid,
                                    int hidden_n, int output_n)
{
    int j = get_group_id(0) * BLOCK_SZ + get_local_id(0);
    if (j >= hidden_n) return;
    float h = hidden[j];
    float sum = 0.0f;
    for (int k = 0; k < output_n; k++)
        sum += w2[j * output_n + k] * delta_out[k];
    delta_hid[j] = h * (1.0f - h) * sum;
}

// w1[i*hidden_n+j] += lr * input[i] * delta_hid[j]   (2D grid: x->i, y->j)
__kernel void update_w1(__global float* __restrict__ w1,
                        __global const float* __restrict__ input,
                        __global const float* __restrict__ delta_hid,
                        int input_n, int hidden_n, float lr)
{
    int i = get_group_id(0) * TILE2D + get_local_id(0);
    int j = get_group_id(1) * TILE2D + get_local_id(1);
    if (i >= input_n || j >= hidden_n) return;
    w1[i * hidden_n + j] += lr * input[i] * delta_hid[j];
}

// w2[j*output_n+k] += lr * hidden[j] * delta_out[k]
__kernel void update_w2(__global float* __restrict__ w2,
                        __global const float* __restrict__ hidden,
                        __global const float* __restrict__ delta_out,
                        int hidden_n, int output_n, float lr)
{
    int j = get_group_id(0) * BLOCK_SZ + get_local_id(0);
    if (j >= hidden_n) return;
    for (int k = 0; k < output_n; k++)
        w2[j * output_n + k] += lr * hidden[j] * delta_out[k];
}
