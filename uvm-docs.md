# UVM 구현 요약 (uvm-docs)

> 대상 스펙: `uvm-manager.md` (Draft v0.10)
> 상세 기능 대조표: `UVM_features.md`
> 모든 수정 코드는 AGENTS.md 관례에 따라 `sbin_codex` 마커 사용

이 문서는 **무엇을 왜 바꿨는지**를 요약한다. 스펙 조항별 대응은 `UVM_features.md`를 본다.

---

## 1. 왜 재작성했나

기존 구현은 마이그레이션 1건마다 GPU 전체를 정지시켰다:

```text
RDMADrainCmd → ShootDownCommand(전 CU 파이프라인 flush + 전 캐시 flush + 전 TLB flush)
             → MemCopy → GPURestartReq → RDMARestartCmd
```

측정 결과 `matrixtranspose -width=64`가 110초 동안 **마이그레이션 1건**만 완료한 뒤
정지했고, `-uvm-ideal`은 `no output port for Driver.UVM`으로 패닉했다.

`uvm-manager.md`는 이 방식을 명시적으로 금지한다 (§2.1, §19, §21):
"UVM must not reuse the existing heavyweight ShootDownCommand / GPURestartReq path",
"No full-TLB flush is permitted", "CU pipeline flush on migration = disabled".

따라서 제어 평면을 **64KB 영역 단위**로 전면 재구성했다.

---

## 2. 제어 평면 구조

```text
                       UVMDriver (amd/driver)
                            |  PCIe (ideal 모드는 directconnection)
                            v
                   Command Processor  ToUVMDriver
                            |  GPU 내부 directconnection  ToUVMInternal
                   /                          \
              GMMU (UVM)                UVMAccessCounter (Ctrl)
                  |                             |
       +----------+-----------+          remote read 계수
       |                      |          remote write stall / replay
   replay 큐            range TLB 무효화        region drain
 (스톨된 번역)        (모든 L1/L2 TLB)
```

캐시 range WB+INV는 CP가 `ToCaches`로 직접 브로드캐스트한다.
`ShootDownCommand` / `GPURestartReq` / `RDMADrainCmd`는 **비-UVM unified-memory
마이그레이션 전용**으로 남으며 UVM 경로에서는 사용되지 않는다.

---

## 3. 주요 변경 사항

### 3.1 akita — 비정지 range 프리미티브 (신규)

| 파일 | 내용 |
|---|---|
| `mem/vm/tlb/rangeinvalidate.go` | `InvalidateRangeReq/Rsp`. PID+VA 범위 무효화. **컴포넌트 state를 바꾸지 않으므로** 무관한 번역은 계속 진행. 진행 중 lookup은 `staleOnFill`로 표시해 채워질 때 설치하지 않는다 |
| `mem/cache/rangeprotocol.go` | `RangeFlushReq/Rsp`. 가상 태그(PID+VA)와 물리 태그(프레임 목록) **양쪽 모두** 매칭 |
| `mem/cache/{writearound,writethrough}/rangeflush.go` | 범위 매칭 in-flight 드레인 + 무효화. 더티 없음 |
| `mem/cache/writeback/rangeflush.go` | 범위 매칭 더티 블록만 write-back. 영역이 완전히 조용해질 때까지 **패스를 반복**한다 |
| `mem/vm/uvmprotocol.go` | `UVMTLBInvalidate`, `UVMFaultReplay`, `UVMRemoteRetry`, `UVMDrainRange` |
| `mem/vm/gmmu/uvmctrl.go` | GMMU가 range 무효화 코디네이터 (스펙 §21.1) |

### 3.2 akita — GMMU / AddressTranslator

- **replay 큐 분리**: 폴트 발생 시 번역 트랜잭션이 `walkingTranslations`에서 빠져나와
  `replayQueue`로 이동한다. 폴트 폭주가 page-walk 슬롯을 잠식하지 못한다 (스펙 §22).
- `UVMFaultReplayReq(64KB)` 1건으로 해당 범위의 모든 스톨 번역을 재실행.
- AddressTranslator에 **region drain**과 **remote write 재번역(retry)** 경로 추가.

### 3.3 mgpusim — CP

- `amd/timing/cp/uvmMiddleware.go` (신규): GPU측 UVM 제어 엔드포인트.
  폴트 릴레이, range TLB 무효화, 캐시 range flush(AT drain 선행), AC 알림/리셋,
  remote drain을 모두 처리.

### 3.4 mgpusim — 드라이버

| 파일 | 내용 |
|---|---|
| `uvm_fault.go` | Coalescing 키 = `(PID, GPU, 64KB)`. **활성 트랜잭션 1개 FIFO** (§8.4). 20us는 유니크 트랜잭션당 1회 |
| `uvm_prefetch`(`uvm_tbn.go`) | occupancy = `GPUResident \| CurrentFaultExpanded64KB`, 엄격히 `>51%`, 64KB→2MB, VA Block 경계 준수 |
| `uvm_migration.go` (신규) | 소스·목적지 물리주소가 모두 연속인 **최대 run**당 MemCopy 1건 → CP → DMA Engine → PCIe |
| `uvm_control.go` (신규) | PTE 갱신 / range 무효화 / replay / remote drain 시퀀싱 |
| `uvm_eviction.go` | 64KB victim, migration-recency LRU, 선제 퇴출(64KB headroom) |
| `uvm_quiescence.go` | **삭제** (전역 quiesce) |

### 3.5 mgpusim — Access Counter (GPU측으로 이동)

`amd/timing/accesscounter`를 CPU측(PCIe 건너편)에서 **GPU당 1개**로 옮겨,
AddressTranslator의 remote egress와 RDMA ingress 사이에 삽입했다. 번역이
CPU-remote로 판정한 직후를 관측하므로 L2 TLB에 캐싱된 remote PTE도 계수를
우회하지 못한다 (스펙 §6.1, §14).

- remote **read**: PCIe 포워딩 + 64KB 카운터 증가, 임계값(기본 8)에서 알림 1회
- remote **write**: **호스트 메모리에 커밋하지 않고 보류** (스펙 §15). 영역이
  GPU-local이 되면 `UVMRemoteRetryRsp`로 AT에 돌려보내 재번역시킨다
- 리셋: 영역 마이그레이션 완료 시 + **커널 런치마다 전체 리셋** (§14.2)

---

## 4. 이번에 잡은 정합성 버그

재작성 과정에서 실제로 데이터가 깨지던 원인 네 가지를 추적해 고쳤다.

1. **폴트가 퇴출 중인 영역의 phase를 덮어씀** — `scheduleFaultHandlingLocked`가
   `RegionEvicting`을 `RegionFaultPending`으로 바꿔, 퇴출이 진행 중인 영역이
   idle로 보여 두 번 victim이 될 수 있었다.
2. **폴트가 D2H(퇴출) 마이그레이션에 붙음** — `migrationCoveringRegion`이 방향을
   보지 않아, 폴트가 반대 방향 전송에 attach되어 영원히 완료되지 않았다.
3. **Victim 재확인 누락** — victim은 미리 선택되지만 비동기로 하나씩 퇴출되므로,
   차례가 오기 전에 admission 대상이 될 수 있다. `beginEviction`에서 재확인한다.
4. **Lost update (실측 확인)** — 드라이버가 거부한 remote write가 PCIe로 비동기
   수행되어 **H2D 스냅샷 이후**에 호스트 메모리에 도착하면, GPU 사본이 그 store를
   놓치고 이어지는 퇴출이 낡은 GPU 프레임을 올바른 호스트 데이터 위에 덮어썼다.
   → admission 전에 `UVMRemoteDrainReq`로 해당 영역의 미결 remote access가
   없음을 확인하도록 수정.

퇴출 순서도 바꿨다. 스펙 §19는 `캐시 WB+INV → TLB 무효화` 순서를 제시하지만 그
안전성은 §19.1의 "캐시가 새 트랜잭션을 막는다"는 가정에 의존한다. 본 모델은 같은
보장을 번역 쪽에서 얻는다:

```text
PTE park → 64KB TLB 무효화 → 모든 AT에 region drain → 64KB 캐시 WB+INV → D2H
```

L1V는 write-around이라 L2의 `WriteDoneRsp`를 받은 뒤에야 AT에 응답하므로,
AT drain 완료는 "모든 store가 L2에 커밋됨"을 뜻한다.

---

## 5. 플래그

```text
-uvm                                  # UVM managed memory
-uvm-ideal                            # 폴트/전송/제어 레이턴시 0, 카운터는 동일
-uvm-fault-latency-us=20
-uvm-access-counter=true              # false면 cold page = INVALID (순수 demand paging)
-uvm-access-counter-threshold=8
-uvm-tbn-expand-ratio=0.51
-uvm-tbn-max-fetch-size=2097152
-uvm-gpu-memory-capacity=<bytes>      # 절대 용량
-uvm-gpu-memory-capacity-ratio=<f>    # GPU DRAM 대비 비율
-uvm-oversubscription-ratio=<f>       # managed 할당 총량 대비 비율 (신규)
-uvm-disable-prefetch / -uvm-disable-eviction
```

### 용량 산정: 측정된 working set 우선

`scripts/3_gen_runners.py`는 `results/metrics.csv`의 `working_set_bytes`(= L1 TLB가
관측한 **실제 touch한 distinct 페이지**)를 읽어 벤치마크별 절대 용량을 계산한다:

```text
capacity = working_set_bytes / ratio   (64KB 내림)
```

할당량이 아니라 측정값을 써야 하는 이유는 둘이 크게 다를 수 있기 때문이다:

| benchmark | working set | 할당 footprint | 비고 |
|---|---:|---:|---|
| matrixtranspose | 33,566,720 | 33,566,720 | 동일 |
| vectoradd | 50,339,840 | 50,343,936 | 동일 |
| **nbody** | **2,633,728** | **33,566,720** | **12.7배 차이** |

nbody는 32MB를 할당하지만 2.6MB만 건드린다. 할당 기준으로 용량을 잡으면 22MB가
되어 전부 상주해버리고 **oversubscription이 전혀 일어나지 않는다**. 측정값 기준이면
1.7MB가 되어 실제로 150%가 된다.

측정값이 없는 벤치마크는 아래 `-uvm-oversubscription-ratio`로 폴백하며, 생성기가
어떤 벤치마크가 baseline 실행을 필요로 하는지 출력한다.

**부트스트랩 순서**: baseline config 실행 → `5_collect_metrics.py` →
`3_gen_runners.py` 재실행.

### `-uvm-oversubscription-ratio` (폴백)

```text
ratio = (AllocateManaged 할당 총량) / (UVM GPU capacity)
```

**분자**는 벤치마크가 `AllocateManaged`로 요청한 바이트를 4KB로 올림해 누적한
값이다 — 커널이 실제로 touch하는 페이지 집합(true working set)이 아니라 **할당
footprint**다. 스펙 §20은 이를 "Managed Working-Set Size"라 부르지만 구현이 재는
것은 할당량이므로 이 문서에서는 할당 footprint로 표기한다.

**분모**는 UVM capacity 예산이며 GPU 물리 메모리(4GB)는 식에 등장하지 않는다.
GPU DRAM 대비 비율이 필요하면 `-uvm-gpu-memory-capacity-ratio`가 그 역할을 한다.

| 플래그 | 계산식 |
|---|---|
| `-uvm-gpu-memory-capacity=N` | capacity = N |
| `-uvm-gpu-memory-capacity-ratio=r` | capacity = **GPU DRAM(4GB)** x r |
| `-uvm-oversubscription-ratio=r` | capacity = **managed 할당 총량** / r |

이 capacity는 UVM 드라이버 내부의 **소프트웨어 예산**이다. GPU DRAM 크기,
DRAM 컨트롤러, 주소 매핑은 전혀 바뀌지 않으며, 물리 프레임 할당자도 여전히 4GB
전체를 줄 수 있다. UVM이 managed 페이지에 대해서만 스스로 상한을 지키고 초과 시
퇴출한다 (스펙 §20: "must not assume all configured HBM is available to managed
memory"). 비-managed 할당(커널 코드 오브젝트, 커널 인자 버퍼 등)은 이 예산에
포함되지 않는다.

관리형 할당이 등록될 때마다 용량을 다시 계산하고 64KB로 내림한다. 벤치마크마다
할당량이 달라도 **같은 비율**이 보장되므로 벤치마크별 용량을 손으로 계산해 넣을
필요가 없다.

검증 예 (`matrixtranspose -width=256`, managed 할당 524,288 B):

```text
-uvm-oversubscription-ratio=1.5
  -> capacity 327,680 B (= 150% oversubscription)
  -> uvm_gpu_resident_bytes_peak 327,680  (용량과 정확히 일치)
  -> uvm_num_evictions 5, uvm_num_pre_evictions 1
  -> -verify 통과
```

---

## 6. 실행

```bash
# 스크립트 구성 생성 (uvm-oversub-150 포함)
cd scripts && ./3_gen_runners.py

# 개별 실행 예
cd mgpusim/amd/samples/matrixtranspose && go build
./matrixtranspose -timing -parallel -arch=gcn3 -report-all -disable-rtm \
    -uvm -uvm-ideal -width=2048 -verify
```

통계는 실행 종료 시 SQLite `mgpusim_metrics` 테이블에 `Location="UVM"`으로
기록된다 (스펙 §27 카운터 전체).

---

## 7. 검증 결과

| 구성 | 벤치마크 | 결과 |
|---|---|---|
| `-uvm -uvm-ideal` | matrixtranspose 256 / 512 / **2048** | Passed |
| `-uvm` (normal) | matrixtranspose 256 / 512 / **2048** | Passed |
| `-uvm -uvm-ideal` | kmeans 8192pt x2 iter | Passed |
| `-uvm` (normal) | kmeans 8192pt x2 iter | Passed |
| `-uvm -uvm-access-counter=false` | matrixtranspose 256 | Passed |
| `-uvm=false` (회귀) | matrixtranspose 512 | Passed |
| `-uvm-oversubscription-ratio=1.5` | matrixtranspose 256 (serial) | Passed |

단위 테스트: 드라이버 35/35, akita `mem/vm` · `mem/cache` 전부 통과.
`golangci-lint run ./amd/...`: 신규 지적 없음 (남은 11건은 모두 기존 이슈).

관측된 동작 예 (`matrixtranspose -width=512 -uvm`):
raw 폴트 91 → 유니크 64KB 서비스 20 + coalesced 71,
폴트 시간 = 20 x 20us = 400us, 마이그레이션 시간 0.576ms (실제 DMA/PCIe),
remote write 1,256건 stall 후 전부 replay, range TLB 무효화 33건,
전역 shootdown 0건.

---

## 8. 알려진 한계

1. **Oversubscription이 `-parallel` 대규모에서 진행되지 않는다.**
   용량을 managed 할당 총량 아래로 제한한 경우 serial 실행은 matrixtranspose 128 / 256에서
   정확히 통과하지만, `-parallel`은 128만 통과하고 256 이상에서 시뮬레이션이
   더 진행되지 않는다(stall). **데이터 오류가 아니라 미완료**다. 용량 소진 시
   admission을 거부하는 경로의 liveness 문제로 보이며 남은 작업이다.
   → `scripts/benchmarks/uvm-oversub-150/` 실행 시 이 제약을 먼저 확인할 것.
2. **단일 GPU만 지원** (플래그 검증으로 거부).
3. **퇴출 copy-back은 보수적** — 더티 페이지 추적 미구현이라 모든 퇴출이 D2H
   트래픽을 발생시킨다 (스펙 §18.3에서 허용).
4. **Remote atomic 미모델링** — 현재 메모리 프로토콜이 remote atomic을 표현하지
   않아 일반 경로를 탄다 (스펙 §15.1은 명시적 거부를 요구).
5. **TBN occupancy를 비트마스크가 아닌 페이지 순회로 계산** — 의미는 동일하며
   스펙 §11.11이 허용하는 최적화 여지.
6. **TBN 조상 확장은 §11.4 의사코드를 따른다** — 아래에서 위로 훑되 첫 실패에서
   중단한다. 따라서 §11.6 예시(256KB 75%)에 도달하려면 그 아래 128KB 노드가
   먼저 임계값을 넘어야 한다.
