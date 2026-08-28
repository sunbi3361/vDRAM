/*
 * OpenCL kernel for Rodinia HotSpot3D (gfx803 / GCN3)
 * Translated from rodinia_hotspot3d.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * 3D stencil thermal simulation. Each cell's temperature is updated from its
 * six neighbors (+/-x, +/-y, +/-z), local power density, and thermal
 * resistances in an NxNxN grid. Boundaries are clamped (a cell at the edge
 * uses its own value for the missing neighbor).
 *
 * The launch geometry uses a constant BLOCK_DIM (8) square for the (x, y)
 * block. The grid is 3D: one work-item per (x, y, z) cell, with z = group_id.z
 * (workgroup z dim == 1). Kernel argument order/types match the HIP source.
 */
#define BLOCK_DIM 8

__kernel void hotspot3d_kernel(
    __global const float* __restrict__ temp_src,
    __global float* __restrict__ temp_dst,
    __global const float* __restrict__ power,
    int nx, int ny, int nz,
    float step_div_cap,
    float Rx_1, float Ry_1, float Rz_1,
    float Ra_1, float amb_temp)
{
    int x = get_group_id(0) * BLOCK_DIM + get_local_id(0);
    int y = get_group_id(1) * BLOCK_DIM + get_local_id(1);
    int z = get_group_id(2);   // one work-item per (x, y, z) cell; workgroup z dim == 1

    if (x >= nx || y >= ny || z >= nz) return;

    int plane = ny * nx;
    int idx = z * plane + y * nx + x;

    float tc = temp_src[idx];

    // +/-x neighbors (clamped boundary)
    float txm = (x > 0)      ? temp_src[z * plane + y * nx + (x - 1)] : tc;
    float txp = (x < nx - 1) ? temp_src[z * plane + y * nx + (x + 1)] : tc;

    // +/-y neighbors (clamped boundary)
    float tym = (y > 0)      ? temp_src[z * plane + (y - 1) * nx + x] : tc;
    float typ = (y < ny - 1) ? temp_src[z * plane + (y + 1) * nx + x] : tc;

    // +/-z neighbors (clamped boundary)
    float tzm = (z > 0)      ? temp_src[(z - 1) * plane + y * nx + x] : tc;
    float tzp = (z < nz - 1) ? temp_src[(z + 1) * plane + y * nx + x] : tc;

    float delta = step_div_cap * (
        power[idx]
        + (txm + txp - 2.0f * tc) * Rx_1
        + (tym + typ - 2.0f * tc) * Ry_1
        + (tzm + tzp - 2.0f * tc) * Rz_1
        + (amb_temp - tc) * Ra_1
    );

    temp_dst[idx] = tc + delta;
}
