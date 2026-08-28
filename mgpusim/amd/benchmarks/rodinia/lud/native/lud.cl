/*
 * OpenCL kernels for Rodinia LUD (gfx803 / GCN3)
 * Translated from rodinia_lud.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Blocked LU decomposition of a dense NxN matrix (no pivoting), BSIZE=16.
 * Three kernels:
 *   lud_diagonal   — in-place LU factor of the diagonal 16x16 block
 *   lud_perimeter  — forward/back-solve for row/column perimeter blocks
 *   lud_internal   — Schur-complement update for interior blocks
 */
#define BSIZE 16

__kernel void lud_diagonal(__global float* a, int n, int offset)
{
    __local float s[BSIZE][BSIZE];
    int tx = get_local_id(0), ty = get_local_id(1);

    s[ty][tx] = a[(offset * BSIZE + ty) * n + (offset * BSIZE + tx)];
    barrier(CLK_LOCAL_MEM_FENCE);

    for (int k = 0; k < BSIZE - 1; k++) {
        if (ty > k && tx == k)
            s[ty][k] /= s[k][k];
        barrier(CLK_LOCAL_MEM_FENCE);
        if (ty > k && tx > k)
            s[ty][tx] -= s[ty][k] * s[k][tx];
        barrier(CLK_LOCAL_MEM_FENCE);
    }

    a[(offset * BSIZE + ty) * n + (offset * BSIZE + tx)] = s[ty][tx];
}

__kernel void lud_perimeter(__global float* a, int n, int offset)
{
    __local float dia [BSIZE][BSIZE];
    __local float peri[BSIZE][BSIZE];

    int tx = get_local_id(0), ty = get_local_id(1);
    int halfn  = get_num_groups(0) / 2;
    int is_row = (get_group_id(0) < (unsigned)halfn);
    int idx    = is_row ? (int)get_group_id(0) : ((int)get_group_id(0) - halfn);
    int blk    = offset + idx + 1;

    dia[ty][tx] = a[(offset * BSIZE + ty) * n + (offset * BSIZE + tx)];

    if (is_row)
        peri[ty][tx] = a[(offset * BSIZE + ty) * n + (blk * BSIZE + tx)];
    else
        peri[ty][tx] = a[(blk   * BSIZE + ty) * n + (offset * BSIZE + tx)];
    barrier(CLK_LOCAL_MEM_FENCE);

    if (is_row) {
        for (int k = 0; k < BSIZE - 1; k++) {
            if (ty > k)
                peri[ty][tx] -= dia[ty][k] * peri[k][tx];
            barrier(CLK_LOCAL_MEM_FENCE);
        }
        a[(offset * BSIZE + ty) * n + (blk * BSIZE + tx)] = peri[ty][tx];
    } else {
        for (int k = 0; k < BSIZE; k++) {
            if (tx == k)
                peri[ty][k] /= dia[k][k];
            barrier(CLK_LOCAL_MEM_FENCE);
            if (tx > k)
                peri[ty][tx] -= peri[ty][k] * dia[k][tx];
            barrier(CLK_LOCAL_MEM_FENCE);
        }
        a[(blk * BSIZE + ty) * n + (offset * BSIZE + tx)] = peri[ty][tx];
    }
}

__kernel void lud_internal(__global float* a, int n, int offset)
{
    /* PERI_COL_STRIDE=17 (not 16) breaks the 8-byte alignment clang needs to
     * fuse the peri_row/peri_col stores into ds_write2st64_b32 (opcode 15),
     * which the GCN3 emulator does not implement. */
    #define PERI_COL_STRIDE 17
    __local float peri_row[BSIZE][BSIZE];
    __local float peri_col[BSIZE][PERI_COL_STRIDE];

    int tx = get_local_id(0), ty = get_local_id(1);
    int col_blk = offset + get_group_id(0) + 1;
    int row_blk = offset + get_group_id(1) + 1;

    peri_row[ty][tx] = a[(offset  * BSIZE + ty) * n + (col_blk * BSIZE + tx)];
    peri_col[ty][tx] = a[(row_blk * BSIZE + ty) * n + (offset  * BSIZE + tx)];
    barrier(CLK_LOCAL_MEM_FENCE);

    float sum = 0.0f;
    for (int k = 0; k < BSIZE; k++)
        sum += peri_col[ty][k] * peri_row[k][tx];
    a[(row_blk * BSIZE + ty) * n + (col_blk * BSIZE + tx)] -= sum;
}
