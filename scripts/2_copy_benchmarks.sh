#!/bin/bash
# sbin_gmmu_omo: copy the samples tree into the generated benchmarks/
# workspace and create one config directory per simulator configuration plus
# the shared logs directory.

mkdir benchmarks
cp -r ../mgpusim/amd/samples benchmarks

mkdir benchmarks/baseline
mkdir benchmarks/ideal-l1tlb
mkdir benchmarks/virtual-caching
mkdir benchmarks/unified

mkdir benchmarks/logs

chmod -R 777 benchmarks
