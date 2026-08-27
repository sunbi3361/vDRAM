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
configs=(baseline ideal-l1tlb virtual-caching virtual-caching-nofbt utopia avatar hpt softwalker)
configs=(latpc)

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
