#!/bin/bash
cd ../samples
cd nbody
nohup srun -J nbody_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/nbody_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/nbody_baseline.err \
	./nbody -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-particles=524288 -iter=4 -metric-file-name=exp_baseline_nbody &