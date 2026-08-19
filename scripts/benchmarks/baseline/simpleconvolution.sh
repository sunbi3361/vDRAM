#!/bin/bash
cd ../samples
cd simpleconvolution
nohup srun -J simpleconvolution_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/simpleconvolution_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/simpleconvolution_baseline.err \
	./simpleconvolution -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-metric-file-name=exp_baseline_simpleconvolution &