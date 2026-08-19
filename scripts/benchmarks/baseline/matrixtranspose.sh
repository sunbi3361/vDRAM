#!/bin/bash
cd ../samples
cd matrixtranspose
nohup srun -J matrixtranspose_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/matrixtranspose_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/matrixtranspose_baseline.err \
	./matrixtranspose -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-width=2048 -metric-file-name=exp_baseline_matrixtranspose &