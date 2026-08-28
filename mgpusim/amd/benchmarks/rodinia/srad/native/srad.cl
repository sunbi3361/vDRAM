/*
 * OpenCL kernels for Rodinia SRAD (gfx803 / GCN3)
 * Translated from rodinia_srad.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Speckle-Reducing Anisotropic Diffusion (SRAD): iterative 2D image smoothing
 * using a Perona-Malik anisotropic diffusion scheme. Two kernels:
 *   srad1: directional gradients (dN/dS/dW/dE) and diffusion coefficient c.
 *   srad2: image update using the diffusion coefficients.
 */
#define BLOCK_SIZE 16

__kernel void srad1(
    __global const float* __restrict__ J,
    __global float* __restrict__ dN,
    __global float* __restrict__ dS,
    __global float* __restrict__ dW,
    __global float* __restrict__ dE,
    __global float* __restrict__ c,
    int rows, int cols, float q0sqr)
{
    int col = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int row = get_group_id(1) * BLOCK_SIZE + get_local_id(1);

    if (row >= rows || col >= cols) return;

    int idx = row * cols + col;

    int iN = (row > 0)        ? (row - 1) : 0;
    int iS = (row < rows - 1) ? (row + 1) : (rows - 1);
    int jW = (col > 0)        ? (col - 1) : 0;
    int jE = (col < cols - 1) ? (col + 1) : (cols - 1);

    float Jc = J[idx];

    float dn = J[iN * cols + col] - Jc;
    float ds = J[iS * cols + col] - Jc;
    float dw = J[row * cols + jW] - Jc;
    float de = J[row * cols + jE] - Jc;

    dN[idx] = dn;
    dS[idx] = ds;
    dW[idx] = dw;
    dE[idx] = de;

    float G2   = (dn*dn + ds*ds + dw*dw + de*de) / (Jc * Jc);
    float L    = (dn + ds + dw + de) / Jc;
    float num  = (0.5f * G2) - ((1.0f / 16.0f) * (L * L));
    float den  = 1.0f + (0.25f * L);
    float qsqr = num / (den * den);

    /* Force the uniform q0sqr into VGPR domain (Jc*0.0f is not folded by
     * the compiler), so all scalar-by-scalar arithmetic emits VOP2/VOP3a
     * forms the GCN3 emulator implements rather than v_add_f32_e64. */
    float q0v = q0sqr + Jc * 0.0f;
    den = (qsqr - q0v) / (q0v + q0v * q0v);
    float ci = 1.0f / (1.0f + den);
    if (ci < 0.0f) ci = 0.0f;
    if (ci > 1.0f) ci = 1.0f;
    c[idx] = ci;
}

__kernel void srad2(
    __global float* __restrict__ J,
    __global const float* __restrict__ dN,
    __global const float* __restrict__ dS,
    __global const float* __restrict__ dW,
    __global const float* __restrict__ dE,
    __global const float* __restrict__ c,
    int rows, int cols, float lambda)
{
    int col = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    int row = get_group_id(1) * BLOCK_SIZE + get_local_id(1);

    if (row >= rows || col >= cols) return;

    int idx = row * cols + col;

    int iS = (row < rows - 1) ? (row + 1) : (rows - 1);
    int jE = (col < cols - 1) ? (col + 1) : (cols - 1);

    float cN = c[idx];
    float cS = c[iS * cols + col];
    float cW = c[idx];
    float cE = c[row * cols + jE];

    float D = cN * dN[idx] + cS * dS[idx] + cW * dW[idx] + cE * dE[idx];

    J[idx] += 0.25f * lambda * D;
}
