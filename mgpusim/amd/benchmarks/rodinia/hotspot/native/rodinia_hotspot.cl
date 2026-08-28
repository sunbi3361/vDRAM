/*
 * OpenCL kernel for Rodinia Hotspot (gfx803 / GCN3)
 * Translated from rodinia_hotspot.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Iterative 2D stencil thermal simulation. Each cell's temperature is updated
 * from its four neighbors (N/S/E/W), the local power density, and thermal
 * resistances.
 *
 * Uses a constant BLOCK_SIZE (not blockDim) for the block geometry. One
 * thread per grid cell. Kernel argument order/types match the HIP source.
 */
#define BLOCK_SIZE 16
#define AMB_TEMP   80.0f

__kernel void hotspot_kernel(
    __global const float* __restrict__ temp_src,
    __global float* __restrict__ temp_dst,
    __global const float* __restrict__ power,
    int grid_cols, int grid_rows,
    float step_div_cap,
    float Rx_1, float Ry_1, float Rz_1)
{
    int col = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int row = get_group_id(1) * BLOCK_SIZE + get_local_id(1);

    if (col < grid_cols && row < grid_rows) {
        int idx = row * grid_cols + col;

        float temp_c = temp_src[idx];
        float temp_n = (row > 0)             ? temp_src[(row - 1) * grid_cols + col] : temp_c;
        float temp_s = (row < grid_rows - 1) ? temp_src[(row + 1) * grid_cols + col] : temp_c;
        float temp_w = (col > 0)             ? temp_src[row * grid_cols + (col - 1)] : temp_c;
        float temp_e = (col < grid_cols - 1) ? temp_src[row * grid_cols + (col + 1)] : temp_c;

        float delta = step_div_cap *
            (power[idx]
             + (temp_n + temp_s - 2.0f * temp_c) * Ry_1
             + (temp_w + temp_e - 2.0f * temp_c) * Rx_1
             + (AMB_TEMP - temp_c) * Rz_1);

        temp_dst[idx] = temp_c + delta;
    }
}
