#!/home/sbin/tools/Python3.10.14/bin/python3

import os

configs = [
    'baseline',
    # 'ideal-l1tlb',
    # 'virtual-caching',
    # 'unified',
    # 'unified-infinite-l1tlb',
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


script_path = os.path.dirname(os.path.realpath(__file__)) + "/benchmarks/"
slurm_node = 0

for config in configs:
    for benchmark in benchmarks:
        # slurm_node = (slurm_node + 1) % 3
        slurm_node = 3
        print(config, benchmark)
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
        submit_file.write("-report-all ")
        submit_file.write("-verify ")
        submit_file.write("-disable-rtm \\\n\t")
        # submit_file.write("-progress-interval=1000000 \\\n\t")

        if config == 'baseline':
            pass
        else:
            if config == 'unified':
                submit_file.write("-use-unified-memory ")
            elif config == 'unified-ideal-l1tlb':
                submit_file.write("-use-unified-memory -ideal-l1tlb ")
            elif config == 'unified-ideal-l2tlb':
                submit_file.write("-use-unified-memory -ideal-l2tlb ")
            elif config == 'ideal-l1tlb':
                submit_file.write("-ideal-l1tlb ")
            elif config == 'ideal-l2tlb':
                submit_file.write("-ideal-l2tlb ")
            elif config == 'unified-infinite-l1tlb':
                submit_file.write("-use-unified-memory -infinite-l1tlb ")
            elif config == 'unified-infinite-l2tlb':
                submit_file.write("-use-unified-memory -infinite-l2tlb ")
            elif config == 'infinite-l1tlb':
                submit_file.write("-infinite-l1tlb ")
            elif config == 'infinite-l2tlb':
                submit_file.write("-infinite-l2tlb ")
            elif config == 'virtual-caching':
                submit_file.write("-virtual-caching ")
            else:
                raise ValueError("unknown config " + config)
            submit_file.write("\\\n\t")

        # limit super long benchmarks
        if benchmark == 'fastwalshtransform':
            submit_file.write("-max-inst=50000000 ") # 50M
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
        elif benchmark == 'nbody':
            submit_file.write("-max-inst=100000000 ") # 100M
        else:
            submit_file.write("-max-inst=100000000 ") # 100M
        submit_file.write("\\\n\t")

        # set benchmark specific parameters (scaled so the working set
        # exceeds the L2 TLB capacity (4096 entries x 4 KB = 16 MB) at 4 KB
        # pages; target footprint ~24-64 MB per benchmark)
        if benchmark == 'altis_cfd':
            submit_file.write("-size=524288 ")
        if benchmark == 'atax':
            submit_file.write("-x=8192 -y=8192 ")
        if benchmark == 'babelstream':
            submit_file.write("-size=4194304 ")
        if benchmark == 'bfs':
            submit_file.write("-node=1048576 -degree=8 ")
        if benchmark == 'bicg':
            submit_file.write("-x=8192 -y=8192 ")
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
        if benchmark == 'matrixtranspose':
            submit_file.write("-width=2048 ")
        if benchmark == 'nbody':
            submit_file.write("-particles=524288 -iter=4 ")
        if benchmark == 'npb_ep':
            submit_file.write("-size=16777216 ")
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
        if benchmark == 'spmv':
            submit_file.write("-dim=16384 -sparsity=0.05 ")
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
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA -num-roots=8 ")
        if benchmark == 'graphbig_bfs':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA ")
        if benchmark == 'graphbig_connectedcomp':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA ")
        if benchmark == 'graphbig_degreecentr':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA ")
        if benchmark == 'graphbig_gc':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA -max-iterations=64 ")
        if benchmark == 'graphbig_kcore':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA -kcore=3 ")
        if benchmark == 'graphbig_sssp':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA ")
        if benchmark == 'graphbig_trianglecount':
            submit_file.write("-dataset=/home/sbin/vdram/scripts/graphbig_input/roadNet_CA ")

        submit_file.write("-metric-file-name=exp_" + config + "_" + benchmark + " ")
        submit_file.write("&")
        submit_file.close()
