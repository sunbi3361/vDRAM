# VirtualDRAM hash collision 측정 계획

> 작성: 2026-08-27 | 마커: `sbin_claude_vdram` (수정 전 코드는 주석 처리)
> 근거: `refs/pact26-paper415.pdf` §3.3(CAMA) / §3.4(Translation Zone), `refs/vdram-draft.md`

---

## 0. 먼저 짚어야 할 것 — 지금 그냥 켜면 collision은 0이 나온다

이게 실험 설계의 출발점이자, 정하지 않으면 이후 모든 수치가 무의미해지는 지점이다.

현재 드라이버는 VA를 **4KB부터 연속으로** 나눠준다:

- `mgpusim/amd/driver/internal/memoryallocator.go:465` `allocatePages()` —
  `pState.nextVAddr`가 `1<<12`에서 시작해 할당량만큼 단조 증가.
  즉 프로세스의 VPN은 `1, 2, 3, ...`으로 조밀하다.
- GPU DRAM은 4GB (`timingconfig/r9nano/builder.go:111` `dramSize`), 4KB page →
  **N = 1,048,576 bins**.

따라서 `H(VPN) = VPN mod N`은 벤치마크 footprint가 N보다 작은 한 **단사(injective)**
이고, 충돌은 구조적으로 발생할 수 없다. 벤치마크별 footprint는 이미
`memory_footprint_peak_pages` (`report.go:1762`)로 나오므로 바로 확인 가능하다.

논문에서 collision을 만드는 요인은 세 가지이고(§3.3), 각각을 **따로** 재현·측정해야 한다:

| 요인 | 논문 근거 | 시뮬레이터에서 재현하는 법 |
|---|---|---|
| ① allocator가 H를 모른 채 base VA를 고름 | §3.3 첫 번째 문제 | **VA 배치 모델**을 도입 (§1.2) |
| ② fragmentation으로 collision-free 영역 확보 실패 | §3.3 두 번째 문제 | Avatar의 2MB-region 랜덤 배치(`avatar/meta`) 재사용 |
| ③ oversubscription (footprint > N_hz) | §3.3 세 번째 문제, §2.6 | `-uvm-oversubscription-ratio` / Hash Zone 크기 축소 |

**결론: VA 배치 모델(§1.2)과 footprint/용량 비율(§1.3)이 1차 독립변수다.**
이 둘을 실험 축으로 세우면, 논문에서 "collision이 어디서 오는가"를 분해한 그림을
하나 더 만들 수 있다 — 지금 원고에 없는, 있으면 좋은 그림이다.

---

## 1. 실험 설계

### 1.1 무엇을 collision이라 부를지 — 3층으로 분리

한 단어로 뭉뚱그리면 리뷰어 질문에 답하지 못한다. 세 층을 따로 센다.

**C0. 정적 bin 충돌 (allocation-time)** — 타이밍 불필요, 배치 품질만의 함수
- `vdram_alloc_pages` : 해시 존에 배치 시도한 페이지 수
- `vdram_bin_conflict_allocs` : 배치 시점에 bin이 이미 점유되어 있던 횟수
- `vdram_bin_occupancy_max` / `_p99` / `_histogram` : bin당 경쟁 VPN 수 분포
- `vdram_distinct_bins_used` , `vdram_bin_pressure` (= live pages / N_hz)

**C1. 동적 validation 실패 (run-time)** — 실제 fallback을 유발한 접근
- `vdram_validations` : DRAM 경계에서 tag 검증한 접근 수
- `vdram_validation_fails` : tag mismatch → safe path로 우회한 접근 수
- `vdram_validation_fail_rate`, `vdram_fails_per_kilo_inst` (MPKI와 대응)
- `vdram_hot_bin_thrash` : 같은 bin에서 점유 VPN이 교체된 횟수 (ping-pong 지표)

**C2. collision이 유발한 fault / migration** — 성능 비용
- `vdram_collision_faults` : validation 실패가 페이지 마이그레이션까지 간 횟수
- `vdram_capacity_faults` : 순수 용량 부족(oversubscription)으로 난 fault
- `vdram_tz_hits` / `vdram_tz_misses` : Translation Zone에서 흡수했는지
- `vdram_hz_evictions` (Hash Zone → TZ), `vdram_tz_evictions` (TZ → CPU)
- `vdram_collision_migrated_bytes`, `vdram_collision_stall_time`

> **C2에서 collision-fault와 capacity-fault를 반드시 분리한다.** "restrictive mapping
> 때문에 fault가 늘었다"가 논문의 주장이므로, 오버서브스크립션이면 어차피 났을 fault와
> 매핑 제약 때문에 새로 생긴 fault를 구분하지 못하면 주장이 서지 않는다.
> 판정 기준: **fault 시점에 해당 VPN의 bin이 비어 있었으면 capacity, 다른 VPN이
> 점유 중이었으면 collision.**

### 1.2 독립변수 A — VA 배치 모델

`allocatePages()`의 VA 선택을 훅 하나로 갈아끼운다 (`-vdram-va-layout=`).

| 모델 | 내용 | 역할 |
|---|---|---|
| `contig` | 현재 stock 동작 (4KB부터 연속) | collision 하한 / sanity |
| `cuda-like` | allocation마다 base를 큰 정렬 경계(2MB 또는 64MB)에 맞추고 그 사이를 비움 | 논문이 말하는 "H를 모르는 allocator"의 현실적 근사 — **기본값 권장** |
| `random` | base를 49-bit VA 공간에서 고정 시드로 무작위 선택 | 최악 케이스 |
| `ncma` / `saba` | 제안 기법 (§2.4) | 개선 효과 |

`cuda-like`가 핵심이다. 실제 `cudaMalloc`은 큰 정렬 경계에서 base를 잡기 때문에
서로 다른 allocation의 VPN 하위 비트가 겹치고, 이게 `mod N`에서 그대로 충돌이 된다.
정렬 경계 크기 자체도 스윕 대상(`-vdram-va-align`)으로 둘 만하다.

### 1.3 독립변수 B — footprint / Hash Zone 용량 비율

- Hash Zone bin 수 `N_hz = (1 - tz_ratio) × GPU capacity / pageSize`
- 스윕: `footprint / N_hz` = 0.5 / 0.9 / 1.0 / 1.25 / 1.5 / 2.0
- 기존 `-uvm-oversubscription-ratio`가 이미 "벤치마크 자기 footprint 기준 상대값"으로
  동작하므로(`3_gen_runners.py:143 oversub_capacity`) 그대로 재사용한다.

> **교란 주의:** TZ 비율을 바꾸면 `N_hz`가 바뀌고 **해시 함수 자체가 바뀐다.**
> TZ sweep과 oversub sweep을 교차시킬 때 이 점을 반드시 캡션에 적어야 한다.
> 대안으로 `N_hz`를 고정(2의 거듭제곱)하고 TZ를 그 바깥에서 잘라내는 변형도 있는데,
> 이쪽이 실험 해석은 깨끗하다. 어느 쪽을 쓸지 P1에서 확정할 것.

### 1.4 config ladder — baseline에서 제안까지

각 단계는 **직전 단계 대비 어떤 카운터가 얼마나 줄어야 하는지**를 미리 적어두고,
안 맞으면 결과가 아니라 구현 버그로 취급한다.

| config | 켜는 것 | 측정 목적 | 예상 (직전 대비) |
|---|---|---|---|
| `vdram-off` | 기존 `-gpu=virtual-caching` | 비교 기준선 | — |
| `vdram-ideal` | tag 검증만, 매핑은 자유 | VirtualDRAM 성능 **상한** | C0/C1/C2 = 0 |
| `vdram-naive` | `mod N` 강제 + `contig` VA + TZ 없음 | 매핑 제약만의 비용 | footprint<N_hz면 여전히 ~0 (§0 검증) |
| `vdram-realva` | + `cuda-like` VA | **collision이 실제로 나타나는 지점** | C0/C1/C2 급증 |
| `vdram-tz` | + Translation Zone 20%, LRU | TZ의 흡수 효과 | C2 감소, C1은 유지 |
| `vdram-ncma` | + bin-aware VA 선택 | NCMA 단독 효과 | C0 급감 |
| `vdram-cama` | + SABA (hot/cold bin budget) | 논문 최종 구성 | C1/C2 추가 감소 |

`vdram-naive`가 거의 0을 찍는 것은 **실패가 아니라 §0의 정량적 확인**이다. 이 행이
있어야 "collision은 mod 매핑 자체가 아니라 H를 모르는 VA 배치에서 온다"는 논문 §3.3의
논지가 데이터로 증명된다.

추가 스윕: TZ 비율 0/5/10/20/40%, VA 정렬 경계, fragmentation on/off.

---

## 2. 구현 위치

### 2.1 공유 상태 패키지 (신규)

```
mgpusim/amd/timing/vdram/hashzone/
├── hashzone.go   Bin(vpn), 레이아웃 산술, Config
└── registry.go   드라이버 소유 authoritative 상태 + 통계
```

**Utopia의 `restseg.Registry`가 거의 그대로 템플릿이다**
(`mgpusim/amd/timing/utopia/restseg/registry.go`). 차이는 두 가지뿐:
- associativity = 1 (direct-mapped) — RestSeg의 way 탐색이 사라짐
- Translation Zone victim pool이 추가됨

필요한 API: `Bin(vpn)`, `Occupant(bin) (pid, vpn, ok)`, `Place`, `Evict`,
`Lookup`, `DemoteToTZ`, `Stats()`.
`restseg`와 동일하게 **mutex + insertion-ordered 순회**로 병렬 엔진에서 결정론을 지킬 것.

### 2.2 할당 훅

`internal/memoryallocator.go:465 allocatePages()` —
`tryRestSegAllocate` / `tryAvatarAllocate` 바로 옆에 `tryVDRAMAllocate`를 추가하고,
같은 함수 안의 `nextVAddr` 계산을 VA 배치 훅으로 대체한다(§1.2).
인터페이스에는 `SetVDRAMRegistry(...)`, `ReserveHashZone(deviceID, tzRatio)`를 추가 —
Utopia의 `SetUtopiaRegistry` / `ReserveRestSeg`와 대칭.

### 2.3 런타임 검증 지점

virtual-caching은 **L2 miss에서만** 변환한다 (CLAUDE.md ANTI-PATTERNS, 메모리
`vc-l1-translation-removed`와 일치). VirtualDRAM은 이 지점을 그대로 물려받는다:

- L2 miss → per-slice L2 address translator → **GMMU 대신 hashzone lookup + tag 비교**
- **hit**: PA 즉시 반환, page walk 없음 → 논문의 fast path
- **fail**: 기존 GMMU walk 경로로 폴백 → 논문의 safe path (§3.2 ⓔ)

즉 새 데이터패스를 만들 필요가 없다. **기존 변환 경로를 조건부로 우회**시키는 것이
전부이고, C1 카운터는 정확히 이 분기점에서 센다.

### 2.4 fault / migration 경로

UVM manager를 재사용한다 (`uvm_fault.go:285 onPageFault`, `uvm_eviction.go`).
논문 §3.2의 ❸❹❺(TZ 콜드 페이지 축출 → HZ victim을 TZ로 → 목표 페이지를 HZ로)는
`evictVictimsThen` + `beginEviction`의 2단 축출로 자연스럽게 표현된다.

> **범위 결정:** 다른 `-gpu=*`(latpc/softwalker/avatar/utopia)는 모두 `-uvm`을
> **금지**하는데, VirtualDRAM은 반대로 **UVM이 필수**다. fault와 migration이 없으면
> collision을 해소할 방법 자체가 없기 때문. `flag.go`의 검증 함수에 이 반대 방향
> 제약을 명시적으로 쓸 것.

통계는 `UVMStats`에 필드를 더하지 말고 **별도 `VDRAMStats`로 분리**한다.
§1.1의 collision-fault / capacity-fault 구분이 UVM 카운터와 섞이면 되돌리기 어렵다.

### 2.5 빌더 · 플래그 · 리포트 · 스크립트

- `timingconfig/builder.go` — `case "vdram"` 추가 (`builder.go:740` 패턴)
  + `timingconfig/vdram/builder.go` (virtual-caching 빌더를 임베딩)
- `flag.go` — `-gpu=vdram`, `-vdram-tz-ratio`, `-vdram-va-layout`,
  `-vdram-va-align`, `-vdram-hashzone-bytes`, `-vdram-cama=off|ncma|saba`,
  `-vdram-frag`
- `report.go` — `collectVDRAMUnits()` + `reportVDRAM()` (`reportUtopia` 패턴,
  `report()` 목록에 추가)
- `scripts/3_gen_runners.py` + `scripts/5_collect_metrics.py` — configs 목록에
  §1.4의 7개 config와 스윕 항목 추가

---

## 3. 진행 단계와 종료 조건

각 단계의 종료 조건을 만족하지 못하면 다음 단계로 넘어가지 않는다.

**P0. 오프라인 collision analyzer (시뮬레이터 수정 없음)** ← 가장 먼저 할 것

지금 있는 것만으로 파이썬 스크립트를 짠다: 벤치마크별 allocation 크기 목록을 뽑아
(`memory_footprint_*` 및 필요하면 allocator에 임시 로그 한 줄) VA 배치 모델 ×
`N_hz` 조합별로 C0 지표를 **해석적으로 계산**한다.

- 왜: 타이밍 모델을 한 줄도 건드리기 전에 §1.2/§1.3 설계가 유효한지 — 즉 **collision이
  0이 아닌지** — 를 반나절에 확인한다. 여기서 안 나오면 뒤 작업 전부가 헛수고다.
- 종료 조건: 최소 한 개의 (VA 모델 × footprint 비율) 조합에서 C0 > 0 이고,
  벤치마크 간 차이가 유의미하게 벌어진다.
- 보너스: 이 결과 자체가 논문의 motivation 그림 재료가 된다.

**P1. hashzone 패키지 + 할당 훅** — C0 계측까지.
종료 조건: `vdram-naive` / `vdram-realva`의 C0가 P0의 해석적 계산과 **일치**.
(이 교차검증이 P0를 먼저 하는 두 번째 이유다.)

**P2. 런타임 검증 경로** — C1 계측. `-gpu=vdram`이 virtual-caching과 동일 결과를
내되 walk 수가 급감하는지 확인. 종료 조건: `-verify` 통과 + `vdram-ideal`에서
`vdram_validation_fails == 0`.

**P3. Translation Zone** — C2 계측, collision/capacity fault 분리.
종료 조건: TZ 비율을 올리면 `vdram_collision_faults`가 단조 감소.

**P4. NCMA → SABA** — 순서대로. NCMA만으로 C0가 얼마나 줄어드는지 먼저 확정한 뒤
SABA를 얹어야 두 기여를 분리해 보고할 수 있다.

**P5. 스크립트 · 스윕 · 플롯** — `6_plot_normalized.py` 옆에 collision 분해
스택바(`9_plot_vdram_collision.py`) 추가.

---

## 4. 리스크와 미리 정해둘 것

- **용량 스케일 차이.** 논문 Table 2는 HBM3 80채널/A100급인데 시뮬은 4GB다. N이 20배
  이상 작아 collision rate를 그대로 논문 수치로 쓸 수 없다. → 모든 용량을 **벤치마크
  footprint 대비 상대값**으로 보고할 것. 절대 bin 수는 부차적.
- **SABA는 컴파일타임 정보가 필요하다.** 시뮬에는 LLVM 패스가 없으므로 allocation
  크기 + 첫 커널의 접근 밀도 프로파일로 근사할 수밖에 없다. 이걸 논문에 **"oracle
  SABA"로 명시**하는 편이 정직하고, 리뷰어 방어에도 낫다.
- **원고 §4.1 수정 필요.** 현재 "modified Accel-Sim/GPGPU-Sim framework"라고 적혀
  있는데 실제 평가는 MGPUSim/akita다. Table 2의 파라미터도 이 저장소의 실제 설정
  (메모리 `target-hw-spec` 참조)과 대조해 맞춰야 한다.
- **ECC tag의 false positive**는 논문과 동일하게 negligible로 두고 모델링하지 않는다.
  Synonym/homonym(§3.5)도 이번 collision 측정 범위 밖 — 별도 작업으로 분리.
- **결정론.** 병렬 엔진에서 카운터를 컴포넌트에 흩어놓으면 실행마다 값이 흔들린다.
  Utopia처럼 registry 한 곳에 모으고 mutex로 보호할 것.

---

## 5. 시간이 없다면 (최소 경로)

`P0` → `vdram-realva` / `vdram-tz` / `vdram-cama` 3개 config만.
이것만으로도 "restrictive mapping이 collision을 만들고, TZ와 CAMA가 각각 얼마나
줄이는가"라는 논문의 핵심 주장은 데이터로 세울 수 있다.
