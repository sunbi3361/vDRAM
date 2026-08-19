#!/bin/bash
cd ../samples
cd spmv
nohup srun -J spmv_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/spmv_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/spmv_baseline.err \
	./spmv -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-dim=16384 -sparsity=0.05 -metric-file-name=exp_baseline_spmv &