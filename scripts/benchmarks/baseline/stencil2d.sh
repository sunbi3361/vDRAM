#!/bin/bash
cd ../samples
cd stencil2d
nohup srun -J stencil2d_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/stencil2d_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/stencil2d_baseline.err \
	./stencil2d -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-row=4096 -col=4096 -iter=1 -metric-file-name=exp_baseline_stencil2d &