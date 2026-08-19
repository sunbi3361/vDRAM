#!/bin/bash
cd ../samples
cd atax
nohup srun -J atax_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/atax_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/atax_baseline.err \
	./atax -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-x=8192 -y=8192 -metric-file-name=exp_baseline_atax &