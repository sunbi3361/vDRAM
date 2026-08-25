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
import re
import sqlite3
from contextlib import closing  # sbin_gmmu_omo

# sbin_gmmu_omo: editable active selections, matching 3_gen_runners.py.
configs = [
    'baseline',
    'ideal-l1tlb',
    'virtual-caching',
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
SAMPLES_DIR = os.path.join(SCRIPTS_DIR, "benchmarks", "samples")
LOGS_DIR = os.path.join(SCRIPTS_DIR, "benchmarks", "logs")

# METRIC_WHATS = [
#     "working_set_pages", "working_set_bytes", "memory_footprint_total_pages",
#     "memory_footprint_total_bytes", "l2_tlb_mpki",
# ]
# sbin_codex: canonical summary metrics selected by Location, matching the rows
# emitted by the runner's extended reporter (extendedreport.go).
GMMU_WHATS = (
    "gmmu_translation_count", "gmmu_translation_avg_latency",
    "gmmu_max_inflight", "gmmu_avg_inflight",
)
MMU_WHATS = (
    "mmu_translation_count", "mmu_translation_avg_latency",
    "mmu_max_inflight", "mmu_avg_inflight",
)
MEMORY_WHATS = (
    "memory_page_size",
    "memory_footprint_live_pages", "memory_footprint_live_bytes",
    "memory_footprint_peak_pages", "memory_footprint_peak_bytes",
    "memory_footprint_total_pages", "memory_footprint_total_bytes",
)
MIGRATION_WHATS = (
    "page_migration_count", "page_migration_pages",
    "page_migration_bytes", "page_migration_avg_latency",
)

TIMESTAMP_RE = re.compile(r"^\d{6}_\d{4}$")


# sbin_gmmu_omo: derive canonical names from the benchmark directory and exact
# filename boundaries so benchmark underscores cannot be consumed as config.
def parse_run_identity(sqlite_path):
    benchmark = os.path.basename(os.path.dirname(sqlite_path))
    filename = os.path.basename(sqlite_path)
    prefix = "exp_"
    if not filename.startswith(prefix) or not filename.endswith(".sqlite3"):
        return None
    stem = filename[len(prefix):-len(".sqlite3")]
    suffix = f"_{benchmark}_"
    if suffix not in stem:
        return None
    config, timestamp = stem.rsplit(suffix, 1)
    if not config or not TIMESTAMP_RE.fullmatch(timestamp):
        return None
    return benchmark, config, timestamp


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

    # for loc, what, value in rows:
    #     if what == "kernel_time" and loc == "Driver":
    #         out["kernel_time"] = value
    #     if what in METRIC_WHATS:
    #         out[what] = value
    #     if what in ("gmmu_total_req_count", "gmmu_page_fault_fill_count",
    #                 "gmmu_migration_fault_count", "page_migration_count"):
    #         out.setdefault(what, 0.0)
    #         out[what] += float(value)
    #     if what == "miss" and loc.endswith(".L2TLB"):
    #         out.setdefault("l2_tlb_miss", 0.0)
    #         out["l2_tlb_miss"] += float(value)
    # sbin_codex: select canonical summary rows by Location so per-GPU
    # duplicates (e.g. GPU[1] working_set rows) cannot override the summary
    # rows, and stale gmmu_total_req_count/page_fault_fill/migration_fault
    # counters are dropped.
    for loc, what, value in rows:
        if what == "kernel_time" and loc == "Driver":
            out["kernel_time"] = value
        elif what == "working_set_pages" and loc == "WorkingSet":
            out["working_set_pages"] = value
        elif what == "working_set_bytes" and loc == "WorkingSet":
            out["working_set_bytes"] = value
        elif what in GMMU_WHATS and loc.endswith(".GMMU"):
            out[what] = value
        elif what in MMU_WHATS and loc == "MMU":
            out[what] = value
        elif what in MEMORY_WHATS and loc == "Driver":
            out[what] = value
        elif what in MIGRATION_WHATS and loc == "Driver":
            out[what] = value
        elif what == "pcie_page_migration_payload_bytes" and loc == "PCIe":
            out[what] = value
        elif what == "l2_tlb_mpki" and loc.endswith(".L2TLB"):
            out[what] = value
        elif what == "miss" and loc.endswith(".L2TLB"):
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

    # fieldnames = [
    #     "benchmark", "config", "passed", "kernel_time", "working_set_pages",
    #     "working_set_bytes", "memory_footprint_total_pages", "memory_footprint_total_bytes",
    #     "l2_tlb_mpki", "l2_tlb_miss", "gmmu_total_req_count",
    #     "gmmu_page_fault_fill_count", "gmmu_migration_fault_count",
    #     "page_migration_count", "error",
    # ]
    # sbin_codex: CSV columns match the current stable summary metrics.
    fieldnames = [
        "benchmark", "config", "passed", "kernel_time", "working_set_pages",
        "working_set_bytes",
        "gmmu_translation_count", "gmmu_translation_avg_latency",
        "gmmu_max_inflight", "gmmu_avg_inflight",
        "mmu_translation_count", "mmu_translation_avg_latency",
        "mmu_max_inflight", "mmu_avg_inflight",
        "memory_page_size",
        "memory_footprint_live_pages", "memory_footprint_live_bytes",
        "memory_footprint_peak_pages", "memory_footprint_peak_bytes",
        "memory_footprint_total_pages", "memory_footprint_total_bytes",
        "page_migration_count", "page_migration_pages",
        "page_migration_bytes", "page_migration_avg_latency",
        "pcie_page_migration_payload_bytes",
        "l2_tlb_mpki", "l2_tlb_miss", "error",
    ]

    os.makedirs(os.path.dirname(args.output), exist_ok=True)

    # sbin_gmmu_omo: selection sets make only listed benchmark/config pairs
    # eligible for collection.
    selected_configs = set(configs)
    selected_benchmarks = set(benchmarks)
    latest_paths = {}
    for sqlite_path in glob.glob(os.path.join(
            args.samples_dir, "*", "exp_*.sqlite3")):
        # m = FILENAME_RE.match(os.path.basename(sqlite_path))
        # if not m:
        #     continue
        # config, benchmark = m.group(1), m.group(2)
        # sbin_gmmu_omo: parse canonical identity before applying selections.
        identity = parse_run_identity(sqlite_path)
        if identity is None:
            continue
        benchmark, config, timestamp = identity
        if benchmark not in selected_benchmarks or config not in selected_configs:
            continue
        key = (benchmark, config)
        previous = latest_paths.get(key)
        if previous is None or timestamp > previous[0]:
            latest_paths[key] = (timestamp, sqlite_path)

    results = []
    for sqlite_path in sorted(path for _, path in latest_paths.values()):
        benchmark, config, _ = parse_run_identity(sqlite_path)

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
