# PROJECT KNOWLEDGE BASE
# branch: main | commit: bba8240 | generated: 2026-08-19

새롭게 파일 수정 시

수정 전 코드는 주석 처리
수정된 코드에는 sbin_codex 표기

필수 사용 flag: -timing -parallel -gpu=r9nano -arch=gcn3 -report-all

---

## OVERVIEW

Two independent Go modules.
- akita: discrete-event simulation framework.
- mgpusim: AMD GCN3 GPU simulator built on akita.
- mgpusim/go.mod replaces github.com/sarchlab/akita/v4 with ../akita.
- scripts/ is a gitignored experiment workspace.

## STRUCTURE

Maintained source:
- akita/
- mgpusim/
- mgpusim/amd/samples/

Generated or copied areas (do not treat as source of truth):
- scripts/benchmarks/
- .omo/
- akita/daisen/static/dist/ and akita/monitoring/web/dist/
- HSACO/binaries/

## WHERE TO LOOK

- akita/sim/ and akita/simulation/: engine and component wiring.
- mgpusim/amd/samples/runner/, mgpusim/amd/driver/,
  mgpusim/amd/samples/runner/timingconfig/: run control and config.
- mgpusim/amd/: GCN3 implementation.
- scripts/: benchmark automation.

## CODE MAP

Core roles. Refs are unmeasured; LSP/codegraph were unavailable.
- sim.Engine: discrete-event engine.
- simulation.Builder: builds and wires a simulation.
- runner.Runner: sample/simulation driver.
- timingconfig.Builder: constructs timing GPU configuration.
- driver.Driver: host command submission interface.
- cp.CommandProcessor: GPU command processor.
- cu.ComputeUnit: timing compute unit.
- emu.ComputeUnit: functional emulator compute unit.
- insts.Disassembler: instruction decoding/disassembly.

## CONVENTIONS

- Comment out pre-edit code; mark modified code with sbin_codex.
- Mandatory flags: -timing -parallel -gpu=virtual-caching -arch=gcn3 -report-all. <!-- sbin_codex -->
- Use the custom Go toolchain:
  GOROOT=/home/sbin/tools/go1.26
  GOPATH=/home/sbin/tools/go1.26/gopath
  PATH append.

## ANTI-PATTERNS (THIS PROJECT)

- Do not rely on the NVIDIA path; it is prototype/unsupported.
- Virtual-caching uses a simplified virtual-address model for L1V, L1S, and L2 data caches; L2D misses/refills and dirty writebacks translate through the per-slice L2 address translator, shared L2TLB, and GMMU at the DRAM boundary. <!-- sbin_codex -->
- Do not assume mi300a or other unhandled GPU selectors target distinct hardware;
  they may fall through to r9nano defaults. <!-- sbin_codex -->
- Do not edit generated/copied areas as authoritative source.

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
cd mgpusim/amd/samples/matrixtranspose && go build
./matrixtranspose -timing -parallel -gpu=r9nano -arch=gcn3 -report-all -verify -disable-rtm \
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

## NOTES

- Generated for commit bba8240 on branch main, 2026-08-19.
- CODE MAP symbol roles are heuristic; centrality was not measured.
- AMD GCN3 is the stable target; NVIDIA support is experimental and unsupported.
