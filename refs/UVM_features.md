# MGPUSim v4 UVM 구현 정리

> branch: main | 기반 스펙: `uvm-manager.md` (Draft v0.10)
> 모든 수정 코드는 AGENTS.md 관례에 따라 `sbin_claude` 마커 사용
>
> **v3 (uvm-manager.md v0.10 대응)** — 제어 평면 전면 재구성:
> 1. UVM 마이그레이션/퇴출에서 **GPU 전역 quiesce 제거**
>    (`RDMADrainCmd` / `ShootDownCommand` / `GPURestartReq` 미사용)
> 2. **64KB range-scoped** TLB 무효화 + 캐시 writeback/invalidate 도입
> 3. **CP가 GPU측 UVM 제어 엔드포인트** — GMMU/AC는 CP를 통해서만 드라이버와 통신
> 4. Fault 서비스 단위를 4KB → **64KB 트랜잭션**으로 변경, FIFO 단일 활성
> 5. 마이그레이션 데이터 평면을 **DMA Engine + PCIe**로 이관 (ideal 모드만 직접 복사)
> 6. Access Counter를 **GPU측으로 이동**, remote write는 **stall 후 마이그레이션**
> 7. GMMU가 **replay 큐**와 **range 무효화 코디네이터** 역할을 소유

---

## 1. 개요

`uvm-manager.md`가 규정하는 UVM 드라이버 모델을 구현한다. 핵심 원칙은
**어떤 UVM 전이도 GPU를 멈추지 않는다**는 것이다 (스펙 §2.1, §19, §21).
모든 무효화/플러시/replay는 64KB 영역 범위로 한정되며, 무관한 CU/캐시/TLB 트래픽은
계속 진행한다.

주소 계층 (스펙 §4):

| 개념 | 단위 |
|---|---:|
| 기본 페이지 / PTE / 폴트 식별 | 4KB |
| Fault 서비스 · TBN 리프 · Access Counter · 퇴출 | 64KB |
| VA Block (TBN 최대 확장) | 2MB |

---

## 2. 제어 평면

```text
                       UVMDriver (amd/driver)
                            |
                            |  PCIe  (ideal 모드에서는 directconnection)
                            v
                   Command Processor (ToUVMDriver)
                            |
                            |  GPU 내부 directconnection (ToUVMInternal)
                   /                          \
                  v                            v
              GMMU (UVM)                UVMAccessCounter (Ctrl)
                  |                             |
       +----------+-----------+          remote read 계수
       |                      |          remote write stall/replay
   replay 큐            range TLB 무효화
 (스톨된 번역)        (모든 L1/L2 TLB에 broadcast)
                            |
                     캐시 range WB+INV 은
                     CP가 ToCaches 로 직접 broadcast
```

### 메시지

| 메시지 | 방향 | 정의 위치 |
|---|---|---|
| `vm.PageFaultReq` | GMMU → CP → Driver | `akita/mem/vm/protocol.go` |
| `vm.UVMFaultReplayReq` | Driver → CP → GMMU + AC | `akita/mem/vm/uvmprotocol.go` |
| `vm.UVMTLBInvalidateReq/Rsp` | Driver → CP → GMMU → 모든 TLB | `akita/mem/vm/uvmprotocol.go` |
| `vm.UVMRemoteRetryRsp` | AC → AddressTranslator | `akita/mem/vm/uvmprotocol.go` |
| `vm.UVMDrainRangeReq/Rsp` | Driver → CP → 모든 AddressTranslator | `akita/mem/vm/uvmprotocol.go` |
| `vm.AccessCounterNotifyReq` | AC → CP → Driver | `akita/mem/vm/protocol.go` |
| `vm.AccessCounterResetReq` | Driver → CP → AC (`ResetAll` 포함) | `akita/mem/vm/protocol.go` |
| `protocol.UVMCacheRangeFlushReq/Rsp` | Driver → CP → 모든 데이터 캐시 | `mgpusim/amd/protocol/uvmprotocol.go` |
| `tlb.InvalidateRangeReq/Rsp` | GMMU → 각 TLB (비정지) | `akita/mem/vm/tlb/tlbprotocol.go` |
| `cache.RangeFlushReq/Rsp` | CP → 각 캐시 (비정지) | `akita/mem/cache/rangeprotocol.go` |

`ShootDownCommand` / `GPURestartReq` / `RDMADrainCmd` 는 **비-UVM unified-memory
마이그레이션 전용으로 남아 있으며 UVM 경로에서는 절대 사용되지 않는다.**

---

## 3. 상태 전이별 제어 동작 (스펙 §21.5)

| 전이 | DMA | 캐시 | TLB | CU flush |
|---|---|---|---|---|
| `INVALID → GPU_LOCAL` | H2D | 없음 | **없음** | 없음 |
| `REMOTE → GPU_LOCAL` | H2D | 없음 | **64KB 무효화** | 없음 |
| `GPU_LOCAL → REMOTE/INVALID` | D2H | **64KB WB+INV** | **64KB 무효화** | 없음 |

**퇴출 순서 해석 (스펙 §19 대비 변경)**: 스펙은 `캐시 WB+INV → TLB 무효화`
순서를 제시하며, 그가 안전한 이유는 §19.1이 "캐시가 해당 범위의 새 트랜잭션을 막는다"고
가정하기 때문이다. 본 모델은 같은 보장을 **번역 쪽에서** 얻는다:

```text
PTE park → 64KB TLB 무효화 → 모든 AT에 64KB range drain
        → 64KB 캐시 WB+INV → D2H
```

TLB 무효화 후에는 어느 CU도 victim의 GPU-local 번역을 새로 얻을 수 없고,
range drain은 그 직전에 번역된 요청이 캐시에 커밋될 때까지 기다린다. L1V는
write-around이므로 L2의 `WriteDoneRsp`를 받은 뒤에야 AT에 응답한다 — 즉 drain 완료는
"모든 store가 L2에 커밋됨"을 의미한다.

`INVALID → GPU_LOCAL` 에 TLB 작업이 없는 이유는 무효 번역을 TLB에 네거티브 캐싱하지
않기 때문이다. `REMOTE → GPU_LOCAL` 은 L2 TLB가 remote PTE를 캐싱할 수 있으므로
64KB 무효화가 필수다.

PTE 3상태는 기존 `vm.Page` 필드로 인코딩한다:

```text
GPU_LOCAL : DeviceID = gpu, RemoteAccessible = false
REMOTE    : DeviceID = 0,   RemoteAccessible = true
INVALID   : DeviceID = 0,   RemoteAccessible = false   -> GMMU 폴트
```

---

## 4. Fault 서비스 엔진

- Coalescing 키 = `(PID, GPU, 64KB 영역)` (스펙 §8.3).
- **활성 트랜잭션 1개, FIFO** (스펙 §8.4). 나머지는 `faultServiceCue` 대기.
- 고정 소프트웨어 레이턴시(기본 20us)는 **유니크 트랜잭션당 1회** 비동기 이벤트로
  스케줄 (스펙 §10.3). 사이클 단위 busy-wait 없음.
- GMMU는 폴트 발생 시 번역 트랜잭션을 `walkingTranslations`에서 꺼내 **`replayQueue`**로
  옮긴다. 폴트 폭주가 page-walk 슬롯을 잠식하지 못한다 (스펙 §22).
- 드라이버가 영역을 replayable로 만들면 `UVMFaultReplayReq(64KB)` 1건으로
  해당 범위의 모든 스톨 번역을 재실행한다.

측정 예 (`matrixtranspose -width=256 -uvm-access-counter=false`):
raw 128건 → unique 9건 + coalesced 119건, fault 시간 = 9 × 20us = 180us.

---

## 5. TBN 프리페처

```text
CurrentFaultExpanded64KBMask = 폴트 VA의 aligned 64KB 리프 전체
TBNOccupancyMask = GPUResidentMask | CurrentFaultExpanded64KBMask
확장 조건 : occupied*100 > total*51      (엄격히 >)
후보     : 64KB → 128KB → 256KB → 512KB → 1MB → 2MB
분모/선택 : VA Block의 유효 할당 페이지만 (스펙 §11.10)

PrefetchMigrationMask = Selected &^ GPUResident &^ Demand &^ InFlight
```

`PrefetchInFlightMask` / `MigratingToGPUMask` 는 occupancy 분자에 **포함하지 않고**
중복 DMA 억제에만 쓴다 (스펙 §11.13).

**해석 주의**: 스펙 §11.4 의사코드는 조상 노드를 아래에서 위로 훑되 **첫 실패에서
중단**한다. 따라서 §11.6 예시(256KB 75%)에 도달하려면 그 아래 128KB 노드가 먼저
임계값을 넘어야 한다. 구현과 단위 테스트는 §11.4 의사코드를 따른다.

---

## 6. Access Counter (GPU측)

- 위치: **GPU당 1개** `GPU[n].UVMAccessCounter`. AddressTranslator의 remote egress와
  RDMA ingress 사이에 삽입되어, "번역이 CPU-remote로 판정한 직후"를 관측한다
  (스펙 §6.1, §14). L2 TLB에 캐싱된 remote PTE도 계수를 우회할 수 없다.
- Remote **read**: PCIe로 포워딩 + 64KB 카운터 증가. 임계값(기본 **8**) 도달 시
  `AccessCounterNotifyReq` 1건 (영역·거주 에피소드당 1회 래치).
- Remote **write**: **호스트 메모리에 커밋하지 않는다** (스펙 §15). 요청을 보류하고
  즉시 마이그레이션을 요청한 뒤, 영역이 GPU-local이 되면
  `UVMRemoteRetryRsp`로 AddressTranslator에 반환 → 재번역 → GPU 메모리에서 완료.
- 리셋: 영역 마이그레이션 완료 시 및 **커널 런치마다 전체 리셋** (스펙 §14.2).
- 알림 억제: 대상 영역이 이미 fault/migration/prefetch 트랜잭션 소유 중이면 무시
  (스펙 §16).

측정 예 (`matrixtranspose -width=2048 -uvm -uvm-ideal`):
remote read 7,864건, remote write 1,536건 stall → 1,536건 replay,
AC 마이그레이션 513건 / 32MB.

---

## 7. 마이그레이션 데이터 평면

- Normal 모드: 선택된 4KB 페이지에서 **소스·목적지 물리주소가 모두 연속인 최대 run**을
  만들고 run당 `MemCopyH2DReq`/`MemCopyD2HReq` 1건 발행 → CP → **DMA Engine** → PCIe
  (스펙 §23.1, §23.1.2). UVM측 동시성 제한 없음; 백프레셔는 DMA/PCIe 모델이 담당.
- Ideal 모드: 동일한 상태 전이·카운터를 유지한 채 `globalStorage` 직접 복사 +
  현재 시각 완료 이벤트 (스펙 §1.2, §23.1.1).
- DMA Engine은 L2를 우회해 DRAM에 접근하므로 퇴출(D2H) 전에 64KB 캐시 WB+INV가
  반드시 선행한다.

---

## 8. Oversubscription / 퇴출

- 용량은 `-uvm-gpu-memory-capacity` 또는 `-uvm-gpu-memory-capacity-ratio` 로 지정
  (기본: GPU DRAM 전체 = oversubscription 없음).
- 퇴출 단위 64KB. victim 선택은 **migration-recency LRU** — `lastMigrationTime`은
  마이그레이션/admission 시에만 갱신하고 일반 GPU 접근으로는 갱신하지 않는다
  (스펙 §18.1, §31.2).
- 퇴출 시퀀스 (스펙 §19): `EVICTING` 표시 → 64KB 캐시 WB+INV → PTE park →
  64KB TLB 무효화 → D2H DMA → 최종 REMOTE/INVALID PTE 설치 → 프레임 해제.
- 모든 퇴출은 D2H를 수행한다 (더티 추적 미모델링, 스펙 §18.3에서 허용).

---

## 9. 플래그

```text
-uvm                                  # UVM managed memory 활성화
-uvm-ideal                            # 폴트/전송/제어 레이턴시 0, 카운터는 동일
-uvm-fault-latency-us=20              # 고정 소프트웨어 폴트 처리 레이턴시
-uvm-access-counter=true              # false면 cold page = INVALID (순수 demand paging)
-uvm-access-counter-threshold=8
-uvm-tbn-expand-ratio=0.51
-uvm-tbn-max-fetch-size=2097152
-uvm-gpu-memory-capacity=<bytes>      # 0 = GPU 메모리 전체
-uvm-gpu-memory-capacity-ratio=<f>
-uvm-disable-prefetch
-uvm-disable-eviction
```

거부되는 조합: `-uvm-ideal` 단독, `-uvm` + `-timing` 없음,
`-uvm` + `-use-unified-memory`, 멀티 GPU, CDNA3.

---

## 10. 통계 (`Location="UVM"`)

스펙 §27의 카운터 세트를 모두 노출한다. Faults / TBN(레벨별 확장 횟수 및 바이트) /
Migration(방향·트리거별) / Remote access / Access counter / Eviction /
Mapping control(PTE 설치, range 무효화, replay) / 잔류 피크 / 타이밍.

`-uvm-ideal` 에서는 `uvm_fault_service_latency_total` 과 `uvm_migration_time` 만 0이
되고 건수·바이트 카운터는 normal 모드와 동일한 정의를 유지한다 (스펙 §1.2).

---

## 11. 검증 결과

| 모드 | 벤치마크 | 결과 |
|---|---|---|
| `-uvm -uvm-ideal` | matrixtranspose 64 / 256 / **2048** | Passed |
| `-uvm` (normal) | matrixtranspose 64 / 256 | Passed |
| `-uvm -uvm-ideal` | kmeans 8192 pts | Passed |
| `-uvm -uvm-access-counter=false` | matrixtranspose 256 | Passed |
| `-uvm=false` (회귀) | matrixtranspose 256 | Passed |
| `-uvm ... -uvm-gpu-memory-capacity` (2배 oversubscribe) | matrixtranspose 128 (parallel/serial), 256 (serial) | Passed |
| `-uvm ... -uvm-gpu-memory-capacity` (2배 oversubscribe) | matrixtranspose 256/512 (parallel) | **미완료 stall** (§12.2) |

단위 테스트: 드라이버 스위트 35/35 통과 (managed 할당의 REMOTE/INVALID 초기 매핑,
64KB coalescing, FIFO 단일 활성, TBN 50%/75% 규칙, AC 마이그레이션과 억제,
용량 강제 퇴출, ideal 타이밍 0).
akita: `mem/vm/...`, `mem/cache/...` 전부 통과.

---

## 10.1 Admission 전 remote drain

CPU→GPU 마이그레이션은 호스트 메모리를 스냅샷한다. 드라이버가 마이그레이션을 거부해
Access Counter가 PCIe로 수행한 remote write는 **비동기로** 호스트 메모리에 도착하므로,
그 사이에 admission이 일어나면 GPU 사본이 그 store를 놓치고 이후 퇴출이 낡은 GPU
프레임을 올바른 호스트 데이터 위에 덮어쓴다 (실측으로 확인된 lost update).

이를 막기 위해 admission은 복사 전에 `UVMRemoteDrainReq`로 해당 영역에 미결
remote access가 없음을 확인한다: Driver → CP → Access Counter → ACK.

---

## 11.1 마이그레이션 거부 (capacity 소진 시)

GPU 용량이 부족하고 퇴출할 victim도 없으면 드라이버는 해당 영역에 대해
`UVMFaultReplayReq{Refused: true}`를 보낸다. Access Counter는 그 영역에 보류했던 write를
재번역으로 되돌리는 대신 **PCIe로 수행**한다. 이것이 없으면 stall된 write가
오지 않을 매핑을 영원히 기다려 데드락한다. 통계: `uvm_num_refused_migrations`,
`uvm_num_remote_writes_performed`.

---

## 12. 알려진 한계

1. **단일 GPU만 지원** (플래그 검증으로 거부).
2. **Oversubscription은 `-parallel` 대규모에서 진행이 멈춘다** —
   `-uvm-gpu-memory-capacity`로 용량을 managed 할당 총량 아래로 제한한 경우:
   - serial 실행: matrixtranspose 128 / 256 모두 **정확히 통과**
   - `-parallel`: 128은 통과하나 256 이상에서 **시뮬레이션이 진행되지 않는다(stall)**

   데이터 오류는 관측되지 않는다(잘못된 결과가 아니라 완료되지 않음). 원인은
   용량 소진 시 admission을 거부(`Refused`)하고 재시도를 유발하는 경로의 liveness
   문제로 보이며, 남은 작업이다. 용량 제한 없는 기본 구성(본 과제의 벤치마크
   스크립트)은 영향받지 않는다.
3. **퇴출 copy-back은 보수적** — 더티 페이지 추적 미구현이라 모든 퇴출이 D2H 트래픽을
   발생시킨다 (스펙 §18.3에서 허용).
4. **Remote atomic 미모델링** — 현재 메모리 프로토콜이 remote atomic을 표현하지 않아
   atomic은 일반 경로를 탄다 (스펙 §15.1은 명시적 거부를 요구).
5. **TBN 비트마스크 미사용** — occupancy를 페이지 순회로 계산한다. 의미는 동일하며
   스펙 §11.11이 허용하는 최적화 여지로 남겨둔다.
6. **커널 런치 AC 리셋의 순서** — 리셋은 UVM 제어 채널로, 커널은 GPU 채널로 가므로
   커널 경계 부근에서 약간의 순서 편차가 있을 수 있다.
