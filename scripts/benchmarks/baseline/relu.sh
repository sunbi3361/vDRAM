#!/bin/bash
cd ../samples
cd relu
nohup srun -J relu_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/relu_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/relu_baseline.err \
	./relu -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-length=16777216 -metric-file-name=exp_baseline_relu &