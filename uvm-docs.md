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

1. **(해결됨) Oversubscription stall.** 원인과 수정은 §9를 본다. 현재
   `-parallel`/serial 모두 matrixtranspose 256 / 512 / 1024 / 2048에서
   `-uvm-oversubscription-ratio=1.5`로 통과한다. 남은 코너 케이스는 아래 7번이다.
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
7. **극단 churn 코너 케이스는 §10 이후 관측되지 않는다.**
   `-uvm-oversubscription-ratio=4` + `-uvm-access-counter-threshold=1` 구성은
   §9 시점까지 간헐적 데이터 불일치가 남아 있었다(parallel 3회 중 1회, serial
   재현적). §10에서 access-counter migration을 fault-service 슬롯에 태워
   직렬화한 뒤로는 parallel 3/3, serial 모두 통과한다. 동시 admission이
   사라져 **도달 불가능해진 것**일 수 있으므로 근본 원인이 제거되었다고
   단정하지는 않는다.

---

## 9. Oversubscription stall — 원인과 수정

`-uvm-oversubscription-ratio`로 용량을 managed 할당 총량 아래로 내리면
시뮬레이션이 멈추던 문제다. livelock이 아니라 **완전한 deadlock**이었다:
엔진 이벤트가 고갈되고 CPU 사용률이 0이 되며 커널은 끝나지 않는다.

### 9.1 재현

```bash
./matrixtranspose -timing -parallel -gpu=r9nano -arch=gcn3 -report-all \
    -disable-rtm -uvm -verify \
    -uvm-oversubscription-ratio=4 -uvm-access-counter-threshold=1 -width=512
```

`-parallel` 전용 문제는 아니다. 동시 in-flight remote write가 많을수록 경합
창이 넓어져 확률이 1에 가까워질 뿐이며, 워킹셋이 커지면 serial에서도 같은
순환에 빠질 수 있다.

### 9.2 순환 (계측으로 확인)

```text
region R 퇴출 ──필요──> CP 캐시 range WB+INV
        └──필요──> 모든 AddressTranslator의 R 구간 drain
                └──대기──> AT가 bottom으로 내보낸 remote WriteReq 1건
                        └──보관──> accesscounter.stalledWrites[R]
                                └──필요──> R에 대한 driver의 응답(replay)
                                        └──없음──> R이 RegionEvicting이라
                                                   notification이 그냥 버려짐
```

계측 로그가 이 고리를 그대로 보여줬다.

```text
[drvdbg] SUPPRESS-BUSY region=0x100000 phase=5(RegionEvicting)
[cpdbg]  FLUSH-START  region=0x100000     <- FLUSH-ISSUE 는 끝내 오지 않음
[atdbg]  DRAIN-BLOCKED region=0x100000 by=*mem.WriteReq addr=0x109500
```

한 번 막히면 CP의 `ToUVMDriver`가 head-of-line 상태가 되어 뒤따르는 모든 UVM
제어 메시지(다른 퇴출의 TLB 무효화, replay, drain)가 함께 멎는다.

### 9.3 수정

| # | 위치 | 내용 |
|---|---|---|
| 1 | `akita/mem/vm/addresstranslator` | region drain이 **remote로 라우팅된 요청을 세지 않는다.** drain의 목적은 "이 구간의 store가 캐시에 도달했는가"인데 remote access는 캐시에 들어가지 않는다. 이것이 순환을 끊는다 |
| 2 | `driver/uvm_eviction.go` | `finalizeEviction`이 해당 region의 access counter를 **재무장(reset)** 한다. 퇴출은 notification을 소비하면서 replay는 하지 않는 유일한 트랜잭션이므로, 소비한 만큼 되돌려 준다 |
| 3 | `timing/accesscounter/counter.go` | reset 시 그 region에 보류 write가 남아 있으면 notification을 **즉시 다시 올린다.** 이전에는 조용히 버려졌다 |
| 4 | `driver/uvm_regions.go` | 모든 notification에 답한다. region이 없으면 refused, 이미 GPU-local이면 replay. 다른 트랜잭션이 replay를 책임질 때만 삼킨다 |
| 5 | `akita/mem/vm/gmmu` | replay는 범위를 지정할 뿐 요청을 지정하지 않으므로, `resumeTranslation`이 매핑을 **다시 확인**하고 아직 park 상태면 재-fault시킨다. 같은 replay 메시지가 그 트랜잭션을 다시 집지 않도록 `refaultedBy`로 표시한다(표시가 없으면 단일 replay 하나가 무한 루프를 돈다) |

### 9.4 함께 드러난 기존 데이터 버그 두 건

stall을 없애자 그 뒤에 가려져 있던 결함이 드러났다. 둘 다 §9.3 이전부터
존재했고(용량 고갈 구성에서 `-parallel` 없이도 재현) 함께 고쳤다.

1. **in-flight admission을 "거부"로 답하던 문제** (`driver/uvm_migration.go`).
   `newMigration`은 이미 마이그레이션 중인 페이지를 집지 않는다. 따라서 용량을
   기다리는 동안 다른 admission이 같은 페이지를 가져가면 빈 마이그레이션이 되고,
   `deferAdmission`이 **region 전체를 refused로 답했다.** 그러면 access counter가
   보류 중이던 write들을 host 메모리로 흘리는데 그 host 메모리는 진행 중인 H2D가
   이미 읽어간 뒤다 → GPU 사본이 authoritative가 되면서 그 write들이 통째로
   사라진다(`get 0`). 이제 덮고 있는 admission이 있으면 **거부 대신 그
   마이그레이션에 합류**한다.
2. **UVM 마이그레이션 DMA 응답을 tick 순서에 의존해 클레임하던 문제**
   (`driver/uvm_driver.go`, `driver/memorycopy.go`). 드라이버가 사용자 copy
   미들웨어보다 먼저 응답을 가로채는 방식이었는데, 미들웨어가 포트를 다시 peek할
   때 head가 그 사이 도착한 마이그레이션 응답일 수 있다. 그러면 사용자 경로가
   그것을 소비하고 `findCommandByReq`가 `panic: cannot find command`로 죽는다.
   이제 `Driver.ClaimUVMDMAReturn`으로 **소유권이 소비자를 결정**한다.

### 9.5 검증

`-timing -parallel -gpu=r9nano -arch=gcn3 -report-all -disable-rtm -uvm -verify`

| 구성 | 결과 |
|---|---|
| `-width=512` (oversubscription 없음) | Passed |
| `-uvm-oversubscription-ratio=1.5` 256 / 512 / 1024 / **2048** | Passed |
| `-uvm-oversubscription-ratio=2` / `2.5` / `4` / `8` (512) | Passed |
| `-uvm-oversubscription-ratio=4` (256) | Passed |
| `-uvm-disable-eviction -uvm-oversubscription-ratio=1.5` | Passed (이전에는 데이터 불일치) |
| `-uvm-access-counter=false -uvm-oversubscription-ratio=4` | Passed |
| `-uvm-disable-prefetch` / `-uvm-ideal` / `-uvm=false` | Passed |

수정 전 같은 조건: ratio 4 + threshold 1은 **정지**, ratio 2.5 / 4 / 8은
`panic: cannot find command`, `-uvm-disable-eviction`은 데이터 불일치,
ratio 1.5 @ 1024는 통과(2048은 미측정).

단위 테스트와 `golangci-lint run ./amd/...`에 신규 지적 없음(기존 실패인
`idealmemcontroller` 컴파일, `dispatching` stdout/stderr, lint 11건은 그대로).

---

## 10. Access-counter migration에 fixed fault latency 부과

### 10.1 이전 동작

20us fixed software latency(§10.1)는 `scheduleFaultHandlingLocked` 한 곳에서만
부과되고, 그 호출처는 demand fault 큐(`startNextFaultServiceLocked`)뿐이었다.
access-counter 경로는 `onAccessCounterNotify → migrateRegionLocked →
startCPUToGPUMigration`으로 곧장 DMA에 도달해 **software latency가 0**이었다.

스펙 문자 그대로는 위반이 아니다. §10.1의 과금 규칙은 "unique **fault-service**
transaction 당 1회"이고 §16의 access-counter 흐름에는 software latency 단계가
없다. 다만 결과적으로:

- 드라이버가 하는 일(capacity 검사 → 퇴출 → DMA → PTE → replay)은 같은데
  페이지가 REMOTE 매핑이었는지에 따라 20us와 0us로 갈렸다.
- §15의 "remote write → immediate migration"이 access-counter 경로를 타므로
  write-triggered migration도 0us였다. 같은 write가 cold/INVALID 페이지에서는
  demand fault를 타 20us였다.
- access-counter migration은 `faultServiceCue`를 우회하므로 §8.4의 "동시에
  하나의 fault service만 활성" 직렬화도 받지 않았다.

실측(matrixtranspose 2048, ratio 1.5): CPU→GPU migration 8,084건 중 7,747건
(96%)이 무과금이었다.

### 10.2 변경

access-counter notification을 **demand fault와 같은 service 큐**에 태운다.

```text
AccessCounterNotifyReq
    |
    v
admitAccessCounterServiceLocked   (region이 이미 트랜잭션 보유 시 §16대로 swallow)
    |
    v
faultServiceCue  (FIFO, 활성 1개 — §8.4)
    |
    v
+20us fixed software latency      (§10.1)
    |
    v
serviceFaultLocked                (demand fault와 동일 — TBN 포함)
```

**access-counter migration도 page fault다.** 따라서 서비스 경로 자체를 하나로
합쳤다 — 큐, 20us, §8.4 직렬화, `serviceFaultLocked`, TBN까지 동일하다.
`FaultTransaction.Trigger`는 통계 귀속(demand / access counter)만 구분한다.

TBN 적용에는 demand mask가 필요한데 counter는 4KB 단위를 모른다(§14의 카운터가
64KB 단위). 그래서 access-counter 서비스의 demand mask는 **notification이 온
64KB region 전체**로 둔다(`regionDemandMask`). 비워 두면 counter가 명시적으로
요청한 region이 prefetch로 집계되어 prefetch 정확도 지표가 무의미해진다.

pending 상태의 access-counter 트랜잭션에 실제 page fault가 coalesce되면
demand fault로 **승격**한다(`promoteToDemandFaultLocked`). 실제 fault는 §11.7이
요구하는 4KB 단위 demand mask를 표현할 수 있으므로, 승격 시 counter가 심어 둔
64KB seed를 버리고 실제 fault 페이지부터 다시 쌓는다. §10.1의 "region당 1회
과금"은 그대로 유지된다(버킷만 이동).

신규 카운터 `uvm_num_access_counter_services` = 20us를 부과받은 AC 서비스 수.
`uvm_fault_service_latency_total`은 두 종류의 합계다.

### 10.3 영향 (실측)

matrixtranspose, `-uvm-oversubscription-ratio=1.5`, `-parallel`:

| | width=512 전 | width=512 후 | width=1024 전 | width=1024 후 |
|---|---|---|---|---|
| `Driver.kernel_time` | 0.145 ms | 0.889 ms (**6.1x**) | 3.580 ms | 3.497 ms (**0.98x**) |
| `fault_service_latency_total` | 0.26 ms | 1.00 ms | 2.28 ms | 3.88 ms |
| AC services (20us 과금) | 0 | 50 | 0 | 194 |
| CPU→GPU migrations | 41 | 33 | 416 | 129 |
| evictions | 34 | 13 | 502 | 44 |
| migrated bytes | 5.63 MB | 2.95 MB | 70.8 MB | 11.3 MB |
| remote accesses | — | — | 19,700 | 64,896 |

두 가지를 유의한다.

1. **커널이 짧을수록 타격이 크다.** 512는 커널 0.145ms에 직렬화된 1.0ms가
   얹혀 6.1x가 되지만, 1024는 커널 3.5ms에 3.88ms가 겹쳐 들어가 사실상
   동일하다. 절대 지연이 아니라 커널 길이 대비 비율이 지배한다.
2. **정책이 migration에서 remote access로 이동한다.** admission 1건이
   20us를 직렬로 소모하므로 migration이 비싸지고(1024에서 416 → 129),
   thrashing이 크게 줄며(eviction 502 → 44) 그만큼 remote access가 늘어난다
   (19,700 → 64,896). 20us를 실제로 모델링하면 나오는 당연한 귀결이지만
   질적으로 다른 동작이므로 이전 측정치와 직접 비교하면 안 된다.

### 10.4 검증

`-timing -parallel -gpu=r9nano -arch=gcn3 -report-all -disable-rtm -uvm -verify`
전부 Passed: oversubscription 없음 / ratio 1.5 (256·512·1024) / ratio 2·2.5·4·8
(512) / ratio 4 (256) / ratio 4 + threshold 1 (512, parallel 3/3 및 serial) /
`-uvm-disable-eviction` / `-uvm-access-counter=false` / `-uvm-disable-prefetch` /
`-uvm-ideal` / `-uvm=false`.

드라이버 유닛 테스트 35/35 통과. lint 신규 지적 없음.

### 10.5 TBN을 access-counter migration에도 적용

§10.2 초기 구현은 access-counter 서비스를 64KB 고정으로 두고 TBN을 태우지
않았다(§11이 fault 기준으로 서술되어 있고 §16 흐름에 TBN 단계가 없다는 이유).
그러나 access-counter로 인한 migration도 page fault이므로 TBN을 **무조건**
적용하도록 통합했다. 위 §10.2의 서술이 최종 형태다.

실측 (`-uvm-oversubscription-ratio=1.5`, `-parallel`):

| | 512 미적용 | 512 적용 | 1024 미적용 | 1024 적용 |
|---|---|---|---|---|
| TBN fault events | 0 | 49 | 0 | 167 |
| 64KB 선택 | 0 | 19 | 0 | 50 |
| 2MB 확장 | 0 | 10 | 0 | 87 |
| `tbn_demand_bytes` | 0 | 3.08 MB | 0 | 10.8 MB |
| `tbn_actual_prefetch_dma_bytes` | 0 | 7.00 MB | 0 | 3.47 MB |
| CPU→GPU migrations | 33 | 42 | 129 | **77** |
| evictions | 13 | 121 | 44 | 45 |
| migrated bytes | 2.95 MB | 16.9 MB | 11.27 MB | 11.33 MB |
| `fault_service_latency_total` | 1.00 ms | 0.98 ms | 3.88 ms | **3.34 ms** |
| `Driver.kernel_time` | 0.889 ms | 1.091 ms (1.23x) | 3.497 ms | 3.463 ms (0.99x) |

1024에서는 **같은 양의 데이터가 더 적고 큰 전송으로** 옮겨진다(migration
129 → 77, migrated bytes 11.27 → 11.33 MB). 서비스 횟수가 줄어 software
latency도 3.88 → 3.34 ms로 감소한다. 512는 워킹셋이 작아 2MB 확장이 과잉
prefetch가 되어 migrated bytes가 5.8배로 늘고 kernel time이 1.23배가 된다.

`-uvm-disable-prefetch`는 access-counter 서비스에도 그대로 적용된다(확인:
TBN events 50건 전부 64KB, prefetch DMA 0 B).

### 10.6 검증 (최종)

`-timing -parallel -gpu=r9nano -arch=gcn3 -report-all -disable-rtm -uvm -verify`
전부 Passed: oversubscription 없음 / ratio 1.5 (256·512·1024) / ratio 2·4·8
(512) / ratio 4 (256) / ratio 4 + threshold 1 (512) / `-uvm-disable-eviction` /
`-uvm-access-counter=false` / `-uvm-disable-prefetch` / `-uvm-ideal` /
`-uvm=false`.

드라이버 유닛 테스트 35/35, `ginkgo -r --skip-package=mccl` 신규 실패 없음
(기존 `dispatching` stdout 이슈만), lint 신규 지적 없음(기존 11건).
