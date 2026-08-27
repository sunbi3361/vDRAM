#!/home/sbin/tools/Python3.10.14/bin/python3
"""sbin_gmmu_omo: plot per-benchmark speedup vs baseline.

Reads the metrics CSV produced by 5_collect_metrics.py, computes each
config's speedup against the benchmark's baseline config
(speedup = t_baseline / t_config; 1.0 == baseline, > 1 faster, < 1 slower),
and writes a grouped bar chart plus the speedup values as a CSV.
(sbin_claude: metric switched from normalized execution time, t_config /
t_baseline, to speedup, t_baseline / t_config, so taller bars always mean
better performance.)

Filename quirk handled here: 5_collect_metrics.py parses
`exp_<config>_<benchmark>.sqlite3` with config matching `[a-z0-9_-]+`, so a
benchmark whose name carries a suite prefix (polybench_2dconv,
memory_bandwidth, tango_blackscholes, ...) comes out of the CSV split as
benchmark=`2dconv` + config=`baseline_polybench`. This script re-glues the
suite prefix back onto the benchmark name using the known config types.

Usage:
    python3 6_plot_normalized.py [--csv results/metrics.csv]
                                 [--out results/normalized_kernel_time.png]
                                 [--values results/normalized_kernel_time.csv]
"""

import argparse
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
# import pandas as pd
import pandas as pd  # noqa: PANDAS_OK  # sbin_gmmu_omo: retained dependency

# sbin_gmmu_omo: editable active selections, matching 3_gen_runners.py.
configs = [
    'baseline',
    # 'uvm',
    # 'uvm-ideal',
    # 'uvm-oversub-150',
    'virtual-caching',
    'utopia',  # sbin_claude_utopia
    'avatar',  # sbin_claude_avatar
    'hpt',  # sbin_claude_hpt
    'softwalker',  # sbin_claude_softwalker
    'latpc',
    'ideal-l1tlb',
    ]

benchmarks=[
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

# canonical order of non-baseline config types for bar grouping
CONFIG_ORDER = [
    "ideal-l1tlb",
    "ideal-l2tlb",
    "latpc",
    # "unified",
    # "unified-infinite-l1tlb",
    # "unified-infinite-l2tlb",
]

# COLORS = {
#     "ideal-l1tlb": "#4C72B0",
#     "ideal-l2tlb": "#55A868",
#     "unified": "#C44E52",
#     "unified-infinite-l1tlb": "#DD8452",
#     "unified-infinite-l2tlb": "#8172B2",
#     "baseline": "#7F7F7F",
#     "utopia": "#CCB974",  # sbin_claude_utopia
#     "avatar": "#64B5CD",  # sbin_claude_avatar
#     "hpt": "#DA8BC3",  # sbin_claude_hpt
# }
# sbin_claude: Okabe-Ito colorblind-safe palette. "virtual-caching" and
# "uvm-ideal" previously had no entry (fell back to matplotlib's default
# color-cycle pick), and "avatar" was too close to "ideal-l1tlb" (light blue
# vs blue) to tell apart at a glance. Every active config now gets a distinct
# hue; unused/legacy keys just keep placeholder colors.
COLORS = {
    "ideal-l1tlb": "#000000",        # sbin_claude: black
    "uvm-ideal": "#56B4E9",          # sbin_claude: sky blue
    "virtual-caching": "#E69F00",    # sbin_claude: orange
    "utopia": "#009E73",             # sbin_claude_utopia, sbin_claude: bluish green
    "avatar": "#CC79A7",             # sbin_claude_avatar, sbin_claude: reddish purple
    "hpt": "#D55E00",                # sbin_claude_hpt, sbin_claude: vermillion
    "softwalker": "#937860",         # sbin_claude_softwalker: brown (distinct from inactive #8C564B)
    "latpc": "#332288",
    "baseline": "#7F7F7F",
    "ideal-l2tlb": "#F0E442",        # sbin_claude: (inactive) yellow
    "unified": "#8C564B",            # sbin_claude: (inactive) brown
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
        SCRIPTS_DIR, "results", "normalized_kernel_time.png"))
    ap.add_argument("--values", default=os.path.join(
        SCRIPTS_DIR, "results", "normalized_kernel_time.csv"))
    ap.add_argument("--figsize", type=float, nargs=2, default=(18.0, 6.0))
    ap.add_argument("--dpi", type=int, default=150)
    args = ap.parse_args()

    # df = pd.read_csv(args.csv, dtype={"benchmark": str, "config": str})
    # df["benchmark"] = [reconstruct_benchmark(b, c)
    #                    for b, c in zip(df["benchmark"], df["config"])]
    # df["config"] = [config_type(c) for c in df["config"]]
    # df["kernel_time"] = pd.to_numeric(df["kernel_time"], errors="coerce")
    # sbin_gmmu_omo: canonicalize legacy rows before applying editable filters;
    # baseline remains available when selected configs need normalization.
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

    # sbin_claude: was normalized_time = r["kernel_time"] / base_t
    # (config/baseline, >1 == slower). Switched to speedup = base_t /
    # r["kernel_time"] (baseline/config, >1 == faster) so bars read directly
    # as "how much faster is this config than baseline".
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
                "speedup": base_t / r["kernel_time"],  # sbin_claude
            })

    if not norm_rows:
        print("No comparable (benchmark, config) pairs found; nothing to plot.")
        return

    norm = pd.DataFrame(norm_rows)
    norm = norm.sort_values(["benchmark", "config"])

    # benchmarks that actually have a non-baseline measurement
    # benches = sorted(norm["benchmark"].unique())
    # present_configs = [c for c in CONFIG_ORDER if c in set(norm["config"])]
    # present_configs += sorted(set(norm["config"]) - set(CONFIG_ORDER))
    # sbin_gmmu_omo: preserve the editable array order in the grouped plot.
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
    # ax.set_yscale("log")  # sbin_codex: use logarithmic y-axis
    ax.set_yscale("log")  # sbin_claude: re-enabled per request
    ax.set_xticks(x)
    ax.set_xticklabels(benches, rotation=45, ha="right", fontsize=9)
    # ax.set_ylabel("normalized execution time\n(config kernel_time / baseline)", fontsize=10)
    # sbin_claude: label/title now describe speedup, not normalized time.
    ax.set_ylabel("speedup (log scale)\n(baseline kernel_time / config kernel_time)", fontsize=10)  # sbin_claude
    # ax.set_title("Normalized kernel time per benchmark vs baseline "
    #              "(baseline = 1.0, log scale)", fontsize=12)
    # sbin_gmmu_omo: describe the linear y-axis used by this chart accurately.
    # ax.set_title("Normalized kernel time per benchmark vs baseline "
    #              "(baseline = 1.0, linear scale)", fontsize=12)
    # sbin_codex: describe the enabled logarithmic y-axis accurately.
    # ax.set_title("Normalized kernel time per benchmark vs baseline "
    #              "(baseline = 1.0, log scale)", fontsize=12)
    # ax.set_title("Speedup per benchmark vs baseline "
    #              "(baseline = 1.0, higher is better)", fontsize=12)  # sbin_claude
    ax.set_title("Speedup per benchmark vs baseline "
                 "(baseline = 1.0, log scale, higher is better)", fontsize=12)  # sbin_claude
    ax.grid(axis="y", which="both", ls=":", alpha=0.4)  # sbin_claude: minor gridlines for log scale
    # ax.legend(ncol=len(present_configs) + 1, fontsize=8,
    #           loc="upper center", bbox_to_anchor=(0.5, -0.15))
    # sbin_gmmu_omo: place the legend below rotated labels in figure space.
    # handles, labels = ax.get_legend_handles_labels()
    # fig.legend(handles, labels, ncol=len(present_configs) + 1, fontsize=8,
    #            loc="lower center", bbox_to_anchor=(0.5, 0.02))
    # fig.subplots_adjust(bottom=0.38)
    # sbin_claude: move the legend above the axes so each color is scanned
    # before the rotated x tick labels, instead of competing with them below.
    handles, labels = ax.get_legend_handles_labels()
    fig.legend(handles, labels, ncol=len(present_configs) + 1, fontsize=8,
               loc="upper center", bbox_to_anchor=(0.5, 0.98))
    fig.subplots_adjust(top=0.80, bottom=0.32)

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    fig.savefig(args.out, dpi=args.dpi, bbox_inches="tight")
    plt.close(fig)
    print(f"Wrote {args.out}")

    # ---------------- normalized values CSV ----------------
    os.makedirs(os.path.dirname(args.values) or ".", exist_ok=True)
    norm.to_csv(args.values, index=False)
    print(f"Wrote {args.values}")

    # ---------------- console summary ----------------
    # print(f"\n{'benchmark':30s}{'config':24s}{'norm':>8s}  "
    #       f"{'base_us':>10s} {'cfg_us':>10s}")
    # for _, r in norm.iterrows():
    #     print(f"{r['benchmark']:30s}{r['config']:24s}"
    #           f"{r['normalized_time']:8.3f}  "
    #           f"{r['baseline_us']:10.1f} {r['config_us']:10.1f}")
    # sbin_claude: column renamed normalized_time -> speedup.
    print(f"\n{'benchmark':30s}{'config':24s}{'speedup':>8s}  "
          f"{'base_us':>10s} {'cfg_us':>10s}")
    for _, r in norm.iterrows():
        print(f"{r['benchmark']:30s}{r['config']:24s}"
              f"{r['speedup']:8.3f}  "
              f"{r['baseline_us']:10.1f} {r['config_us']:10.1f}")


if __name__ == "__main__":
    main()
