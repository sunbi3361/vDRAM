#!/bin/bash
cd ../samples
cd fastwalshtransform
nohup srun -J fastwalshtransform_baseline -w compasslab4 \
	--output=/home/sbin/vdram_v3/scripts/benchmarks/logs/fastwalshtransform_baseline.out \
	--error=/home/sbin/vdram_v3/scripts/benchmarks/logs/fastwalshtransform_baseline.err \
	./fastwalshtransform -timing -parallel -report-all -verify -disable-rtm \
	-max-inst=50000000 \
	-length=16777216 -metric-file-name=exp_baseline_fastwalshtransform &