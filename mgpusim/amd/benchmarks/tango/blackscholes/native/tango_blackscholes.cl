/*
 * OpenCL kernel for Tango Black-Scholes (gfx803 / GCN3)
 * Translated from tango_blackscholes.cpp for the clang-ocl -mcpu=gfx803 flow.
 *
 * Each work-item prices one European option (call + put) with the
 * Black-Scholes closed-form formula, using a polynomial approximation
 * of the cumulative normal distribution (Abramowitz & Stegun 26.2.17).
 *
 * Uses a constant BLOCK_SIZE for the block geometry so the compiler emits
 * no hidden ABI arguments. The gfx803 emulator lacks the inlined full-precision
 * log/exp/sqrt expansions (V_ALIGNBIT_B32), so the native_* forms are used:
 * clang lowers native_log(x) to log2(x)*ln2 and native_exp(x) to exp2(x*log2e),
 * both of which the emulator implements at double precision.
 */
#define BLOCK_SIZE 256

static inline float cnd(float d) {
    const float A1 = 0.31938153f;
    const float A2 = -0.356563782f;
    const float A3 = 1.781477937f;
    const float A4 = -1.821255978f;
    const float A5 = 1.330274429f;
    const float RSQRT2PI = 0.39894228040143267793994605993438f;

    float K = 1.0f / (1.0f + 0.2316419f * sqrt(d * d));
    float cnd_val = RSQRT2PI * exp2(-0.5f * d * d * 1.4426950408889634f) *
                    (K * (A1 + K * (A2 + K * (A3 + K * (A4 + K * A5)))));

    if (d > 0.0f) cnd_val = 1.0f - cnd_val;
    return cnd_val;
}

__kernel void blackscholes_kernel(
    __global const float* __restrict__ S,       // stock price
    __global const float* __restrict__ K,       // strike price
    __global const float* __restrict__ T,       // time to expiration
    __global const float* __restrict__ sigma,   // volatility
    float        r,                             // risk-free rate
    __global float* callPrice,
    __global float* putPrice,
    int          N)
{
    int idx = get_group_id(0) * BLOCK_SIZE + get_local_id(0);
    if (idx >= N) return;

    float s  = S[idx];
    float k  = K[idx];
    float t  = T[idx];
    float v  = sigma[idx];

    float sqrtT  = sqrt(t);
    float d1     = ((log2(s / k) * 0.6931471805599453f) + (r + 0.5f * v * v) * t) / (v * sqrtT);
    float d2     = d1 - v * sqrtT;

    float expRT  = exp2(-r * t * 1.4426950408889634f);
    float cnd_d1 = cnd(d1);
    float cnd_d2 = cnd(d2);

    callPrice[idx] = s * cnd_d1 - k * expRT * cnd_d2;
    putPrice[idx]  = k * expRT * (1.0f - cnd_d2) - s * (1.0f - cnd_d1);
}
