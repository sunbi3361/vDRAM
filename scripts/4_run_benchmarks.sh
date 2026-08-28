#!/bin/bash
# sbin_gmmu_omo: launch the generated runners for every config x benchmark.
# Requires 2_copy_benchmarks.sh, 2_1_copy_binary.sh and 3_gen_runners.py to
# have run first.


configs=(baseline ideal-l1tlb virtual-caching utopia avatar hpt softwalker latpc uvm uvm-ideal)
# configs=(uvm uvm-ideal)
# configs=(uvm-oversub-150)
# configs=(virtual-caching)
# configs=(utopia) # sbin_claude_utopia: active selection (last assignment wins)
# configs=(avatar) # sbin_claude_avatar: speculative translation config
# configs=(hpt) # sbin_claude_hpt: FS-HPT hashed page table config
# configs=(softwalker) # sbin_claude_softwalker: software page-walk config
# configs=(baseline)
# configs=(softwalker latpc)
configs=(baseline ideal-l1tlb virtual-caching virtual-caching-nofbt utopia avatar hpt softwalker latpc)
# configs=(softwalker latpc)
# configs=(uvm-ideal)
configs=(avatar)

benchmarks=(
    'atax'
    'bicg'
    'bfs'
    'fft'
    'kmeans'
    'matrixmultiplication'
    'matrixtranspose'
    'nbody'
    'nw'
    'pagerank'
    'relu'
    'spmv'
    'stencil2d'
    'vectoradd'
)

# benchmarks=(
#     'atax'
#     'bicg'
# )

# sbin_claude: the full integrated set (v4 originals + the suites ported from
# ~/vdram_v2). 3_gen_runners.py generates a runner for every one of these, so
# uncomment this block -- it is the last assignment, so it wins -- to run
# everything instead of the selection above.
# benchmarks=(
#     'altis_cfd'
#     'atax'
#     'babelstream'
#     'bfs'
#     'bicg'
#     'cache_latency'
#     'fastwalshtransform'
#     'fft'
#     'fir'
#     'floydwarshall'
#     'graphbig_betweennesscentr'
#     'graphbig_bfs'
#     'graphbig_connectedcomp'
#     'graphbig_degreecentr'
#     'graphbig_gc'
#     'graphbig_kcore'
#     'graphbig_sssp'
#     'graphbig_trianglecount'
#     'gups'
#     'kmeans'
#     'matrixmultiplication'
#     'matrixtranspose'
#     'nbody'
#     'npb_ep'
#     'nw'
#     'pagerank'
#     'pannotia_color'
#     'pannotia_mis'
#     'pannotia_sssp'
#     'parboil_cutcp'
#     'parboil_sgemm'
#     'polybench_2dconv'
#     'polybench_3dconv'
#     'polybench_3mm'
#     'polybench_correlation'
#     'polybench_doitgen'
#     'polybench_fdtd2d'
#     'polybench_gemm'
#     'polybench_gemver'
#     'polybench_gesummv'
#     'polybench_jacobi1d'
#     'polybench_jacobi2d'
#     'polybench_lu'
#     'polybench_mvt'
#     'polybench_syr2k'
#     'reduction'
#     'relu'
#     'rodinia_backprop'
#     'rodinia_gaussian'
#     'rodinia_hotspot'
#     'rodinia_hotspot3d'
#     'rodinia_lavamd'
#     'rodinia_lud'
#     'rodinia_pathfinder'
#     'rodinia_srad'
#     'simpleconvolution'
#     'spmv'
#     'stencil2d'
#     'tango_blackscholes'
#     'vectoradd'
# )

for config in ${configs[@]};
do
    for benchmark in ${benchmarks[@]};
    do
        echo $config $benchmark
        cd benchmarks/$config
        bash ${benchmark}.sh
        cd ../..
    done
done
