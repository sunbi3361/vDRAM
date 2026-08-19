#!/bin/bash
cd ../samples
cd floydwarshall
nohup srun -J floydwarshall_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/floydwarshall_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/floydwarshall_baseline.err \
	./floydwarshall -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=10000000 \
	-node=2048 -metric-file-name=exp_baseline_floydwarshall &