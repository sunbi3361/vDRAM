#!/bin/bash
cd ../samples
cd bfs
nohup srun -J bfs_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/bfs_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/bfs_baseline.err \
	./bfs -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-node=1048576 -degree=8 -metric-file-name=exp_baseline_bfs &