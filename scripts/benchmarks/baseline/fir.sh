#!/bin/bash
cd ../samples
cd fir
nohup srun -J fir_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/fir_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/fir_baseline.err \
	./fir -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-length=16777216 -metric-file-name=exp_baseline_fir &