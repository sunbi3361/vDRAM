#!/bin/bash
cd ../samples
cd nw
nohup srun -J nw_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/nw_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/nw_baseline.err \
	./nw -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-metric-file-name=exp_baseline_nw &