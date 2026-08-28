# VirtualDRAM 시뮬레이션 실행 지시서

이 문서는 Accel-Sim/GPGPU-Sim 기반 VirtualDRAM 평가 실험의 실행 명세다. Phase 0부터 순서대로 진행하며, 각 Phase의 성공 기준을 만족하지 못하면 다음 Phase로 넘어가지 말 것.

---

## 0. 아키텍처 확정안 요약

기존 설계에서 다음이 변경되었다. 시뮬레이터 구현은 이 확정안을 따른다.

**변경 전 (논문 초안)**
- Hash zone이 direct-mapped
- CAMA = SABA(bin 예산 배분) + NCMA(비연속 VA 할당 + `ptrs[]` 인다이렉션 + PTX rewriting)

**변경 후 (확정안)**
- Hash zone이 **2-way set associative**. 두 way는 **동일 DRAM row**에 배치
- NCMA 제거. VA는 애플리케이션 관점에서 **완전히 연속**
- SABA는 bin 예산 배분이 아니라 **셋 내 victim 선택용 hot/cold 힌트 1비트**로 축소
- 셋 인덱스는 상태 없는 순수 산술: `set = VPN mod S` (S = 파티션 내 셋 개수)

**중요 — 채널 인터리빙 관련 정정**

GPU는 cacheline 단위로 memory partition을 인터리빙한다. 요청이 memory controller에 도착한 시점에는 파티션이 이미 VA에 의해 결정되어 있다. 따라서 해시 함수는 **파티션 간 배치를 결정할 수 없고, 파티션 내부 셋 인덱스만 결정한다**. 논문 초안의 `PFN = VPN mod N`은 이 구분이 없어 잘못된 표현이며, 시뮬레이터는 다음과 같이 구현해야 한다.

```
partition = <기존 VA 인터리빙 그대로>   // 변경 없음
set_index = VPN mod S                   // 파티션 내부
way       = ECC 태그 검증으로 결정 (way 0 → way 1 순차 프로브)
```

**접근 경로**

```
VA 요청 → VIVT L1/L2 → (L2 miss) → memory partition
  → set_index 계산
  → way 0 ECC 태그 검증 → 일치하면 완료
  → way 1 ECC 태그 검증 → 일치하면 완료 (같은 row, column access만 추가)
  → 둘 다 실패 → TLB/page walk → Translation Zone 또는 page fault
```

---

## 1. Phase 0 — 선결 과제 (최우선)

아래 두 항목을 해결하기 전에는 A1~A5를 시작하지 말 것. 여기서 문제가 발견되면 이후 모든 결과를 다시 돌려야 한다.

### 0-1. Baseline DRAM 대역폭 수치 검증

논문 초안 Figure 12는 baseline 2.1 GB/s, VirtualDRAM 73.7 GB/s로 보고하고 있다. Table 2의 HBM3 구성은 3.35 TB/s이므로 각각 **활용률 0.06%와 2.2%**다. 어떤 GPU 워크로드에서도 나올 수 없는 값이며, 설정 또는 집계에 오류가 있다는 신호다.

확인할 것:

1. 대역폭 집계가 **전체 80채널**을 합산하는지, 채널 1개만 세고 있는지
2. 집계 단위가 GB/s가 맞는지 (사이클당 바이트를 잘못 환산했을 가능성)
3. 워크로드 입력 크기가 지나치게 작아 커널 실행 시간 대부분이 launch overhead인지
4. `-gpgpu_frfcfs_dram_sched_queue_size` 등 DRAM 큐 설정이 비정상적으로 작은지

검증 방법: STREAM 계열 또는 단순 memcpy 커널을 돌려 이론 대역폭의 최소 60% 이상이 나오는지 확인한다. 여기서 안 나오면 시뮬레이터 설정 문제가 확정이다.

**성공 기준**: 대역폭 포화 커널에서 이론치의 60% 이상 달성.

### 0-2. Baseline 정확도 검증

Figure 3은 RTX 3090 실측인데 시뮬레이션은 A100급 코어 + HBM3 구성이다. 구성이 달라 직접 비교가 성립하지 않는다.

- 실제 하드웨어와 시뮬레이터 IPC의 상관계수를 워크로드별로 측정하여 보고
- Methodology에 워크로드별 **입력 크기와 footprint**를 명시할 수 있도록 수집
- 시뮬레이션 구성과 실측 구성이 다른 이유를 문서화 (또는 구성을 통일)

**성공 기준**: 상관계수 보고 가능. 이상치 워크로드는 원인 규명 또는 제외 사유 명시.

---

## 2. 시뮬레이터 구현 요구사항

A1~A5를 돌리기 전에 아래가 모두 구현/확인되어야 한다.

### 2-1. Way 수 파라미터화 (필수)

way 수가 하드코딩되어 있으면 A2 전체가 불가능하다. 셋 인덱스 계산과 way 프로브를 분리하고 way 수를 config 파라미터로 노출할 것. `way = 1`은 direct-mapped와 동일해야 하며, `way = full`(fully-associative)도 지원해야 한다. fully-associative는 성능 상한 기준선으로만 쓰므로 타이밍 정확도는 중요하지 않고 hit rate만 정확하면 된다.

### 2-2. 동일 row 타이밍 모델 (필수)

way 0과 way 1은 같은 DRAM row에 있으므로, way 0 실패 후 way 1 프로브는 **row activation 없이 column access만** 발생해야 한다. 이를 모델링하지 않으면 way 1 프로브가 full DRAM access로 계산되어 associativity가 실제보다 비싸게 나오고, A2 결과가 통째로 왜곡된다.

구현 확인 방법: way 0 miss / way 1 hit인 요청의 평균 지연이 way 0 hit 대비 tCCD 수준(수 사이클)만 증가하는지 로그로 확인.

### 2-3. HBM 용량 스케일링 (필수)

W/M sweep은 **워크로드 입력이 아니라 HBM 용량을 바꿔서** 수행한다. 입력을 바꾸면 접근 패턴 자체가 달라져 hit rate 변화의 원인을 분리할 수 없다.

**주의**: 용량을 줄일 때 **채널 수는 80으로 고정**하고 채널당 용량만 줄일 것. 채널을 같이 줄이면 대역폭이 변해 sweep이 오염된다.

### 2-4. 통계 수집 항목

아래를 워크로드별로 수집한다.

**필수**
- L2 miss 수 (= DRAM-bound 요청 수, 모든 hit rate의 분모)
- way 0 hit / way 1 hit / Translation Zone hit / page fault 각각의 **접근 횟수** (페이지 수 아님)
- IPC
- DRAM 대역폭 (전체 채널 합산)
- 커널 시작 후 시간축 hit rate (compulsory miss 분리용, 최소 10구간 버킷)
- Translation Zone에 들어간 페이지의 재접근 비율 (TZ 자체 hit rate)

**가능하면**
- Row buffer hit rate
- Bank-level parallelism (동시 활성 bank 수 평균)

Row buffer hit는 GPGPU-Sim `dram.cc`에 카운터가 있으나 bank 단위 집계는 기본 출력이 아닐 수 있다. **A2 시작 전에 계측 가능 여부를 먼저 확인하고 보고할 것.** 불가능하면 계측 코드를 추가하거나, 최소한 row buffer hit rate만이라도 확보한다.

---

## 3. 실험 Knob

| Knob | 값 | 비고 |
|---|---|---|
| `W/M` (오버서브 비율) | 0.5 / 0.75 / 0.9 / 1.0 / 1.25 / 1.5 / 2.0 | HBM 용량으로 조절. 분모는 **전체 M**, hash zone 용량 아님 |
| `ways` | 1 / 2 / 4 / 8 / full | |
| `T` (Translation Zone 비율) | 0 / 5 / 10 / 20 / 30 % | |
| `hint` (SABA hot/cold) | on / off | |

**정의 고정 사항**

- Hash zone hit rate의 **분모는 L2 miss**다. 전체 메모리 요청이 아니다. 전체 요청 기준으로 재면 VIVT L2가 걸러준 만큼 인위적으로 높게 나오고, 워크로드 간 L2 hit rate 차이 때문에 비교도 불가능해진다.
- 모든 hit rate는 **접근 횟수 가중**으로 계산한다. 페이지 수 기준으로 재지 말 것.
- `W/M`은 항상 **전체 HBM 용량 M** 기준이다. `(1-T)·M` 기준으로 정의하면 T를 바꿀 때 x축이 함께 움직여 T sweep과 W sweep을 겹쳐볼 수 없다.

---

## 4. 실험 명세

### A1. Hash zone hit rate 분해

**목적**: 논문 전체의 근간. 성능은 hit rate의 함수이므로 이것이 낮으면 나머지 결과가 무의미하다.

**구성**: `ways = 2`, `T = 20%`, `hint = on` 고정. W/M을 7개 값으로 sweep. 전체 31개 워크로드.

**출력**: 워크로드별 stacked bar. 구간은 way 0 hit / way 1 hit / TZ hit / page fault 4개. W/M 값마다 별도 그림 또는 대표 3개 값(0.5 / 1.0 / 1.5).

**추가 기준선**: 동일 용량의 **fully-associative hash zone** 결과를 점선으로 중첩. 두 곡선의 간격이 "배치 정책이 더 벌 수 있는 여지"이며, 간격이 좁으면 복잡한 allocator가 불필요하다는 증거가 된다.

**해석 포인트**
- `W/M = 0.5`는 용량이 충분히 남는 지점이므로 여기서의 miss는 **전부 conflict**다. 즉 이 한 점이 해시 함수와 associativity 품질만을 순수하게 측정한다. 반드시 포함할 것.
- `way 1 hit` 구간의 두께가 곧 associativity의 기여도다.
- `W/M = 0.9`는 함정 구간이다. `T = 20%`면 hash zone은 `0.8M`이므로, W가 0.8M~1.0M 사이일 때 **기존 시스템은 전부 담기는데 VirtualDRAM은 넘친다**. Translation Zone이 용량을 뺏어서 생기는 구조적 손해이며, 리뷰어가 반드시 찾아낸다. 숨기지 말고 명시적으로 측정·보고할 것.

**성공 기준**: `W/M = 0.5`에서 2-way hit rate가 fully-associative 대비 몇 %p 이내인지 수치로 보고. (이 값이 작으면 CAMA 축소 결정이 정당화된다.)

---

### A2. Way 수 sweep

**목적**: 2-way 선택의 정당화. associativity를 늘릴 때의 이득과 DRAM 효율 손실의 교차점을 찾는다.

**구성**: `ways ∈ {1, 2, 4, 8, full}`, `T = 20%`, `hint = on`. `W/M ∈ {0.5, 1.5}` 두 지점에서 각각 수행.

**출력**: x축 = way 수. 좌축에 hash zone hit rate와 IPC, 우축에 row buffer hit rate와 bank-level parallelism을 함께 그린다.

**해석 포인트**: way를 늘리면 hit rate는 오르지만 같은 row 재접근이 늘어 bank 병렬성이 떨어진다. **2-way에서 무릎(knee)이 나오는 것**이 이 그림의 목표다. `W/M = 0.5`(conflict만)와 `1.5`(capacity 지배) 두 지점에서 곡선 모양이 다를 것으로 예상되며, 그 차이 자체가 논지가 된다.

**성공 기준**: 2-way와 4-way의 IPC 차이가 작고, 4-way 이상에서 DRAM 효율 저하가 관측되면 2-way 선택이 데이터로 정당화된다.

---

### A3. Ablation

**목적**: 각 구성 요소의 기여도 분리. 리뷰어가 반드시 요구한다.

**중요**: 이것은 새 시뮬레이션이 아니다. A1·A2·A4·A5 결과에서 아래 조합을 뽑아 쓴다.

| 구성 | ways | T | hint |
|---|---|---|---|
| baseline (translation-before-access) | — | — | — |
| + hash zone | 1 | 0% | off |
| + associativity | 2 | 0% | off |
| + SABA 힌트 | 2 | 0% | on |
| + Translation Zone (full) | 2 | 20% | on |

**출력**: 누적 IPC 막대그래프. `W/M ∈ {0.5, 1.5}` 두 지점.

**해석 포인트**: **SABA 힌트의 기여가 작게 나와도 문제없다.** 오히려 "associativity가 conflict의 대부분을 해결하므로 allocator를 최소화했다"는 논문 서사와 일치한다. 기여도를 측정하지 않고 복잡한 allocator를 유지하는 것이 위험한 상태다.

---

### A4. Oversubscription sweep

**목적**: Translation Zone과 SABA 힌트가 존재하는 유일한 이유가 여기다.

**구성**: A1과 동일한 실행에서 지표만 다르게 뽑는다. `W/M` 7개 값 전체. `hint = on`과 `off`를 모두 수행하여 오버서브 구간에서의 차이를 본다.

**출력**: x축 = W/M, y축 = IPC 및 page fault 횟수. baseline과 VirtualDRAM 비교.

**해석 포인트**: `W/M ≤ 1.0`에서는 어떤 allocator든 충돌이 거의 없으므로 **hint on/off 차이가 0에 가까울 것으로 예상된다**. 이를 숨기지 말고 "비오버서브 구간에서 SABA 기여는 0에 가깝고, 오버서브 구간에서만 의미가 있다"고 정직하게 서술하는 편이 강하다.

---

### A5. Translation Zone 크기 sweep

**목적**: T 값의 정당화. **프레이밍 주의 — "20%가 최적"을 보이려는 실험이 아니다.**

**구성**: `T ∈ {0, 5, 10, 20, 30%}` × `W/M ∈ {0.5, 1.0, 1.5}`. `ways = 2`, `hint = on`.

**출력**: x축 = T, y축 = IPC. W/M별로 3개 곡선.

**해석 포인트 (중요)**

Translation Zone은 **총 용량을 늘려주지 않는다**. 총량은 고정이다. TZ가 하는 일은 conflict victim을 CPU로 내보내는 대신 DRAM에 붙잡아 두는 것, 즉 **page fault(PCIe 2000 사이클)를 page walk(수백 사이클)로 바꾸는 것**뿐이다.

따라서 **TZ 크기는 오버서브 비율이 아니라 conflict 발생률에 비례해야 한다**. 2-way가 conflict를 크게 줄였으므로 최적 T는 20%보다 훨씬 작을 것으로 예상된다(5% 내외 추정). 예상 곡선 모양은 다음과 같다.

- `W/M = 0.5`: TZ는 순수 낭비이므로 **T = 0이 최적**
- `W/M = 1.5`: 어느 정도 T가 이득
- 그 사이에 교차점 존재

`T = 0`을 반드시 포함할 것. TZ가 정말 필요한지 확인하는 지점이다.

**추가 지표**: TZ에 들어간 페이지의 재접근 비율(TZ 자체 hit rate)을 함께 보고한다. 이것이 낮으면 TZ는 낭비된 용량이라는 뜻이며, 그 자체가 T를 줄일 근거가 된다.

**성공 기준**: 최적 T를 데이터로 확정. 20%보다 작게 나오면 "2-way 덕분에 TZ를 작게 가져갈 수 있다"는 서사로 논문에 반영한다.

---

## 5. 실행 계획

### 구성 수

전체 격자(7 × 5 × 5 = 175)를 돌릴 필요 없다. **한 축씩 고정한 십자(cross) 설계**로 충분하다.

| 실험 | 구성 수 |
|---|---|
| W/M sweep (ways=2, T=20%) | 7 |
| way sweep (W/M = 0.5, 1.5) | 10 |
| T sweep (W/M = 0.5, 1.0, 1.5) | 15 |
| hint off (겹치는 지점 일부) | ~5 |
| **워크로드당 합계** | **~30** |

31개 워크로드 × 30 ≈ 930회 실행. 병렬 실행 권장.

### 순서

1. **Phase 0** (0-1, 0-2) — 여기서 막히면 즉시 보고하고 중단
2. **구현 요구사항 2-1 ~ 2-4** 완료 및 검증
3. **A1** (W/M sweep) — 가장 중요. 결과 나오면 중간 보고
4. **A2** (way sweep) — 2-1, 2-2 구현 정확성에 의존
5. **A5** (T sweep)
6. **A4** (hint on/off 추가분)
7. **A3** — 기존 결과에서 추출, 신규 실행 없음

### 산출물

각 실험마다 다음을 남긴다.

- 원시 통계 CSV (워크로드 × 구성 × 지표)
- 재현용 config 파일 및 실행 커맨드
- 그림 생성 스크립트
- 이상치 워크로드 목록과 원인 메모

---

## 6. 보고 시 주의

- 실행 실패나 예상과 다른 결과가 나오면 **임의로 구성을 바꾸지 말고 먼저 보고**할 것. 특히 Phase 0에서 대역폭이 정상화되지 않으면 그 원인 규명이 최우선이다.
- 워크로드별 **footprint와 실측 WSS를 둘 다** 수집한다. 둘이 크게 다른 워크로드(Temporal 타입)는 footprint 기준 오버서브가 심해 보여도 실제로는 여유가 있어, 이 구분 없이는 결과 해석이 어긋난다.
- 커널 시작 직후 hash zone은 비어 있으므로 초반 hit rate가 낮은 것은 정상이다(compulsory miss). 이것이 섞이면 짧은 커널이 부당하게 나쁘게 나오므로 **steady-state 구간을 분리하거나 시간축 곡선을 함께 보고**할 것.
- Accel-Sim 인용이 논문 초안 §4.1에서 `[]`로 비어 있다. 정확한 인용 정보를 확인해 둘 것.