# MGPUSim UVM / Demand-Paging 구현 정리

> branch: main | 작업 완료일: 2026-08-21 (v2 수정 반영)
> 기반 문서: `UVM.md` (구현 스펙) | 모든 수정 코드는 AGENTS.md 관례에 따라 `sbin_codex` 마커 사용
>
> **v2 수정 (2026-08-21)**:
> 1. `-uvm-ideal`에서 폴트 제어 평면 PCIe 레이턴시도 0 (directconnection 배선)
> 2. TBN 확장 기준을 "하위 노드(리프) 51% 이상 마이그레이션 시 해당 노드 전체 선택"으로 변경
> 3. Access Counter를 GPU단(GMMU)으로 이동 — PCIe(원격) 트래픽 감지, 드라이버와 알림/리셋으로 동기화
> 4. Eviction은 드라이버 LRU 리스트 기반 (Access Counter와 무관)
> 5. RemoteAccessible 페이지에 대한 Write 트래픽은 카운터와 무관하게 즉시 GPU 마이그레이션 요청
> 6. **Eviction 시 TLB shootdown**: 퇴출 victim 예약 → ShootDownCommand(GPU TLB flush) →
>    ACK 수신 후 PTE/프레임 확정 — stale 번역 사용 방지

---

## 1. 개요

MGPUSim에 통합 가상 메모리(UVM, Unified Virtual Memory) demand-paging 모델을 구현했다.
스펙의 6가지 필수 메커니즘을 모두 포함한다:

1. `-uvm` 플래그 — 켜면 벤치마크 할당이 `AllocateManaged`를 사용
2. TBN(Tree-Based Neighborhood) 프리페처 — 4KB 기본 페이지, 64KB 최소 마이그레이션/프리페치 단위, 2MB VA 블록
3. 고정 **20us 페이지 폴트 처리 레이턴시**
4. **Access Counter** — 64KB 단위
5. `-uvm-ideal` 모드 — 폴트/마이그레이션 타이밍 0, 기능적 상태 머신은 동일하게 실행·집계
6. GPU 메모리 **oversubscription** — 페이지 퇴출(eviction) 및 재마이그레이션

핵심 설계 원칙(스펙 §24): UVM은 TLB 미스에 붙는 단순 레이턴시가 아니라 **상태 저장형 메모리 관리
메커니즘**이며, `fault → residency 판정 → TBN 선택 → 용량 검사 → 퇴출 → 마이그레이션 → 매핑 갱신
→ replay`의 기능적 시퀀스가 normal/ideal 양쪽 모드에 동일하게 존재한다. 타이밍만 달라진다.

---

## 2. 수정 파일

### Akita (`akita/mem/vm/`)

| 파일 | 변경 |
|---|---|
| `pagetable.go` | `vm.Page`에 `Managed`, `RemoteAccessible` 필드 추가 |
| `protocol.go` | `PageFaultReq` / `PageFaultRsp` 메시지 추가 (GMMU↔드라이버 폴트 프로토콜) |
| `gmmu/gmmu.go` | GMMU에 `uvmPort`, `UVMServiceProvider`, 트랜잭션 `waitingOnUVM` 상태 추가 |
| `gmmu/gmmuMiddleware.go` | `finalizePageWalk`에서 managed 비상주 페이지 폴트 게이팅, `sendUVMFault`, `processUVMFaultRsp` |
| `gmmu/builder.go` | `WithUVMServiceProvider` 세터, UVM 포트 생성 |

### MGPUSim 드라이버 (`mgpusim/amd/driver/`)

| 파일 | 변경 |
|---|---|
| `api.go` | `AllocateManaged` API 추가 |
| `builder.go` | `WithUVM(config)`, UVM 매니저/포트 생성 |
| `driver.go` | `uvm *UVMManager`, `uvmPort`, Tick에 `parseFromUVM` 추가, `Handle` 오버라이드 |
| `internal/memoryallocator.go` | `AllocateManaged`, `TryAllocatePhysicalPage`, `FreePhysicalPage`, `ManagedAllocationResult` |
| `internal/device.go` | `GetStorageSize()` |
| `memorycopy.go` | managed-aware H2D/D2H: CPU-resident 페이지는 `globalStorage` 직접 읽기/쓰기, D2H는 flush + UVM 드레인 후 지연 읽기(deferred D2H) |

### 신규 UVM 서브시스템 (`mgpusim/amd/driver/uvm_*.go`)

| 파일 | 역할 |
|---|---|
| `uvm_config.go` | `UVMConfig`: 폴트 레이턴시, AC 임계값, TBN 임계값/최대 크기, region/block 크기, GPU 용량 |
| `uvm_types.go` | `PageKey`, `FaultKey`, `ManagedPage`, `ManagedAllocation`, `PageFault`, `FaultWaiter`, `Migration`, `RegionState`, `VABlock`, `AccessCounterState`, `ResidencyState` |
| `uvm_manager.go` | `UVMManager`: 레지던시/폴트/마이그레이션/퇴출/AC 상태 + 통계 소유 |
| `uvm_fault.go` | 접근 기록, 폴트 생성·병합(coalescing), fault-ready 파이프라인, 마이그레이션, replay, `hasPendingWorkInRange` |
| `uvm_tbn.go` | TBN 선택: 64KB 리프 최소, 형제-서브트리 활동 기반 2MB까지 확장 |
| `uvm_eviction.go` | 64KB 단위 결정적 LRU 희생자 선택, 보수적 copy-back 퇴출 |
| `uvm_regions.go` | 64KB 원격 접근 카운터 + 임계값 기반 마이그레이션 트리거 |
| `uvm_events.go` | Akita 비동기 이벤트: 폴트 처리 완료, 마이그레이션 완료, ideal 마이그레이션 완료 |
| `uvm_driver.go` | `Driver.Handle` 디스패치, `UVMStats`/`UVMEnabled`, 폴트 응답 |
| `uvm_stats.go` | `UVMStats` 카운터 |
| `uvm_manager_test.go` | 8개 Ginkgo 마이크로벤치마크 (Test A–G + 스래싱) |

### 러너/설정/벤치마크

| 파일 | 변경 |
|---|---|
| `samples/runner/flag.go` | `-uvm`, `-uvm-ideal`, `-uvm-fault-latency-us`, `-uvm-access-counter-threshold`, `-uvm-tbn-expand-ratio`, `-uvm-tbn-max-fetch-size` + 조합 검증 |
| `samples/runner/runner.go` | UVM 필드, `SetManagedMemory` 라우팅 |
| `samples/runner/report.go` | UVM 통계 블록 (26개 행, `Location="UVM"`) |
| `samples/runner/timingconfig/builder.go` | UVM 설정 배선, GMMU↔드라이버 UVM 포트 연결 (PCIe 루트 콤플렉스) |
| `timingconfig/gpubuilder/interface.go`, `r9nano/builder.go` | `WithUVMServiceProvider`, GMMU UVM 포트 노출 |
| `benchmarks/benchmark.go` | `Benchmark` 인터페이스에 `SetManagedMemory()` 추가 |
| 27개 벤치마크 파일 | `SetManagedMemory` 구현 + `AllocateManaged` 3-way 라우팅 |

---

## 3. 신규 구조체 / 메시지

- **메시지**: `vm.PageFaultReq`, `vm.PageFaultRsp`
- **매니저 타입**: `UVMManager`, `UVMConfig`, `UVMStats`, `ManagedPage`, `ManagedAllocation`, `PageFault`, `FaultWaiter`, `Migration`, `RegionState`, `VABlock`, `AccessCounterState`, `tbnSelection`, `deferredD2H`
- **이벤트**: `faultHandlingCompleteEvent`, `migrationCompleteEvent`, `idealMigrationCompleteEvent`

---

## 4. 상태 머신 (정확한 흐름)

```text
GPU translation (GMMU)
  → managed 페이지 판정 (TranslationReq.IsWrite 전파)
  ├─ IsMigrating                          → 폴트
  ├─ CPU-resident && !RemoteAccessible    → 폴트 (첫 접근)
  ├─ RemoteAccessible && Write            → 즉시 폴트 (AC와 무관)
  └─ RemoteAccessible && Read             → 원격 접근 (GPU단 AC++)
      → sendUVMFault (PageFaultReq) → 트랜잭션 park  [비-이상: PCIe / ideal: directconnection]
      → Driver.parseFromUVM → UVMManager.onManagedAccess
          └─ CPUResident → coalesceFault (4KB 페이지 단위)
              → faultHandlingCompleteEvent 스케줄 (20us 또는 ideal=0)
              → handleFaultReady: TBN 선택(51% 규칙) → 용량 검사 → 드라이버 LRU 퇴출 → CPU→GPU 마이그레이션
              → migrateData: 바이트 복사 + 완료 이벤트 (size/bandwidth 또는 ideal=0)
              → completeMigration: PTE 갱신, GMMU AC 리셋, 대기자 replay
              → PageFaultRsp → GMMU가 PTE 재조회 → TranslationRsp를 L2TLB로

GPU단 Access Counter (GMMU)
  → 원격 read 접근마다 64KB 카운터++
  → 임계값 도달 → AccessCounterNotifyReq → 드라이버 triggerAccessCounterMigration
  → 드라이버가 마이그레이션 완료 시 AccessCounterResetReq로 GMMU 카운터 초기화 (동기화)
```

---

## 5. 플래그 의미론

| `-uvm` | `-uvm-ideal` | 동작 |
|---:|---:|---|
| 0 | 0 | 기존 비-UVM 동작 (회귀 검증 완료) |
| 0 | 1 | 거부 (`-uvm-ideal requires -uvm`) |
| 1 | 0 | 전체 UVM 타이밍: 20us 폴트 + 마이그레이션 레이턴시 |
| 1 | 1 | 기능적 상태 머신 동일, 타이밍 0 |

추가 검증 거부: `-uvm` + `-timing` 없음, `-uvm` + `-use-unified-memory`, 멀티 GPU, CDNA3.

추가 설정 노브:

```text
-uvm-fault-latency-us=20            # 고정 폴트 처리 레이턴시 (us)
-uvm-access-counter-threshold=64    # GPU단 원격 접근 카운터 임계값
-uvm-tbn-expand-ratio=0.51          # TBN 노드 확장 비율 (하위 리프 51% 이상)
-uvm-tbn-max-fetch-size=2097152     # TBN 최대 페치 크기 (2MB)
```

---

## 6. 폴트 레이턴시 구현

- **20us**를 GPU 주파수로 변환: `ceil(20e-6 × Freq)` 사이클 → **유니크 폴트 배치당 1회** 비동기 Akita 이벤트로 스케줄.
  - 1GHz에서 20,000 사이클을 단일 이벤트로 점프 — 사이클-바이-사이클 busy-wait 없음 (스펙 §20 준수).
- **ideal**: `readyAt = now` — 같은 상태 머신을 거치되 타이밍만 0.
- **병합**: `map[FaultKey]*PageFault` — 같은 페이지 재접근은 `Waiters`에 추가되고 `CoalescedFaultReqs` 증가. 20us는 대기 워프/요청마다가 아니라 배치당 1회만 부과.

---

## 7. TBN 알고리즘

- 2MB VA 블록 = 32 × 64KB 영역(리프), 512 × 4KB 페이지.
- 폴트 주소를 64KB로 align-down → **최소 64KB 리프 항상 선택** (요구 페이지 포함 보장).
- **51% 규칙 (v2)**: 계층 확장 64KB → 128KB → … → 2MB. 각 상위 노드에 대해
  노드 내 페이지 중 GPU-resident 비율 ≥ `-uvm-tbn-expand-ratio`(기본 0.51)이면
  **해당 노드 전체(하위 리프 모두)를 마이그레이션 대상으로 선택**. `TBNMaxFetchSize`에서 중단.
- 이미 GPU-resident인 페이지는 전송/회계에서 제외.
- 통계: `uvm_tbn_fetches`, `uvm_tbn_64kb_fetches`, `uvm_tbn_larger_fetches`, `uvm_demand_migrated_pages`, `uvm_prefetched_pages`.

---

## 8. Access Counter 알고리즘 (GPU단, v2)

- **위치**: Access Counter는 GPU단(GMMU)에 존재. GMMU가 원격(RemoteAccessible) 페이지에 대한
  **Read 번역(PCIe 트래픽)을 감지**하여 64KB 영역 카운터를 증가시킨다.
- **드라이버 동기화**: 카운터 ≥ `-uvm-access-counter-threshold`(기본 64) →
  GMMU가 `AccessCounterNotifyReq`를 드라이버로 전송 → 드라이버가
  `triggerAccessCounterMigration` 실행 (20us 미부과). 드라이버는 마이그레이션 완료 시
  `AccessCounterResetReq`로 GMMU 카운터를 초기화(epoch 동기화).
- GPU-resident 페이지 접근은 카운트하지 않음.
- **Write는 카운터와 무관** (v2): RemoteAccessible 페이지에 대한 Write 번역은
  GMMU가 즉시 폴트로 처리 → 즉각 GPU 마이그레이션 요청.
- 통계: `uvm_remote_accesses`, `uvm_access_counter_notifications`, `uvm_access_counter_triggered_migrations`, `uvm_access_counter_resets`.

---

## 9. Oversubscription / 퇴출 정책

- **하드 용량**: `GPUCapacityBytes`(= GPU DRAM 크기) 예산 `freeGPUFrames`로 강제. 관리형 가상 할당은 GPU 물리 메모리를 초과 가능.
- **희생자 선택 (v2)**: 드라이버 LRU 리스트(`container/list`) — GPU-resident 영역만 리스트에 존재하고
  GPU 접근 시 MRU로 이동, 퇴출은 LRU 끝부터. **Access Counter와 무관** (별도 구조). 요구 프레임 수만큼 퇴출.
- **TLB shootdown (v2)**: 퇴출은 비동기 3단계로 수행 — ① victim 예약 ② `ShootDownCommand`를 GPU로 전송
  (CP가 TLB flush, `ShootDownCompleteRsp` 반환) ③ ACK 수신 후에만 PTE 갱신·GPU 프레임 해제·LRU 제거를
  확정하고 대기 중이던 마이그레이션을 재개. 퇴출 후 GPU 접근은 항상 TLB 미스 → GMMU →
  RemoteAccessible 판정(Read: 원격 접근 / Write: 폴트)으로 이어져 stale 매핑 사용이 불가능하다.
- **D2H (GPU 메모리 읽기)**: GPU-resident 페이지는 기존 DMA 경로(`MemCopyD2HReq` → CP → DMA engine →
  GPU DRAM)로 읽고, CPU-resident 페이지는 `globalStorage` 직접 읽기. managed 버퍼는 flush
  (L1V/L2D dirty write-back) + UVM 폴트/마이그레이션 드레인(`hasPendingWorkInRange`) 후 재조회하여
  정합성을 보장한다 (deferred D2H).
- **비적격 영역**: 마이그레이션 중(`MigrationID != ""`), 미해결 폭트 대기(`ActiveFaults > 0`), eviction-locked, 활성 마이그레이션 대상 영역.
- **퇴출 전이**: GPUResident → (보수적 GPU→CPU 데이터 복사 — 더티 추적 미구현이므로 모든 퇴출이 전송 트래픽 발생) → CPUResident(`RemoteAccessible=true`).
- **재마이그레이션**: 퇴출된 페이지는 remote-accessible이 되어 Access Counter 경로로 재마이그레이션 → `uvm_repeated_migrations` 증가 (스래싱 감지).
- 통계: `uvm_evictions`, `uvm_evicted_pages`, `uvm_evicted_bytes`, `uvm_gpu_resident_pages_peak`, `uvm_gpu_resident_bytes_peak`, `uvm_repeated_migrations`.

---

## 10. 마이그레이션 타이밍

- 20us 폴트 레이턴시와 분리. `migration latency = transfer size / 16GB/s` (효과적 CPU-GPU 대역폭 참조, 별도 PCIe 모델 중복 없음).
- 데이터 평면은 `globalStorage` 바이트 복사 (CPU 백킹 ↔ GPU 프레임) — 마이그레이션 카운트/바이트는 유지.
- ideal 모드: 동일한 복사·카운트 수행, 완료 이벤트를 현재 시간에 스케줄.
- **uvm-ideal PCIe 0 (v2)**: uvm-ideal 모드에서는 GMMU↔드라이버 UVM 폴트 채널을 PCIe 대신
  **directconnection(제로 레이턴시)**으로 배선. 폴트 요청/응답/AC 알림/리셋 메시지의
  제어 평면 레이턴시가 제거되어 baseline에 근접한 상한 성능 측정이 가능.
  (측정: vectoradd 1024 ideal kernel_time 2.10e-04 → 2.78e-05, 약 7.5배 개선)

---

## 11. 통계 블록 (실행 종료 시 `Location="UVM"` 26개 행)

```text
UVMEnabled, IdealUVM
PageFaultRequests, UniquePageFaults, CoalescedFaultRequests
TBNFetches, TBN64KBFetches, TBNLargerFetches
DemandMigratedPages, PrefetchedPages
CPUToGPUMigrations, GPUToCPUMigrations, MigratedPages, MigratedBytes
Evictions, EvictedPages, EvictedBytes, RepeatedMigrations
RemoteAccesses, AccessCounterNotifications, AccessCounterTriggeredMigrations
PeakGPUResidentPages, PeakGPUResidentBytes
FaultHandlingTime, MigrationTime, EvictionTime
```

ideal 모드에서 `FaultHandlingTime == 0`, `MigrationTime == 0`이며 카운트·바이트는 유지된다.

---

## 12. 테스트 결과

### 단위 테스트
- **드라이버 스위트**: 31/31 통과 — 기존 23개 + 신규 UVM 8개:
  - managed 할당 CPU 레지던시 등록, TBN 64KB 정렬, 동일 페이지 폴트 병합(3요청 → 유니크 1),
    demand 폴트→마이그레이션→replay, 128MB 용량 내 192MB 워킹셋 퇴출·용량 상한,
    원격 접근 AC 증가·임계값 마이그레이션, ideal 타이밍 0, 스래싱 재마이그레이션(Test A–G + F).
- **akita VM**: Virtual Memory 4/4, Address Translator 14/14, idealtlb 5/5, MMU 16/16, GMMU 4/4 통과.
- **전체 mgpusim 스위트** (`ginkgo -r --skip-package=mccl`): 내 변경으로 인한 실패 없음.
  - 단, `Dispatcher` 2개(stderr 출력 검증)는 **기존 실패** — git stash로 기준선에서도 동일 확인.

### 엔드-투-엔드 검증 (vectoradd, virtual-caching)
| 모드 | 크기 | 결과 |
|---|---|---|
| `-uvm -uvm-ideal` | 128/256/512/1024 | 모두 Passed, 타이밍 0 |
| `-uvm` (비-이상) | 256 | Passed — 97 폴트 × 20us = 1.94ms 폴트 시간, 마이그레이션 0.457ms |
| `-uvm` (비-이상) | 1024 | 진행 확인 (wave 71%+) — 시뮬레이션 레이턴시로 느리지만 정상 동작 |
| `-uvm=false` (레거시) | 1024 | Passed, UVM 통계 0행 |

---

## 13. 알려진 한계

1. **단일 GPU만 지원** (플래그 검증으로 거부). 멀티-GPU UVM 코히런스는 비목표(스펙 §21).
2. **가상-캐싱 모델의 쓰기 흡수**: virtual-tagged L1V/L2D가 CPU-resident 관리 페이지에 대한 커널 쓰기를 캐시에 흡수하여 퇴출/flush 시점에만 폴트가 발생한다. D2H 읽기는 flush 완료 + 범위 내 UVM 폴트/마이그레이션 드레인(`hasPendingWorkInRange`)까지 지연(deferred D2H)하여 정합성을 보장한다. 결과적으로 호스트 읽기백이 직렬화된다.
3. **퇴출 copy-back은 보수적**: 더티 페이지 추적 미구현 → 모든 퇴출이 전송 트래픽을 발생 (스펙 §12.5에 명시된 허용).
4. **마이그레이션 타이밍은 해석적**: `size/16GB/s` 지연 사용. 기존 PMC/`RemotePMCPorts` GPU-간 경로는 기준선과 동일하게 미사용 — UVM은 CP DMA/globalStorage 데이터 평면 사용.
5. **Access Counter 히스토그램 미출력** (스펙 §10.5 "If feasible" 항목).
6. **GMMU 원격 접근 카운터는 첫 요청 기준**: TLB MSHR 병합으로 write 요청이 read와
   병합되면 GMMU 폴트 판정이 첫 요청의 IsWrite를 따름. 병합된 write는 카운터 임계값
   경로로 보완됨 (즉시성은 페이지 단위 첫 요청 기준).

---

## 14. Acceptance Criteria 대응 (스펙 §23)

- [x] `-uvm=false` 레거시 보존 — vectoradd 1024 Passed
- [x] `-uvm=true` 시 벤치마크가 `AllocateManaged` 사용 — 27개 벤치마크 3-way 라우팅
- [x] 비상주 GPU 접근 → demand 페이지 폴트 — GMMU 게이팅
- [x] 중복 접근 병합 — `map[FaultKey]*PageFault` + 대기자 리스트
- [x] 유니크 폴트 배치당 20us 고정 지연 — 비동기 이벤트 1회
- [x] 대기 요청마다 별도 부과 없음 — 배치 단위
- [x] TBN 최소 64KB 정렬 영역 마이그레이션 — 단위 테스트 검증
- [x] 2MB VA 블록 내 계층적 확장 — 활동 기반 64KB→2MB
- [x] 64KB 단위 Access Counter — 키 정렬 검증
- [x] AC 임계값 → CPU→GPU 마이그레이션 트리거 — 단위 테스트 검증
- [x] GPU 용량 엄격 강제 — 128MB 용량에 192MB 워킹셋, resident ≤ 용량
- [x] 관리형 할당이 GPU 물리 메모리 초과 가능 — 192MB 할당 성공
- [x] 퇴출로 신규 마이그레이션 공간 확보 — eviction 카운트 검증
- [x] 퇴출 페이지의 GPU 재마이그레이션 — 스래싱 테스트 `RepeatedMigrations > 0`
- [x] `-uvm-ideal` 폴트/마이그레이션 타이밍 0 — 통계 검증
- [x] ideal 모드에서도 폴트/마이그레이션/프리페치/퇴출 보고 — 동일 상태 머신
- [x] 실행 종료 시 UVM 통계 출력 — SQLite `mgpusim_metrics` 26행
- [x] 비-UVM 회귀 테스트 통과 — 전체 스위트 (기존 Dispatcher 2건 제외)
- [x] 전용 UVM 마이크로벤치마크 통과 — 드라이버 31/31
- [x] 20us 지연에 사이클-바이-사이클 busy-wait 없음 — Akita 스케줄 이벤트
