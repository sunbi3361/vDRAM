#!/home/sbin/tools/Python3.10.14/bin/python3
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "matplotlib",
#     "numpy",
# ]
# ///

# --- How to run ---
# Run: uv run scripts/7_plot_l2_tlb_mpki.py [--csv PATH] [--out PATH]
# Or:  python3 scripts/7_plot_l2_tlb_mpki.py [--csv PATH] [--out PATH]
# ------------------

"""sbin_gmmu_omo: plot selected L2 TLB MPKI values from metrics.csv."""

from __future__ import annotations

import argparse
import csv
import math
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np

# sbin_gmmu_omo: editable active selections, matching 3_gen_runners.py.
configs = [
    'baseline',
    'unified',
    'ideal-l1tlb',
    'uvm',
    'uvm-ideal',
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

CONFIG_ORDER = [
    "ideal-l1tlb",
    "ideal-l2tlb",
    "unified",
    "unified-infinite-l1tlb",
    "unified-infinite-l2tlb",
]

COLORS = {
    "ideal-l1tlb": "#4C72B0",
    "ideal-l2tlb": "#55A868",
    "unified": "#C44E52",
    "unified-infinite-l1tlb": "#DD8452",
    "unified-infinite-l2tlb": "#8172B2",
    "baseline": "#7F7F7F",
}


def reconstruct_benchmark(benchmark: str, config: str) -> str:
    """Canonicalize a benchmark from a legacy greedily split CSV row."""
    for prefix in CONFIG_ORDER + ["baseline"]:
        if config == prefix:
            return benchmark
        if config.startswith(prefix + "_"):
            return config[len(prefix) + 1:] + "_" + benchmark
    return benchmark


def config_type(config: str) -> str:
    """Canonicalize a config from a legacy greedily split CSV row."""
    for prefix in CONFIG_ORDER + ["baseline"]:
        if config == prefix or config.startswith(prefix + "_"):
            return prefix
    return config


def load_values(csv_path: str) -> list[tuple[str, str, float]]:
    """Read, canonicalize, and select numeric L2 TLB MPKI rows."""
    selected_configs = set(configs)
    selected_benchmarks = set(benchmarks)
    rows: list[tuple[str, str, float]] = []
    with open(csv_path, newline="", encoding="utf-8") as csv_file:
        for row in csv.DictReader(csv_file):
            benchmark = reconstruct_benchmark(row["benchmark"], row["config"])
            config = config_type(row["config"])
            try:
                mpki = float(row["l2_tlb_mpki"])
            except ValueError:
                continue
            if math.isnan(mpki):
                continue
            if benchmark in selected_benchmarks and config in selected_configs:
                rows.append((benchmark, config, mpki))

    benchmark_order = {name: index for index, name in enumerate(benchmarks)}
    config_order = {name: index for index, name in enumerate(configs)}
    return sorted(rows, key=lambda row: (
        benchmark_order[row[0]], config_order[row[1]]))


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--csv", default=DEFAULT_CSV)
    parser.add_argument("--out", default=os.path.join(
        SCRIPTS_DIR, "results", "l2_tlb_mpki.png"))
    parser.add_argument("--values", default=os.path.join(
        SCRIPTS_DIR, "results", "l2_tlb_mpki.csv"))
    parser.add_argument("--figsize", type=float, nargs=2, default=(18.0, 6.0))
    parser.add_argument("--dpi", type=int, default=150)
    args = parser.parse_args()

    rows = load_values(args.csv)
    if not rows:
        print("No selected numeric L2 TLB MPKI values found; nothing to plot.")
        return

    present_benchmarks = [name for name in benchmarks
                          if any(row[0] == name for row in rows)]
    present_configs = [name for name in configs
                       if any(row[1] == name for row in rows)]
    values = {(benchmark, config): mpki
              for benchmark, config, mpki in rows}

    x = np.arange(len(present_benchmarks))
    width = 0.8 / len(present_configs)
    offsets = (np.arange(len(present_configs)) -
               (len(present_configs) - 1) / 2.0) * width

    fig, axis = plt.subplots(figsize=args.figsize)
    for offset, config in zip(offsets, present_configs):
        heights = [values.get((benchmark, config), np.nan)
                   for benchmark in present_benchmarks]
        axis.bar(x + offset, heights, width, label=config,
                 color=COLORS.get(config), edgecolor="white", linewidth=0.5)
        # sbin_gmmu_omo: mark numeric zero rows without marking missing pairs.
        zero_positions = [bar_x for bar_x, benchmark in zip(
            x + offset, present_benchmarks)
            if values.get((benchmark, config)) == 0.0]
        axis.plot(zero_positions, [0.0] * len(zero_positions),
                  linestyle="None", marker="_", markersize=8,
                  markeredgewidth=2, color=COLORS.get(config),
                  label="_nolegend_")

    axis.set_xticks(x)
    axis.set_xticklabels(present_benchmarks, rotation=45, ha="right", fontsize=9)
    # sbin_gmmu_omo: symlog keeps exact zero bars at zero while compressing
    # the several-orders-of-magnitude positive MPKI range.
    axis.set_yscale("symlog", linthresh=1.0, linscale=1.0)
    # axis.set_ylabel("L2 TLB MPKI", fontsize=10)
    # axis.set_title("L2 TLB MPKI per benchmark and config", fontsize=12)
    # sbin_gmmu_omo: make the nonlinear scale and its linear threshold explicit.
    axis.set_ylabel("L2 TLB MPKI (symlog; linear below 1 MPKI)", fontsize=10)
    axis.set_title("L2 TLB MPKI per benchmark and config "
                   "(symlog scale; linear below 1 MPKI)", fontsize=12)
    axis.grid(axis="y", ls=":", alpha=0.4)
    # axis.legend(ncol=len(present_configs), fontsize=8,
    #             loc="upper center", bbox_to_anchor=(0.5, -0.15))
    # sbin_gmmu_omo: place the legend below rotated labels in figure space.
    handles, labels = axis.get_legend_handles_labels()
    fig.legend(handles, labels, ncol=len(present_configs), fontsize=8,
               loc="lower center", bbox_to_anchor=(0.5, 0.02))
    fig.subplots_adjust(bottom=0.38)

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    fig.savefig(args.out, dpi=args.dpi, bbox_inches="tight")
    plt.close(fig)
    print(f"Wrote {args.out}")

    os.makedirs(os.path.dirname(args.values) or ".", exist_ok=True)
    with open(args.values, "w", newline="", encoding="utf-8") as values_file:
        writer = csv.writer(values_file)
        writer.writerow(["benchmark", "config", "l2_tlb_mpki"])
        writer.writerows(rows)
    print(f"Wrote {args.values}")


if __name__ == "__main__":
    main()
