# Utopia 구현 계획 (utopia.md 기반)

> 작성: 2026-08-26 | 마커: `sbin_claude_utopia` (수정 전 코드는 주석 처리)

## 0. 설계 원칙 (utopia.md 4.13)

- **정책(policy)은 드라이버에**: RestSeg 생성·소유권·점유율·마이그레이션 결정·TAR/SF 갱신·shootdown.
- **지연(timing)은 GPU 모델에**: RSW/FSW, TAR/SF 캐시 접근 지연.
- 수정 전 코드는 주석 처리, 수정 코드에 `sbin_claude_utopia` 표기.

## 1. 패키지 배치

```
mgpusim/amd/timing/utopia/                  ← 새 timed 컴포넌트 패키지
├── restseg/        RestSeg 레이아웃·해시 공유 타입 (드라이버·GPU 공용 leaf 패키지)
├── rsw/            RSW 워커 (UTU: Utopia Translation Unit)
└── tarsf/          TARCache / SFCache (pagewalkcache 스타일 지연 모델)

mgpusim/amd/samples/runner/timingconfig/utopia/   ← -gpu=utopia 빌더 (r9nano 임베딩)
mgpusim/amd/driver/utopia_restseg.go              ← 드라이버 측 RestSeg 매니저
```

`restseg`가 핵심: `Hash(VPN)`, `PFN = BasePFN + set*Assoc + way` 산술,
`RestSegConfig{Base, Size, PageSize, Assoc, NumSets}`를 한 곳에 두어
드라이버 할당과 GPU RSW 조회가 반드시 같은 레이아웃을 쓰도록 강제 (utopia.md 4.5).

## 2. `-gpu=utopia` 실행 경로

1. `runner/flag.go` gpu flag 설명에 `utopia` 추가 + utopia flag 군 추가.
2. `timingconfig/builder.go` `createGPUBuilder` switch에 `case "utopia"`.
3. `timingconfig/utopia/builder.go`: virtual-caching과 동일하게 `r9nano.Builder` 임베딩,
   `WithTranslationTopology(r9nano.NewUtopiaTranslationTopology(cfg))` 주입.
4. r9nano에 `TranslationTopology` 전략 인터페이스 신설
   (baseline = 현행 buildL2TLB/connectL2TLBToGMMU, utopia = UTU/TAR/SF 추가 배선).
5. `driver.Builder.WithUtopia(UtopiaConfig)` — 기존 `WithUVM(UVMConfig)` 패턴.

## 3. RestSeg/FlexSeg 비율 flag

| Flag | 의미 |
|---|---|
| `-utopia-restseg-ratio` | GPU 메모리 중 RestSeg 비율 (FlexSeg = 나머지) |
| `-utopia-restseg-size` | 바이트 직접 지정 (ratio보다 우선) |
| `-utopia-restseg-assoc` | way 수; `NumSets = (Size/PageSize)/Assoc` 자동 산출 |
| `-utopia-tar-cache-bytes` / `-utopia-sf-cache-bytes` | TAR/SF 캐시 크기 (기본 2KB) |
| `-utopia-tar-sf-latency` / `-utopia-tar-sf-miss-latency` | 캐시 hit/miss 지연 |
| `-utopia-alloc-mode` | `fault`(Mode A) / `ptw-track`(Mode B) |

크기는 `sets × assoc × pageSize`로 내림 정렬해 단일 `restseg.RestSegConfig`를 만들고
드라이버·GPU 빌더 양쪽에 동일 객체 전달. 2차 RestSeg(2MB)는 4KB 검증 후 슬라이스 확장.

## 4. GPU 측 번역 흐름 (utopia.md 4.7 순서 준수)

현재: `L1TLB → (l1TLBAddressMapper) → L2TLB → GMMU(FSW, 지연 모델)`.

**최종 구조 (병렬, 이중 접점):**

```
L1TLB bottom ──→ UTU.Top
UTU ──(lookup)──→ L2TLB.Top          ← l1TLBAddressMapper.Port를 UTU로 교체
UTU ──(RSW: SFCache → TARCache)      ← L2 lookup과 병렬 시작
L2TLB.LowModule ──→ UTU.Walk         ← L2 miss가 UTU로 복귀
UTU ──(NotInRestSeg && L2 miss)──→ GMMU.Top (FSW, GMMU 무수정)
```

- L2 hit 먼저 도착 → RSW 폐기 (`rsw-canceled` 통계) — 검증 테스트 4.
- L2 miss가 UTU.Walk에 도착하면 RSW 결과와 join: RSW hit이면 RestSeg 산술로 만든
  `vm.Page`를 응답(L2가 fill), NotInRestSeg일 때만 GMMU로 — FSW는 RSW 결과 전에 시작 금지.
- SF==0 set은 TAR 접근 생략 (feature 3).
- **중간 마일스톤**: UTU를 L2TLB↔GMMU 사이 직렬 배치(배선 절반, 순서 규칙 무위반) 후 병렬 전환.

**TAR/SF 캐시**: pagewalkcache 패턴(유한 capacity/assoc/latency/port).
miss 시 v1은 설정 가능한 메모리 지연 부과(기존 GMMU `pageWalkingLatency`와 같은 모델링 수준).
"실제 메모리 계층 경유 경합"(4.6)은 v2 — 현재 FSW 자체가 지연 모델이므로 v1 비대칭 회피.

## 5. 드라이버 측 RestSeg 매니저

- **프레임 예약**: 디바이스 등록 시 GPU 메모리 base에서 RestSeg 연속 구간을 일반 할당자
  풀에서 제외 → buddy/UVM eviction이 RestSeg 프레임을 배포 불가 (`RestSeg XOR FlexSeg` 불변식).
- **권위 상태**: `TAR[set][way]`, `SF[set]` authoritative 사본 + SRRIP 메타데이터 (feature 7).
- **할당 훅**: `allocatePages`에서 `Hash(VPN)` set에 빈 way 있으면 RestSeg, 없으면 FlexSeg.
  UVM 모드는 `uvm_fault.go` demand-fault 시점에 동일 로직 = Mode A.
- **Mode B**: GMMU per-page walk 횟수·비용 카운터 → threshold 초과 시 access-counter
  알림 경로(GMMU→CP→driver) 재사용해 마이그레이션 후보 통지.
- **마이그레이션**: SRRIP victim → uvm_migration.go page copy + shootdown.go TLB shootdown 재사용.
  UTU·TARCache·SFCache Ctrl 포트를 r9nano `addTLB()` 목록에 등록하면 기존 shootdown
  브로드캐스트가 TAR/SF 캐시 invalidation을 자동 커버.

## 6. 마일스톤

| 단계 | 내용 | 검증 | 상태 |
|---|---|---|---|
| P1 | restseg 공유 패키지 + flag/`-gpu=utopia` 골격 (r9nano와 동일 동작) | 기존 acceptance 통과 | ✅ 완료 (2026-08-26) |
| P2 | 드라이버 RestSeg 예약·할당 (Mode A) | 유닛: set/way 배정, XOR 불변식 | ✅ 완료 |
| P3 | TARCache/SFCache + UTU 직렬 버전 | utopia.md 4.15 테스트 1·3 (테스트 2는 unit 레벨 추가 예정) | ✅ 완료 |
| P6(일부) | 통계 report.go 연동 (`utopia_*` 메트릭) | -report-all CSV 확인 | ✅ 완료 |
| P4 | 병렬 L2TLB+RSW 전환 | 테스트 4 (L2 hit 시 RSW 취소) | ⬜ 미착수 |
| P5 | SRRIP 교체 + 마이그레이션 + shootdown 연동 | 테스트 5·6 | ⬜ 미착수 (UTU.InvalidateMetadataCaches 훅은 준비됨) |
| P6(잔여) | Mode B PTW 추적 + determinism/벤치마크 파이프라인 | 필수 flag 실행 + determinism | ⬜ 미착수 |

### 구현 파일 (P1~P3, 마커: sbin_claude_utopia)

신규:
- `mgpusim/amd/timing/utopia/restseg/` — Config(해시·PFN 산술)·Registry(권위 TAR/SF) + 유닛 테스트
- `mgpusim/amd/timing/utopia/rsw/` — UTU 컴포넌트 (SF→TAR 상태기계, 메타데이터 캐시, 통계)
- `mgpusim/amd/samples/runner/timingconfig/r9nano/translation_topology.go` — TranslationTopology 전략 (baseline/utopia)
- `mgpusim/amd/samples/runner/timingconfig/utopia/builder.go` — r9nano 임베딩 GPU 빌더
- `mgpusim/amd/driver/utopia.go` — UtopiaConfig + Driver.UtopiaRegistry()
- `mgpusim/amd/driver/internal/utopia_allocator_test.go`, `timingconfig/utopia_topology_test.go`

수정:
- `driver/internal/memoryallocator.go` — ReserveRestSeg, RestSeg-first 할당, GPU 페이지 테이블 제외(XOR 불변식), Free 시 TAR 반환
- `driver/builder.go`(createMemAllocator 추출), `driver/driver.go`(RegisterGPU 예약), `driver/context.go`(PID())
- `r9nano/builder.go` — translationTopology 훅 배선
- `timingconfig/builder.go` — utopia case + UtopiaPlatformConfig
- `runner/flag.go`, `runner/runner.go`, `runner/report.go` — flag·검증·utopia_* 메트릭
- `driver/mock_internal_test.go` — mockgen 재생성

### scripts 파이프라인 연동 (완료)
- `3_gen_runners.py`: `utopia` config + `utopia_restseg_ratios` sweep dict
  (예: `'utopia-rs-25': 0.25`; sweep 디렉토리는 자동 생성)
- `4_run_benchmarks.sh`: `configs=(utopia)` 활성 / `2_copy_benchmarks.sh`: utopia 디렉토리
- `5_collect_metrics.py`: `utopia` config + `utopia_*` 컬럼 10종 (UTU별 합산)
- `6_plot_normalized.py`(색상 포함)·`7_plot_l2_tlb_mpki.py`: utopia 추가

### 회귀 검증
- acceptance (1-GPU, gcn3, baseline r9nano): 15개 벤치마크 stderr 전부 "Passed", exit 0.
  ("stdout is not empty" 경고는 kernel-info/progress가 stdout으로 출력되는 기존 문제 —
  clean tree의 cp/internal/dispatching 테스트 실패와 동일 원인, utopia와 무관)
- 전체 ginkgo 스위트: dispatching 2건(기존, clean tree에서도 실패) 외 전부 통과.

### 실측 검증 (matrixtranspose -width=128 / fir -length=8192, verify 통과)
- 기본(512MB RestSeg): RSW hit 35, FlexSeg walk 0, 점유 35프레임
- `-utopia-restseg-size=65536 -utopia-restseg-assoc=4`: RSW hit 16(=전체 프레임), FlexSeg walk 19 → 비율 조정·fallback 동작 확인
- RestSeg 페이지는 GPU 페이지 테이블에 없으므로 verify 통과 = RSW가 실제 번역을 수행한 증거

## 7. 주의점

- **해시 일관성**: 드라이버 할당·UTU 조회가 다른 해시면 조용히 전부 miss.
  restseg 단일 소스 + 동일 config 유닛 테스트 필수.
- **TAR 태그에 PID 포함**: `{PID, VPN_tag}` 매치 (multi-process 안전).
- **`-parallel` 결정성**: TickingComponent + 포트 메시지로만 구현. Go map 순회 victim 선정 금지.
- **UVM 상호작용**: FSW 중 UVM fault는 기존 GMMU replay 큐가 처리. UVM eviction이
  RestSeg 페이지를 내보낼 때 TAR/SF 갱신 경로 필요 (P5).
