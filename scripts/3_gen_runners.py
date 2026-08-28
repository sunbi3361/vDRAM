#!/home/sbin/tools/Python3.10.14/bin/python3

import csv
import os

configs = [
    'baseline',
    'ideal-l1tlb',
    'virtual-caching',
    'virtual-caching-nofbt',
    'uvm',
    'uvm-ideal',
    # sbin_codex: 150% oversubscription. The UVM GPU capacity is derived from
    # each benchmark's own AllocateManaged footprint, so every benchmark is
    # oversubscribed by the same ratio regardless of how much it allocates.
    'uvm-oversub-150',
    # sbin_claude_utopia: hybrid RestSeg/FlexSeg translation (-gpu=utopia).
    # 'utopia' uses the runner defaults (RestSeg ratio 0.125, 16 ways).
    # RestSeg-ratio sweep entries go into utopia_restseg_ratios below.
    'utopia',
    # sbin_claude_avatar: speculative translation with rapid validation
    # (-gpu=avatar). 'avatar' uses the runner defaults (compress ratio 0.8,
    # validation latency 200, 2MB-region fragmentation on). Compress-ratio
    # sweep entries go into avatar_compress_ratios below.
    'avatar',
    # sbin_claude_hpt: FS-HPT hashed page table (-gpu=hpt). 'hpt' uses the
    # runner default (1 memory access per walk = ideal HPT). Access-count
    # sweep entries go into hpt_accesses_per_walk below.
    'hpt',
    # sbin_claude_softwalker: SoftWalker software page-table walk
    # (-gpu=softwalker). 'softwalker' uses the runner defaults (32 slots/CU,
    # comm 10, setup 20, 8 cycles/level, In-TLB MSHR max 512). Ablation and
    # sweep entries go into softwalker_in_tlb_mshr_max below.
    'softwalker',
    'latpc',
    ]

# sbin_claude_utopia: RestSeg ratio per utopia sweep config, mirroring the
# oversubscription_ratios pattern:
#
#   ratio = RestSeg bytes / GPU memory (FlexSeg keeps the remainder)
#
# Each key is generated as its own config directory/runner. Uncomment (or
# add) entries to sweep the ratio; the plain 'utopia' config keeps the
# default 0.125.
utopia_restseg_ratios = {
    # 'utopia-rs-6':  0.0625,
    # 'utopia-rs-25': 0.25,
    # 'utopia-rs-50': 0.5,
    }
configs += list(utopia_restseg_ratios)  # sbin_claude_utopia

# sbin_claude_avatar: compress ratio per avatar sweep config (fraction of
# frames whose sectors embed page information for CAVA rapid validation).
# Each key is generated as its own config directory/runner. Uncomment (or
# add) entries to sweep the ratio; the plain 'avatar' config keeps the
# default 0.8.
avatar_compress_ratios = {
    # 'avatar-cr-50':  0.5,
    # 'avatar-cr-100': 1.0,
    }
configs += list(avatar_compress_ratios)  # sbin_claude_avatar

# sbin_claude_hpt: memory references per hashed-page-table walk. 1 is ideal
# HPT (no hash collision); larger values model collision chains, and 5 is the
# sanity check that reproduces a full radix walk without the page-walk cache.
# Each key is generated as its own config directory/runner. Uncomment (or add)
# entries to sweep; the plain 'hpt' config keeps the runner default.
hpt_accesses_per_walk = {
    # 'hpt-acc-2': 2,
    # 'hpt-acc-5': 5,
}
configs += list(hpt_accesses_per_walk)  # sbin_claude_hpt

# sbin_claude_softwalker: In-TLB MSHR capacity per softwalker sweep config.
# 0 is the paper's "SW w/o In-TLB MSHR" ablation (Figure 16); intermediate
# values reproduce the Figure 24 capacity sweep. The plain 'softwalker'
# config keeps the runner default (512 = every L2 TLB way).
softwalker_in_tlb_mshr_max = {
    # 'softwalker-noitm':   0,
    # 'softwalker-itm-128': 128,
    # 'softwalker-itm-256': 256,
}
configs += list(softwalker_in_tlb_mshr_max)  # sbin_claude_softwalker

# sbin_claude_softwalker: SoftPWB slots per CU (PW Warp threads). The plain
# config keeps the runner default of 32.
softwalker_slots_per_cu = {
    # 'softwalker-slots-8':  8,
    # 'softwalker-slots-16': 16,
}
configs += list(softwalker_slots_per_cu)  # sbin_claude_softwalker

# sbin_claude_latpc: LATP tag ablation. The plain 'latpc' config now runs the
# paper's PW Buffer tag (Base Address + Stride*Index, Fig. 15) on top of the
# 128-entry page walk queue that is baseline GMMU hardware (Table 2).
# 'latpc-gidtag' is the narrower Regularity-Detector-group-ID tag, which
# cannot merge walks issued by different warp instructions.
#
# Each key becomes its own config directory/runner. Uncomment to run it.
latpc_variants = {
    # 'latpc-gidtag': {'addr_tag': False},
    }
configs += list(latpc_variants)  # sbin_claude_latpc

# sbin_codex: oversubscription ratio per config (uvm-manager.md 20).
#
#   ratio = working set / UVM GPU capacity
#
# The capacity is a UVM-internal budget; the modeled GPU memory stays at its
# configured size.
oversubscription_ratios = {
    'uvm-oversub-150': 1.5,
    }

# sbin_codex: the working set is read back from a completed baseline run.
#
# It has to be measured rather than derived from the allocation size, because
# the two can differ by more than an order of magnitude. nbody allocates 32 MB
# but only ever touches 2.6 MB, so sizing the capacity from its allocation
# would leave it entirely resident and not oversubscribed at all.
#
# Benchmarks without a measurement fall back to -uvm-oversubscription-ratio,
# which the driver resolves from the allocation footprint instead.
METRICS_CSV = os.path.dirname(os.path.realpath(__file__)) + "/results/metrics.csv"
UVM_REGION_SIZE = 64 * 1024


def load_working_sets():
    """benchmark -> working set in bytes, from the collected metrics."""
    if not os.path.exists(METRICS_CSV):
        return {}

    working_sets = {}
    with open(METRICS_CSV, newline="") as handle:
        for row in csv.DictReader(handle):
            raw = (row.get("working_set_bytes") or "").strip()
            if not raw:
                continue
            try:
                measured = int(float(raw))
            except ValueError:
                continue
            if measured <= 0:
                continue
            # Every config observes the same pages, so prefer baseline and
            # accept any other config as a stand-in.
            benchmark = row["benchmark"]
            if row["config"] == "baseline" or benchmark not in working_sets:
                working_sets[benchmark] = measured

    return working_sets


def oversub_capacity(working_set_bytes, ratio):
    """UVM capacity giving the requested ratio, floored to a 64KB region."""
    capacity = int(working_set_bytes / ratio)
    capacity = capacity // UVM_REGION_SIZE * UVM_REGION_SIZE

    return max(capacity, UVM_REGION_SIZE)


working_sets = load_working_sets()
missing_working_set = set()

# sbin_claude: integrated benchmark set (v4 originals + the suites
# ported from ~/vdram_v2). Every entry has size arguments below.
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


script_path = os.path.dirname(os.path.realpath(__file__)) + "/benchmarks/"
# sbin_claude: graphbig inputs now live in this workspace
# (scripts/graphbig_input) instead of the hard-coded ~/vdram path.
GRAPHBIG_DATASET = (os.path.dirname(os.path.realpath(__file__))
                    + "/graphbig_input/roadNet_CA")
slurm_node = 0

for config in configs:
    for benchmark in benchmarks:
        slurm_node = (slurm_node + 1) % 4
        # slurm_node = 3
        print(config, benchmark)
        # sbin_claude_utopia: sweep configs (utopia_restseg_ratios keys) have
        # no directory from 2_copy_benchmarks.sh, so create it here.
        os.makedirs(script_path + config, exist_ok=True)
        submit_file_name = script_path + config + "/" + benchmark + ".sh"
        submit_file = open(submit_file_name, "w")
        submit_file.write("#!/bin/bash\n")
        submit_file.write("cd ../samples\n")
        submit_file.write("cd " + benchmark + "\n")
        submit_file.write("nohup srun -J " + benchmark + "_" + config + " -w compasslab" + str(slurm_node+1) + " \\\n\t")
        # submit_file.write("nohup srun -J " + benchmark + "_" + config + " \\\n\t")
        submit_file.write("--output=" + script_path + "logs/" + benchmark + "_" + config + ".out" + " \\\n\t")
        submit_file.write("--error=" + script_path + "logs/" + benchmark + "_" + config + ".err" + " \\\n\t")
        # submit_file.write("nohup ./" + benchmark + " ")
        submit_file.write("./" + benchmark + " ")
        submit_file.write("-timing ")
        submit_file.write("-parallel ")
        submit_file.write("-arch=gcn3 ")
        submit_file.write("-report-all ")
        # submit_file.write("-verify ")
        submit_file.write("-disable-rtm \\\n\t")
        submit_file.write("-metric-file-name=exp_" + config + "_" + benchmark + " \\\n\t")
        submit_file.write("-progress-interval=1000000 \\\n\t")

        if config == 'baseline':
            pass
        elif config == 'ideal-l1tlb':
            submit_file.write("-gpu=ideal-l1tlb ")
        elif config == 'virtual-caching':
            submit_file.write("-gpu=virtual-caching ")
        elif config == 'virtual-caching-nofbt':
            submit_file.write("-gpu=virtual-caching ")
            submit_file.write("-fbt-entries=0 ")
        elif config == 'uvm':
            submit_file.write("-uvm ")
        elif config == 'uvm-ideal':
            submit_file.write("-uvm -uvm-ideal ")
        elif config in oversubscription_ratios:
            ratio = oversubscription_ratios[config]
            submit_file.write("-uvm ")
            if benchmark in working_sets:
                capacity = oversub_capacity(working_sets[benchmark], ratio)
                submit_file.write("-uvm-gpu-memory-capacity="
                                  + str(capacity) + " ")
            else:
                missing_working_set.add(benchmark)
                submit_file.write("-uvm-oversubscription-ratio="
                                  + str(ratio) + " ")
        # sbin_claude_utopia: utopia configs. The plain config relies on the
        # runner defaults; sweep configs pin the RestSeg ratio explicitly.
        elif config == 'utopia':
            submit_file.write("-gpu=utopia ")
            submit_file.write("-utopia-restseg-ratio=0.8 ")
        elif config in utopia_restseg_ratios:
            submit_file.write("-gpu=utopia ")
            submit_file.write("-utopia-restseg-ratio="
                              + str(utopia_restseg_ratios[config]) + " ")
        # sbin_claude_avatar: avatar configs. The plain config relies on the
        # runner defaults; sweep configs pin the compress ratio explicitly.
        elif config == 'avatar':
            submit_file.write("-gpu=avatar ")
            submit_file.write("-avatar-frag=true ")
            submit_file.write("-avatar-mod-entries=8 ")
            submit_file.write("-avatar-compress-ratio=0.675 ")
            submit_file.write("-avatar-validation-latency=7 ")
            submit_file.write("-avatar-compress-ratio=0.675 ")
        elif config in avatar_compress_ratios:
            submit_file.write("-gpu=avatar ")
            submit_file.write("-avatar-frag=false ")
            submit_file.write("-avatar-compress-ratio="
                              + str(avatar_compress_ratios[config]) + " ")
        # sbin_claude_hpt: hpt configs. The plain config relies on the runner
        # default (1 access per walk); sweep configs pin the access count.
        elif config == 'hpt':
            submit_file.write("-gpu=hpt ")
        elif config in hpt_accesses_per_walk:
            submit_file.write("-gpu=hpt ")
            submit_file.write("-hpt-accesses-per-walk="
                              + str(hpt_accesses_per_walk[config]) + " ")
        # sbin_claude_softwalker: softwalker configs. The plain config relies
        # on the runner defaults; sweep configs pin one knob explicitly.
        elif config in ('softwalker', 'latpc'):
            submit_file.write("-gpu=" + config + " ")
        # sbin_claude_latpc: LATP tag ablation configs.
        elif config in latpc_variants:
            variant = latpc_variants[config]
            submit_file.write("-gpu=latpc ")
            submit_file.write("-latpc-addr-tag="
                              + str(variant['addr_tag']).lower() + " ")
        elif config in softwalker_in_tlb_mshr_max:
            submit_file.write("-gpu=softwalker ")
            submit_file.write("-sw-in-tlb-mshr-max="
                              + str(softwalker_in_tlb_mshr_max[config]) + " ")
        elif config in softwalker_slots_per_cu:
            submit_file.write("-gpu=softwalker ")
            submit_file.write("-sw-slots-per-cu="
                              + str(softwalker_slots_per_cu[config]) + " ")
        else:
            raise ValueError("unknown config " + config)
        submit_file.write("\\\n\t")

        # limit super long benchmarks
        if benchmark == 'fastwalshtransform':
            submit_file.write("-max-inst=50000000 ") # 50M
        elif benchmark == 'atax':
            submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'bicg':
            submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'fir':
            submit_file.write("-max-inst=100000000 ") # 100M
        elif benchmark == 'floydwarshall':
            submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'graphbig_betweennesscentr':
            submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'graphbig_bfs':
            submit_file.write("-max-inst=200000000 ") # 200M
        elif benchmark == 'graphbig_connectedcomp':
            submit_file.write("-max-inst=50000000 ") # 50M
        elif benchmark == 'graphbig_gc':
            submit_file.write("-max-inst=200000000 ") # 200M
        elif benchmark == 'graphbig_kcore':
            submit_file.write("-max-inst=50000000 ") # 50M
        elif benchmark == 'graphbig_trianglecount':
            submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'gups':
            submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'rodinia_backprop':
            submit_file.write("-max-inst=30000000 ") # 30M
        # elif benchmark == 'nbody':
        #     submit_file.write("-max-inst=10000000 ") # 10M
        elif benchmark == 'pagerank':
            submit_file.write("-max-inst=10000000 ") # 10M
        else:
            submit_file.write("-max-inst=100000000 ") # 100M
        submit_file.write("\\\n\t")

        # set benchmark specific parameters (scaled so the working set
        # exceeds the L2 TLB capacity (4096 entries x 4 KB = 16 MB) at 4 KB
        # pages; target footprint ~24-64 MB per benchmark)
        if benchmark == 'altis_cfd':
            submit_file.write("-size=524288 ")
        if benchmark == 'atax':
            submit_file.write("-x=2048 -y=2080 ")
        if benchmark == 'babelstream':
            submit_file.write("-size=4194304 ")
        if benchmark == 'bfs':
            submit_file.write("-node=1048576 -degree=8 ")
        if benchmark == 'bicg':
            submit_file.write("-x=2048 -y=2080 ")
        if benchmark == 'fastwalshtransform':
            submit_file.write("-length=16777216 ")
        if benchmark == 'fft':
            submit_file.write("-bytes=67108864 ")
        if benchmark == 'fir':
            submit_file.write("-length=16777216 ")
        if benchmark == 'floydwarshall':
            submit_file.write("-node=2048 ")
        if benchmark == 'kmeans':
            submit_file.write("-points=131072 -clusters=4 -features=32 -max-iter=3 ")
        if benchmark == 'matrixmultiplication':
            submit_file.write("-x=4096 -y=4096 -z=4096 ")
        if benchmark == 'matrixtranspose':
            submit_file.write("-width=2048 ")
        if benchmark == 'nbody':
            submit_file.write("-particles=524288 -iter=4 ")
        if benchmark == 'npb_ep':
            submit_file.write("-size=16777216 ")
        if benchmark == 'pagerank':
            submit_file.write("-node=32768 -sparsity=0.01 ")
        if benchmark == 'parboil_cutcp':
            # 2M atoms x 16 B = 33.6 MB (grid-side reduced 16->8 so the
            # gridPoints x atoms loop work stays ~1.07G, same as before)
            submit_file.write("-num-atoms=2097152 -grid-side=8 ")
        if benchmark == 'parboil_sgemm':
            submit_file.write("-size=1536 ")
        if benchmark == 'polybench_2dconv':
            submit_file.write("-size=2048 ")
        if benchmark == 'polybench_3dconv':
            submit_file.write("-size=200 ")
        if benchmark == 'polybench_3mm':
            submit_file.write("-size=1600 ")
        if benchmark == 'polybench_correlation':
            submit_file.write("-size=3072 ")
        if benchmark == 'polybench_fdtd2d':
            submit_file.write("-size=1536 -tmax=5 ")
        if benchmark == 'polybench_gemm':
            submit_file.write("-size=1536 ")
        if benchmark == 'polybench_jacobi2d':
            submit_file.write("-size=2048 -tsteps=3 ")
        if benchmark == 'polybench_mvt':
            submit_file.write("-size=3600 ")
        if benchmark == 'polybench_syr2k':
            submit_file.write("-size=2048 -inner-size=2048 ")
        if benchmark == 'relu':
            submit_file.write("-length=16777216 ")
        if benchmark == 'rodinia_backprop':
            submit_file.write("-input=12288 -hidden=3072 -output=8 ")
        if benchmark == 'rodinia_gaussian':
            submit_file.write("-size=2048 ")
        if benchmark == 'rodinia_hotspot':
            submit_file.write("-size=2048 -iterations=3 ")
        if benchmark == 'rodinia_hotspot3d':
            submit_file.write("-size=200 -iterations=1 ")
        if benchmark == 'rodinia_lavamd':
            submit_file.write("-num-boxes=8 -particles-per-box=8192 ")
        if benchmark == 'rodinia_lud':
            submit_file.write("-size=3072 ")
        if benchmark == 'rodinia_pathfinder':
            submit_file.write("-rows=3072 -cols=6144 ")
        if benchmark == 'rodinia_srad':
            submit_file.write("-size=3072 -iterations=3 ")
        if benchmark == 'simpleconvolution':
            submit_file.write("-width=2048 -height=2048 -mask-size=7")
        if benchmark == 'spmv':
            submit_file.write("-dim=1048576 -sparsity=0.000005 ")
        if benchmark == 'stencil2d':
            submit_file.write("-row=4096 -col=4096 -iter=1 ")
        if benchmark == 'tango_blackscholes':
            submit_file.write("-size=4194304 ")
        if benchmark == 'vectoradd':
            submit_file.write("-width=2048 -height=2048 ")
        if benchmark == 'polybench_doitgen':
            submit_file.write("-nr=128 -nq=128 -np=128 ")
        if benchmark == 'polybench_gemver':
            submit_file.write("-size=2048 ")
        if benchmark == 'polybench_gesummv':
            submit_file.write("-size=2048 ")
        if benchmark == 'polybench_jacobi1d':
            submit_file.write("-size=4194304 -tsteps=100 ")
        if benchmark == 'polybench_lu':
            submit_file.write("-size=3072 -k=1 ")
        if benchmark == 'pannotia_color':
            submit_file.write("-num-nodes=524288 -num-edges=4194304 ")
        if benchmark == 'pannotia_mis':
            submit_file.write("-num-nodes=524288 -num-edges=4194304 ")
        if benchmark == 'pannotia_sssp':
            submit_file.write("-num-nodes=524288 -num-edges=4194304 ")
        if benchmark == 'gups':
            submit_file.write("-table-size=4194304 ")
        if benchmark == 'reduction':
            submit_file.write("-size=4194304 ")
        if benchmark == 'graphbig_betweennesscentr':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " -num-roots=8 ")
        if benchmark == 'graphbig_bfs':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " ")
        if benchmark == 'graphbig_connectedcomp':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " ")
        if benchmark == 'graphbig_degreecentr':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " ")
        if benchmark == 'graphbig_gc':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " -max-iterations=64 ")
        if benchmark == 'graphbig_kcore':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " -kcore=3 ")
        if benchmark == 'graphbig_sssp':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " ")
        if benchmark == 'graphbig_trianglecount':
            submit_file.write("-dataset=" + GRAPHBIG_DATASET + " ")
        if benchmark == 'nw':
            submit_file.write("-length=4096")

        submit_file.write("&")
        submit_file.close()

# sbin_codex: report which benchmarks still need a baseline measurement.
if missing_working_set:
    print()
    print("no working_set_bytes in results/metrics.csv for: "
          + ", ".join(sorted(missing_working_set)))
    print("  -> these fall back to -uvm-oversubscription-ratio "
          "(allocation-based).")
    print("  -> run the baseline config and 5_collect_metrics.py, then "
          "re-run this script.")
