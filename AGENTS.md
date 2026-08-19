새롭게 파일 수정 시

수정 전 코드는 주석 처리
수정된 코드에는 sbin_codex 표기

필수 사용 flag: -timing -parallel -gpu=r9nano -arch=gcn3 -report-all

---

## COMMANDS

```bash
# Toolchain (go is NOT on the default PATH)
export GOROOT=/home/sbin/tools/go1.26
export GOPATH=/home/sbin/tools/go1.26/gopath
export PATH=$PATH:$GOROOT/bin:$GOPATH/bin

# Build
( cd akita && go build ./... )
( cd mgpusim && go build ./... )

# Unit tests
cd mgpusim && ginkgo -r --skip-package=mccl
cd akita && ginkgo -r

# Lint
cd mgpusim && golangci-lint run ./amd/... --timeout=10m

# Acceptance
cd mgpusim/amd/tests/acceptance && go build && ./acceptance -num-gpu=1 -arch=gcn3

# Determinism
cd mgpusim/amd/tests/deterministic && python3 test.py

# Sample run (mandatory flags)
cd mgpusim/amd/samples/fir && go build
./matrixtranspose -timing -parallel -arch=gcn3 -report-all -verify -disable-rtm \
	-metric-file-name=exp_baseline_matrixtranspose \
	-progress-interval=1000000 \
	-max-inst=100000000 \
	-width=512 &

# Experiment pipeline
cd scripts
bash 0_clean.sh
python3 1_compile_benchmarks.py
bash 2_copy_benchmarks.sh && bash 2_1_copy_binary.sh
python3 3_gen_runners.py
bash 4_run_benchmarks.sh
python3 5_collect_metrics.py
```