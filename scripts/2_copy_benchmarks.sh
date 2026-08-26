#!/bin/bash
# sbin_gmmu_omo: copy the samples tree into the generated benchmarks/
# workspace and create one config directory per simulator configuration plus
# the shared logs directory.

mkdir benchmarks
cp -r ../mgpusim/amd/samples benchmarks

mkdir benchmarks/baseline
mkdir benchmarks/ideal-l1tlb
mkdir benchmarks/virtual-caching
mkdir benchmarks/uvm
mkdir benchmarks/uvm-ideal
mkdir benchmarks/uvm-oversub-150
mkdir benchmarks/utopia # sbin_claude_utopia (ratio-sweep dirs are created by 3_gen_runners.py)
mkdir benchmarks/avatar # sbin_claude_avatar (ratio-sweep dirs are created by 3_gen_runners.py)
mkdir benchmarks/hpt # sbin_claude_hpt (access-sweep dirs are created by 3_gen_runners.py)
mkdir benchmarks/softwalker # sbin_claude_softwalker (sweep dirs are created by 3_gen_runners.py)

mkdir benchmarks/logs

chmod -R 777 benchmarks
