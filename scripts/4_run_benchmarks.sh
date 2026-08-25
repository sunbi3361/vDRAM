#!/bin/bash
# sbin_gmmu_omo: launch the generated runners for every config x benchmark.
# Requires 2_copy_benchmarks.sh, 2_1_copy_binary.sh and 3_gen_runners.py to
# have run first.

# configs=(baseline unified ideal-l1tlb ideal-l2tlb unified-infinite-l1tlb unified-infinite-l2tlb infinite-l1tlb infinite-l2tlb)
# configs=(baseline unified ideal-l1tlb ideal-l2tlb unified-infinite-l1tlb unified-infinite-l2tlb)
# configs=(baseline ideal-l1tlb)
# configs=(unified unified-infinite-l1tlb)
configs=(virtual-caching)
# configs=(baseline virtual-caching)
configs=(baseline ideal-l1tlb virtual-caching uvm uvm-ideal)
# configs=(uvm uvm-ideal)
# configs=(uvm-ideal)
# configs=(baseline)

benchmarks=(
    # 'atax'
    'bfs'
    # 'bicg'
    # 'fastwalshtransform'
    'fft'
    # 'fir'
    # 'floydwarshall'
    'kmeans'
    'matrixmultiplication'
    'matrixtranspose'
    'nbody'
    'nw'
    'pagerank'
    'relu'
    'simpleconvolution'
    'spmv'
    'stencil2d'
    'vectoradd'
)

# benchmarks=(
#     'kmeans'
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
