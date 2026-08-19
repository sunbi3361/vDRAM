#!/bin/bash
cd ../samples
cd kmeans
nohup srun -J kmeans_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/kmeans_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/kmeans_baseline.err \
	./kmeans -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-points=131072 -clusters=4 -features=32 -max-iter=3 -metric-file-name=exp_baseline_kmeans &