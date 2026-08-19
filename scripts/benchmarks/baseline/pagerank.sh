#!/bin/bash
cd ../samples
cd pagerank
nohup srun -J pagerank_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/pagerank_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/pagerank_baseline.err \
	./pagerank -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=100000000 \
	-metric-file-name=exp_baseline_pagerank &