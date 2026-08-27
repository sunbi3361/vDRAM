#!/home/sbin/tools/Python3.10.14/bin/python3
"""sbin_claude_uvm: plot per-benchmark uvm-family speedup vs baseline.

Reads the metrics CSV produced by 5_collect_metrics.py, computes each uvm
variant's speedup against the benchmark's baseline config
(speedup = t_baseline / t_config; 1.0 == baseline, > 1 faster, < 1 slower),
and writes a grouped bar chart (log y-scale, since uvm configs can run up to
~150x slower than baseline, i.e. speedup down around 1/150) plus the speedup
values as a CSV. Companion to 6_plot_normalized.py, scoped to just the uvm
family (uvm, uvm-ideal, uvm-oversub-150) so those large swings don't compress
the near-1x configs plotted there onto a few flat pixels.

Usage:
    python3 7_plot_normalized_uvm.py [--csv results/metrics.csv]
                                     [--out results/normalized_kernel_time_uvm.png]
                                     [--values results/normalized_kernel_time_uvm.csv]
"""

import argparse
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

# sbin_claude_uvm: editable active selections, matching 3_gen_runners.py.
configs = [
    'baseline',
    'uvm',
    'uvm-ideal',
    'uvm-oversub-150',
]

benchmarks = [
    'atax',
    'bfs',
    'bicg',
    'fastwalshtransform',
    'fft',
    'fir',
    'floydwarshall',
    'kmeans',
    'matrixmultiplication',
    'matrixtranspose',
    'nbody',
    'nw',
    'pagerank',
    'relu',
    'simpleconvolution',
    'spmv',
    'stencil2d',
    'vectoradd',
]

SCRIPTS_DIR = os.path.dirname(os.path.realpath(__file__))
DEFAULT_CSV = os.path.join(SCRIPTS_DIR, "results", "metrics.csv")

# sbin_claude_uvm: uvm-family config names carry no suite prefix in the
# current metrics CSV, so no suite-prefix reconstruction is needed here
# (contrast with 6_plot_normalized.py's CONFIG_ORDER/reconstruct_benchmark).
CONFIG_ORDER = []

COLORS = {
    "baseline": "#7F7F7F",
    "uvm": "#E69F00",               # sbin_claude_uvm: orange
    "uvm-ideal": "#56B4E9",         # sbin_claude_uvm: sky blue
    "uvm-oversub-150": "#D55E00",   # sbin_claude_uvm: vermillion
}


def reconstruct_benchmark(benchmark, config):
    """Undo the suite-prefix split: config 'baseline_polybench' + benchmark
    '2dconv' -> 'polybench_2dconv'; plain configs pass through unchanged."""
    for prefix in CONFIG_ORDER + ["baseline"]:
        if config == prefix:
            return benchmark
        if config.startswith(prefix + "_"):
            return config[len(prefix) + 1:] + "_" + benchmark
    return benchmark


def config_type(config):
    """Strip the suite suffix from a config ('unified_shared_mem' -> 'unified')."""
    for prefix in CONFIG_ORDER + ["baseline"]:
        if config == prefix or config.startswith(prefix + "_"):
            return prefix
    return config


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--csv", default=DEFAULT_CSV)
    ap.add_argument("--out", default=os.path.join(
        SCRIPTS_DIR, "results", "normalized_kernel_time_uvm.png"))
    ap.add_argument("--values", default=os.path.join(
        SCRIPTS_DIR, "results", "normalized_kernel_time_uvm.csv"))
    ap.add_argument("--figsize", type=float, nargs=2, default=(18.0, 6.0))
    ap.add_argument("--dpi", type=int, default=150)
    args = ap.parse_args()

    df = pd.read_csv(args.csv, dtype={"benchmark": str, "config": str})
    df["benchmark"] = [reconstruct_benchmark(b, c)
                       for b, c in zip(df["benchmark"], df["config"])]
    df["config"] = [config_type(c) for c in df["config"]]
    df["kernel_time"] = pd.to_numeric(df["kernel_time"], errors="coerce")
    selected_configs = set(configs)
    normalization_configs = selected_configs | {"baseline"}
    df = df[df["benchmark"].isin(benchmarks) &
            df["config"].isin(normalization_configs)]

    # only benchmarks with a valid, positive baseline are comparable
    base = (df[df["config"] == "baseline"]
              .set_index("benchmark")["kernel_time"].dropna())
    base = base[base > 0]

    norm_rows = []
    for bench, base_t in base.items():
        for _, r in df[(df["benchmark"] == bench) &
                       (df["config"] != "baseline")].iterrows():
            if pd.isna(r["kernel_time"]) or r["kernel_time"] <= 0:
                continue
            norm_rows.append({
                "benchmark": bench,
                "config": r["config"],
                "baseline_us": base_t * 1e6,
                "config_us": r["kernel_time"] * 1e6,
                "speedup": base_t / r["kernel_time"],
            })

    if not norm_rows:
        print("No comparable (benchmark, config) pairs found; nothing to plot.")
        return

    norm = pd.DataFrame(norm_rows)
    norm = norm.sort_values(["benchmark", "config"])

    # sbin_claude_uvm: preserve the editable array order in the grouped plot.
    benches = [b for b in benchmarks if b in set(norm["benchmark"])]
    present_configs = [c for c in configs
                       if c != "baseline" and c in set(norm["config"])]

    # ---------------- plot ----------------
    n_bench, n_conf = len(benches), len(present_configs)
    x = np.arange(n_bench)
    width = 0.8 / n_conf
    offsets = (np.arange(n_conf) - (n_conf - 1) / 2.0) * width

    fig, ax = plt.subplots(figsize=args.figsize)
    for off, cfg in zip(offsets, present_configs):
        sub = norm[norm["config"] == cfg].set_index("benchmark")["speedup"]
        vals = [sub.get(b, np.nan) for b in benches]
        ax.bar(x + off, vals, width, label=cfg, color=COLORS.get(cfg),
               edgecolor="white", linewidth=0.5)

    ax.axhline(1.0, color="black", lw=1.2, ls="--", label="baseline")
    ax.set_yscale("log")  # sbin_claude_uvm: uvm configs span orders of magnitude
    ax.set_xticks(x)
    ax.set_xticklabels(benches, rotation=45, ha="right", fontsize=9)
    ax.set_ylabel("speedup (log scale)\n(baseline kernel_time / config kernel_time)",
                  fontsize=10)
    ax.set_title("uvm-family speedup per benchmark vs baseline "
                 "(baseline = 1.0, log scale, higher is better)", fontsize=12)
    ax.grid(axis="y", which="both", ls=":", alpha=0.4)

    # sbin_claude_uvm: legend above the axes, out of the way of rotated
    # x tick labels, matching 6_plot_normalized.py.
    handles, labels = ax.get_legend_handles_labels()
    fig.legend(handles, labels, ncol=len(present_configs) + 1, fontsize=8,
               loc="upper center", bbox_to_anchor=(0.5, 0.98))
    fig.subplots_adjust(top=0.80, bottom=0.32)

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    fig.savefig(args.out, dpi=args.dpi, bbox_inches="tight")
    plt.close(fig)
    print(f"Wrote {args.out}")

    # ---------------- speedup values CSV ----------------
    os.makedirs(os.path.dirname(args.values) or ".", exist_ok=True)
    norm.to_csv(args.values, index=False)
    print(f"Wrote {args.values}")

    # ---------------- console summary ----------------
    print(f"\n{'benchmark':30s}{'config':24s}{'speedup':>8s}  "
          f"{'base_us':>10s} {'cfg_us':>10s}")
    for _, r in norm.iterrows():
        print(f"{r['benchmark']:30s}{r['config']:24s}"
              f"{r['speedup']:8.3f}  "
              f"{r['baseline_us']:10.1f} {r['config_us']:10.1f}")


if __name__ == "__main__":
    main()
