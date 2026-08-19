#!/bin/bash
cd ../samples
cd fft
nohup srun -J fft_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/fft_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/fft_baseline.err \
	./fft -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-bytes=67108864 -metric-file-name=exp_baseline_fft &