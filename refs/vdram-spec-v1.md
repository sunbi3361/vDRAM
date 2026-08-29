# VirtualDRAM — 구현용 요약

> MGPUSim 확장을 위한 설계 요약. ASPLOS 버전 기준 (2-way + VA base shift).

---

## 0. Overview

HBM을 두 영역으로 나눈다.

| 영역 | 비율 | 주소 방식 | 태그 |
|---|---|---|---|
| Hash Zone | ~95% | VA에서 해시로 위치 결정, 2-way | ECC에 임베딩 (TME) |
| Translation Zone | ~5% | 기존 page table (PIPT) | 없음 |

핵심: **translate-before-access → validate-on-access.**
MC는 VA를 받아 곧바로 DRAM에 접근하고, 돌아온 데이터의 ECC decode가 "이 페이지가 맞는지"를 동시에 판정한다. 별도 태그 배열도, lookup도 없다.

- 캐시는 VIVT (L1/L2). VA로 index/tag.
- 해시: `set = (VPN + f(ASID)) mod S`, `S = (프레임 수) / 2`
  - **ASID 항 필수.** 없으면 같은 바이너리를 도는 두 프로세스가 같은 VPN을 받아 전 페이지가 충돌한다.
- 파티션/채널 인터리빙은 기존 그대로. 해시는 **파티션 내부 오프셋만** 결정한다.
- **way 비트 = 같은 row 안의 column 비트.** way1 프로브는 activation 없이 column access(tCCD)만 추가된다.

---

## 1. ECC Tagging (TME)

기존 두 ECC의 syndrome 여유 공간에 태그를 실어 보낸다. **저장 0, 트래픽 0, 예측기 0.**

태그 예산: `TS ≤ R − b` → 시스템 SEC-DED 15b + 온다이 RS 8b × 2 = **31b**

| 코드 | 필드 | 성질 |
|---|---|---|
| System SEC-DED (15b) | ASID(6) + VA 상위(9) | verify-only |
| On-die RS (16b) | VA 하위(13) + Permission(3) | recoverable (∆t 복원 가능) |

**2-way 전환 시**: set 수가 절반이 되어 해시가 소비하는 VPN 비트가 1 줄고, **태그가 1비트 더 필요**하다.
VA[57:49] 9비트가 현재 상수이므로(실제 GPU는 49b VA) 여기서 차감한다.
→ **associativity ×2 = 마진 1비트.** 8-way까지 3비트 소모, 마진 9비트.

디코드 결과 (구현할 4가지):

| 결과 | 조건 | 동작 |
|---|---|---|
| MATCH | syndrome 0 | 데이터 반환 |
| 1-bit / 1-sym error | 정정 가능 | 정정 후 반환 |
| COL (tag mismatch) | 태그 불일치 | 다음 way → TZ / safe path |
| DUE | 그 외 | safe path |

**시뮬레이터에서는 실제 ECC 인코딩이 불필요하다.** 프레임마다 `(VPN, ASID, perm)` shadow를 저장하고 비교하면 충분하다. 대신 오분류율은 fault injection 결과 수치를 파라미터로 주입한다.

---

## 2. Memory Allocation

**연속 VA는 반드시 연속 bin 구간에 사상된다.** `mod S`이므로 길이 n 할당은 원주 S인 원 위의 **호(arc) 하나**가 된다. 따라서 allocator의 자유도는 청크 배열이 아니라 **회전량(base) 하나뿐**이다.

### 핵심 성질

> 호를 끝과 끝을 이어 붙여 배치하면 (`r_{i+1} = r_i + n_i mod S`),
> **Σnᵢ ≤ A·S 인 한 모든 set의 점유는 A 이하다.**
>
> 증명: 이어붙인 결과는 길이 `L = Σnᵢ`인 구간 `[0, L)`을 mod S로 감은 것이다.
> set s의 점유수는 `s + kS ∈ [0, L)`인 `k ≥ 0`의 개수 = `⌈(L − s)/S⌉ ≤ ⌈L/S⌉ ≤ A`. ∎

`A·S`는 곧 HBM의 총 프레임 수다.
→ **footprint가 HBM에 들어가기만 하면 배치는 항상 성공한다.** FAIL 경로가 없다.

### 알고리즘

```
alloc(n, asid):
    base_bin = cursor
    cursor   = (cursor + n) mod S
    VPN0     = mod S 조건을 만족하는 free VPN  # base_bin - f(asid)
    return VPN0                                # 연속 VA n 페이지
```

- free/realloc으로 단편화되면: set별 2-bit 점유 카운터 + 계층 요약 비트맵 위에서
  "모든 set에 여유 way가 있는 길이 n 구간"을 sliding window로 탐색한다.
  (S ≈ 10M이므로 요약 없이는 malloc당 10M 스캔 — 계층 구조 필수)
- `ptrs[]`, 컴파일러 패스, `cudaLaunchKernel` 인터셉트를 **전부 제거**한다.
  VA가 완전히 연속이므로 cuBLAS 등 closed binary도 그대로 동작한다.
- 2MB 페이지는 shift 단위가 512 bin이 되어 배치 해상도가 512배 거칠어진다. (별도 확인 필요)

### 2-way가 필요한 이유

위 정리는 A=1(direct-mapped)에서도 성립한다. 그럼에도 2-way가 필요한 지점은 정적 배치가 아니라 동적 상황이다.

1. **free/realloc 단편화** — 호는 연속이어야 하므로 direct-mapped는 길이 n의 *완전히 빈* 구간을 요구한다. 2-way는 "각 set에 way가 하나라도 남은" 구간이면 된다.
2. **allocator가 관여할 수 없는 할당** — 드라이버 내부 버퍼, closed library workspace, 다른 컨텍스트의 할당.
3. **oversubscription** — `L > A·S` 구간에서 resident set 선택의 자유도.
4. **다중 컨텍스트** — ASID 해시가 있어도 잔여 충돌이 남는다.

> 한 문장 요약: **associativity는 allocator를 "정확해야 하는 것"에서 "괜찮으면 되는 것"으로 바꾼다.**

---

## 3. Memory Access Flow

```
① VA 요청
    ↓ (synonym filter → 필요 시 canonical VA로 치환)
② VIVT L1/L2 접근 (VA index/tag, 라인에 perm + ASID 동반)
    ↓ miss
③ set = (VPN + f(ASID)) mod S  →  way 0 접근
    ↓
   [MATCH]     → 반환
   [COL/DUE]   → ④
    ↓
④ way 1 접근 (같은 row, column access만 추가)
    ↓
   [MATCH]     → 반환
   [COL/DUE]   → ⑤
    ↓
⑤ Safe path: TLB → page walk
    ↓
   [PTE valid]   → TZ 프레임에서 읽기
   [PTE invalid] → ⑥ page fault
    ↓
⑥ TZ에서 LRU cold page를 CPU로 축출
   → 해당 set의 victim way를 TZ로 이동
   → 요청 페이지를 그 way에 설치
```

### 필수 계측

- **way0 / way1 / TZ / fault 분해** — 분모는 **L2 miss**, **접근 횟수 가중**(페이지 수 아님)
- set 점유 히스토그램
- **way1 프로브의 same-row 타이밍 모델** — 미반영 시 full DRAM access로 계산되어 associativity 실험 전체가 왜곡된다
- row buffer hit rate, bank-level parallelism (BLP)
- "HBM에 빈 프레임이 있는데 배치가 실패한 횟수" — 0이어야 한다 (§2 정리의 검증)
- W/M sweep은 워크로드가 아니라 **HBM 용량**으로 조절

---

## 4. 구현 우선순위

| 순위 | 항목 | 이유 |
|---|---|---|
| 1 | way 수 파라미터화 (1/2/4/8/full) | "왜 2인가"의 실증 근거가 아직 없음. knee 위치에 따라 §IV 골격과 태그 예산 서술이 함께 바뀜 |
| 2 | same-row 타이밍 모델 | 미반영 시 1번 결과가 통째로 무의미 |
| 3 | shadow tag 비교 기반 validation | ECC 인코딩 없이 기능 검증 가능 |
| 4 | 동적 TZ (하한 + 확장/회수) | 정적 TZ에서 배치 실패가 0이 아니면 최적화가 아니라 버그 수정이 됨 |
| 5 | 다중 컨텍스트 (2~4 커널 동시) | ASID 해시의 효과 검증 |