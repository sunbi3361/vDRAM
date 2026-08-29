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
    'virtual-caching-nofbt',
    'virtual-caching',
    'uvm',
    'uvm-oversub-150',
    'uvm-ideal',
    'utopia',  # sbin_claude_utopia
    # sbin_claude_utopia: RestSeg ratio sweep configs, matching
    # utopia_restseg_ratios in 3_gen_runners.py.
    # 'utopia-rs-6',
    # 'utopia-rs-25',
    # 'utopia-rs-50',
    'avatar',  # sbin_claude_avatar
    # sbin_claude_avatar: compress ratio sweep configs, matching
    # avatar_compress_ratios in 3_gen_runners.py.
    # 'avatar-cr-50',
    # 'avatar-cr-100',
    'hpt',  # sbin_claude_hpt
    # sbin_claude_hpt: access-count sweep configs, matching
    # hpt_accesses_per_walk in 3_gen_runners.py.
    # 'hpt-acc-2',
    # 'hpt-acc-5',
    'softwalker',  # sbin_claude_softwalker
    # sbin_claude_softwalker: sweep configs, matching
    # softwalker_in_tlb_mshr_max / softwalker_slots_per_cu in
    # 3_gen_runners.py.
    # 'softwalker-noitm',
    # 'softwalker-itm-128',
    # 'softwalker-itm-256',
    # 'softwalker-slots-8',
    # 'softwalker-slots-16',
    'latpc',
    ]

# sbin_claude: integrated benchmark set, kept in sync with
# 3_gen_runners.py.
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
    'graphbig_betweennesscentr',
    'graphbig_bfs',
    'graphbig_connectedcomp',
    'graphbig_degreecentr',
    'graphbig_gc',
    'graphbig_kcore',
    'graphbig_sssp',
    'graphbig_trianglecount',
    'gups',
    'kmeans',
    'matrixmultiplication',
    'matrixtranspose',
    'nbody',
    'npb_ep',
    'nw',
    'pagerank',
    'pannotia_color',
    'pannotia_mis',
    'pannotia_sssp',
    'parboil_cutcp',
    'parboil_sgemm',
    'polybench_2dconv',
    'polybench_3dconv',
    'polybench_3mm',
    'polybench_correlation',
    'polybench_doitgen',
    'polybench_fdtd2d',
    'polybench_gemm',
    'polybench_gemver',
    'polybench_gesummv',
    'polybench_jacobi1d',
    'polybench_jacobi2d',
    'polybench_lu',
    'polybench_mvt',
    'polybench_syr2k',
    'reduction',
    'relu',
    'rodinia_backprop',
    'rodinia_gaussian',
    'rodinia_hotspot',
    'rodinia_hotspot3d',
    'rodinia_lavamd',
    'rodinia_lud',
    'rodinia_pathfinder',
    'rodinia_srad',
    'simpleconvolution',
    'spmv',
    'stencil2d',
    'tango_blackscholes',
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
# sbin_claude_utopia: RestSeg-walk counters emitted per UTU by the runner's
# reportUtopia (report.go). Summed over GPUs like the L2 TLB miss counter.
UTOPIA_WHATS = (
    "utopia_rsw_hit_count", "utopia_rsw_miss_count",
    "utopia_sf_filtered_count",
    "utopia_sf_cache_hit_count", "utopia_sf_cache_miss_count",
    "utopia_tar_cache_hit_count", "utopia_tar_cache_miss_count",
    "utopia_flexseg_walk_count", "utopia_passthrough_count",
    "utopia_restseg_occupied_frames",
)
# sbin_claude_avatar: speculation/CAVA/EAF counters emitted per ASU by the
# runner's reportAvatar (report.go). Summed over GPUs like the UTU counters.
AVATAR_WHATS = (
    "avatar_l1_miss_forwarded_count", "avatar_speculation_count",
    "avatar_cava_pass_count", "avatar_cava_mismatch_count",
    "avatar_cava_incompressible_count", "avatar_cava_no_metadata_count",
    "avatar_early_completion_count", "avatar_real_response_first_count",
    "avatar_swallowed_rsp_count", "avatar_page_table_veto_count",
    # sbin_claude_avatar v3: avatar_validation_read_count,
    # avatar_validation_wait_cycle_sum and avatar_stale_validation_rsp_count
    # are gone - the ASU no longer issues a sector fetch of its own, because
    # CAST's speculative access is the requester's demand access.
    "avatar_spec_out_of_range_count", "avatar_walk_cancel_sent_count",
    "avatar_forward_suppressed_count", "avatar_orphan_rsp_count",
    "avatar_frame_install_count", "avatar_frame_invalidate_count",
    "avatar_region_bound_count", "avatar_region_free_count",
    # sbin_claude_avatar v4: the MOD is PC-indexed now, so misses that reach
    # the ASU without a PC cannot be speculated on. Should stay near zero.
    "avatar_spec_no_pc_count",
)
# sbin_claude_hpt: hashed-page-table walk counters emitted per GMMU by the
# runner's reportHPT (report.go). Present only in -gpu=hpt runs.
HPT_WHATS = (
    "hpt_walk_count", "hpt_memory_access_count",
)
# sbin_claude_softwalker: software-walk counters emitted per GMMU and In-TLB
# MSHR counters emitted per L2 TLB by the runner's reportSoftWalker
# (report.go). Present only in -gpu=softwalker runs.
SOFTWALKER_GMMU_WHATS = (
    "sw_walk_count", "sw_extra_cycles_total", "sw_admission_blocked_ticks",
)
SOFTWALKER_L2TLB_WHATS = (
    "in_tlb_mshr_alloc_count", "in_tlb_mshr_refuse_cap_count",
    "in_tlb_mshr_refuse_set_count",
)
LATPC_GPU_WHATS = (
    "l1vtlb_mshr_reservation_failure_count",
    # sbin_claude_latpc: the shared L2 TLB MSHR is the structure LATC's
    # L1-side relief runs into next, so its stalls have to be collected too.
    "l2tlb_mshr_reservation_failure_count",
    "latc_mshr_group_count", "latc_mshr_coalesced_miss_count",
    # sbin_claude_latpc: Regularity Detector output (Fig. 8's premise).
    "rd_inst_count", "rd_multi_vpn_inst_count",
    "rd_unique_vpn_count", "rd_prefetch_vpn_count",
)
LATPC_GMMU_WHATS = (
    "latp_batch_count", "latp_batched_member_count",
    # sbin_claude_latpc: prefetch requests that reached the walker with no
    # lead to join and had to take a walker slot of their own.
    "latp_lone_prefetch_walk_count",
    # sbin_claude_latpc: page walk queue / address tag diagnostics.
    "pw_queue_head_block_tick_count", "latp_lookahead_join_count",
    "latp_cross_group_join_count",
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
        # sbin_claude_utopia: sum RestSeg-walk counters over every UTU.
        elif what in UTOPIA_WHATS and loc.endswith(".UTU"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        # sbin_claude_avatar: sum speculation counters over every ASU.
        elif what in AVATAR_WHATS and loc.endswith(".ASU"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        # sbin_claude_hpt: sum hashed-walk counters over every GMMU.
        elif what in HPT_WHATS and loc.endswith(".GMMU"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        # sbin_claude_softwalker: sum software-walk counters over every GMMU
        # and In-TLB MSHR counters over every L2 TLB.
        elif what in SOFTWALKER_GMMU_WHATS and loc.endswith(".GMMU"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        elif what in SOFTWALKER_L2TLB_WHATS and loc.endswith(".L2TLB"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        elif what in LATPC_GPU_WHATS and re.fullmatch(r"GPU\[\d+\]", loc):
            out.setdefault(what, 0.0)
            out[what] += float(value)
        elif what in LATPC_GMMU_WHATS and loc.endswith(".GMMU"):
            out.setdefault(what, 0.0)
            out[what] += float(value)
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
        "l2_tlb_mpki", "l2_tlb_miss",
        # sbin_claude_utopia: RestSeg-walk summary columns.
        *UTOPIA_WHATS,
        # sbin_claude_avatar: speculation/CAVA/EAF summary columns.
        *AVATAR_WHATS,
        # sbin_claude_hpt: hashed-page-table walk summary columns.
        *HPT_WHATS,
        # sbin_claude_softwalker: software-walk and In-TLB MSHR columns.
        *SOFTWALKER_GMMU_WHATS,
        *SOFTWALKER_L2TLB_WHATS,
        *LATPC_GPU_WHATS,
        *LATPC_GMMU_WHATS,
        "error",
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
