#!/bin/bash
cd ../samples
cd vectoradd
nohup srun -J vectoradd_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/vectoradd_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/vectoradd_baseline.err \
	./vectoradd -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-width=2048 -height=2048 -metric-file-name=exp_baseline_vectoradd &