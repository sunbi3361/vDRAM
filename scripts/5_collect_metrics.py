#!/home/sbin/tools/Python3.10.14/bin/python3
"""sbin_gmmu_omo: collect the sqlite metrics produced by 4_run_benchmarks.sh.

Scans benchmarks/samples/<benchmark>/exp_<config>_<benchmark>.sqlite3 and the
corresponding log files, and writes the consolidated metrics CSV consumed by
analyze_metrics.py / compare_configs.py.

Usage:
    python3 5_collect_metrics.py [--output results/metrics.csv]
"""

import argparse
import csv
import glob
import os
# import re
import sqlite3
from contextlib import closing  # sbin_gmmu_omo

# sbin_gmmu_omo: editable active selections, matching 3_gen_runners.py.
configs = [
    'baseline',
    'ideal-l1tlb',
    'virtual-caching',
    # 'unified',
    # 'unified-infinite-l1tlb',
    ]

benchmarks=[
    'altis_cfd',
    'atax',
    'babelstream',
    'bfs',
    'bicg',
    'cache_latency',
    'fastwalshtransform',
    'fft',
    'fir',
    'floydwarshall',
    'kmeans',
    'matrixtranspose',
    'nbody',
    'npb_ep',
    'parboil_cutcp',
    'parboil_sgemm',
    'polybench_2dconv',
    'polybench_3dconv',
    'polybench_3mm',
    'polybench_correlation',
    'polybench_fdtd2d',
    'polybench_gemm',
    'polybench_jacobi2d',
    'polybench_mvt',
    'polybench_syr2k',
    'relu',
    'rodinia_backprop',
    'rodinia_gaussian',
    'rodinia_hotspot',
    'rodinia_hotspot3d',
    'rodinia_lavamd',
    'rodinia_lud',
    'rodinia_pathfinder',
    'rodinia_srad',
    'spmv',
    'stencil2d',
    'tango_blackscholes',
    'vectoradd',
    'polybench_doitgen',
    'polybench_gemver',
    'polybench_gesummv',
    'polybench_jacobi1d',
    'polybench_lu',
    'pannotia_color',
    'pannotia_mis',
    'pannotia_sssp',
    'gups',
    'reduction',
    'graphbig_betweennesscentr',
    'graphbig_bfs',
    'graphbig_connectedcomp',
    'graphbig_degreecentr',
    'graphbig_gc',
    'graphbig_kcore',
    'graphbig_sssp',
    'graphbig_trianglecount',
]

SCRIPTS_DIR = os.path.dirname(os.path.realpath(__file__))
SAMPLES_DIR = os.path.join(SCRIPTS_DIR, "benchmarks", "samples")
LOGS_DIR = os.path.join(SCRIPTS_DIR, "benchmarks", "logs")

METRIC_WHATS = [
    "working_set_pages", "working_set_bytes", "total_memory_footprint",
    "total_allocated_pages", "l2_tlb_mpki",
]

# FILENAME_RE = re.compile(r"^exp_([a-z0-9_-]+)_([a-z0-9_]+)\.sqlite3$")


# sbin_gmmu_omo: derive canonical names from the benchmark directory and exact
# filename boundaries so benchmark underscores cannot be consumed as config.
def parse_run_identity(sqlite_path):
    benchmark = os.path.basename(os.path.dirname(sqlite_path))
    filename = os.path.basename(sqlite_path)
    prefix = "exp_"
    suffix = f"_{benchmark}.sqlite3"
    if not filename.startswith(prefix) or not filename.endswith(suffix):
        return None
    config = filename[len(prefix):-len(suffix)]
    if not config:
        return None
    return benchmark, config


def extract_metrics(sqlite_path):
    # conn = sqlite3.connect(f"file:{sqlite_path}?mode=ro", uri=True)
    # cur = conn.cursor()
    # out = {}
    # try:
    #     cur.execute("SELECT Location, What, Value FROM mgpusim_metrics")
    #     rows = cur.fetchall()
    # except sqlite3.Error as e:
    #     conn.close()
    #     return {"error": f"sqlite-{e}"}
    # conn.close()
    # sbin_gmmu_omo: close read-only SQLite handles through a context manager.
    out = {}
    with closing(sqlite3.connect(
            f"file:{sqlite_path}?mode=ro", uri=True)) as conn:
        try:
            rows = conn.execute(
                "SELECT Location, What, Value FROM mgpusim_metrics").fetchall()
        except sqlite3.Error as e:
            return {"error": f"sqlite-{e}"}

    for loc, what, value in rows:
        if what == "kernel_time" and loc == "Driver":
            out["kernel_time"] = value
        if what in METRIC_WHATS:
            out[what] = value
        if what in ("gmmu_total_req_count", "gmmu_page_fault_fill_count",
                    "gmmu_migration_fault_count", "page_migration_count"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        if what == "miss" and loc.endswith(".L2TLB"):
            out.setdefault("l2_tlb_miss", 0.0)
            out["l2_tlb_miss"] += float(value)
    return out


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default=os.path.join(SCRIPTS_DIR,
                                                         "results",
                                                         "metrics.csv"))
    parser.add_argument("--samples-dir", default=SAMPLES_DIR)
    parser.add_argument("--logs-dir", default=LOGS_DIR)
    args = parser.parse_args()

    fieldnames = [
        "benchmark", "config", "passed", "kernel_time", "working_set_pages",
        "working_set_bytes", "total_memory_footprint", "total_allocated_pages",
        "l2_tlb_mpki", "l2_tlb_miss", "gmmu_total_req_count",
        "gmmu_page_fault_fill_count", "gmmu_migration_fault_count",
        "page_migration_count", "error",
    ]

    os.makedirs(os.path.dirname(args.output), exist_ok=True)

    # sbin_gmmu_omo: selection sets make only listed benchmark/config pairs
    # eligible for collection.
    selected_configs = set(configs)
    selected_benchmarks = set(benchmarks)
    results = []
    for sqlite_path in sorted(glob.glob(os.path.join(
            args.samples_dir, "*", "exp_*.sqlite3"))):
        # m = FILENAME_RE.match(os.path.basename(sqlite_path))
        # if not m:
        #     continue
        # config, benchmark = m.group(1), m.group(2)
        # sbin_gmmu_omo: parse canonical identity before applying selections.
        identity = parse_run_identity(sqlite_path)
        if identity is None:
            continue
        benchmark, config = identity
        if benchmark not in selected_benchmarks or config not in selected_configs:
            continue

        row = {"benchmark": benchmark, "config": config}
        metrics = extract_metrics(sqlite_path)
        row.update(metrics)

        # sbin_gmmu_omo: determine pass/fail from the run log.
        log_path = os.path.join(args.logs_dir,
                                f"{benchmark}_{config}.out")
        if os.path.isfile(log_path):
            with open(log_path, encoding="utf-8", errors="replace") as fh:
                row["passed"] = "Pass" in fh.read()
        results.append(row)

    results.sort(key=lambda r: (r["benchmark"], r["config"]))
    with open(args.output, "w", newline="", encoding="utf-8") as fh:
        w = csv.DictWriter(fh, fieldnames=fieldnames)
        w.writeheader()
        for r in results:
            w.writerow(r)

    print(f"Collected {len(results)} runs -> {args.output}")


if __name__ == "__main__":
    main()
