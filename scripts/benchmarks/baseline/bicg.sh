#!/bin/bash
cd ../samples
cd bicg
nohup srun -J bicg_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/bicg_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/bicg_baseline.err \
	./bicg -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-x=8192 -y=8192 -metric-file-name=exp_baseline_bicg &