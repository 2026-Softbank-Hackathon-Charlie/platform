# SEC-07: KEV·EPSS 근거와 수집 시각 표시 — 설계

- 날짜: 2026-11-07
- 상태: 승인된 설계 (브레인스토밍 2026-11-07)
- 관련: ADR 0034 (보안 통제와 심각도 정책), SEC-04 (이미지 스캔)

## 목표

1. 실행 이미지 취약점마다 "지금 먼저 고쳐야 하는 근거"를 붙인다: CISA KEV 등재 여부와 FIRST EPSS 점수.
2. 보안 화면의 "수집 상태" 탭에서 수집 시각과 검사 시각을 구분해서 읽히게 한다.

하지 않는 것: 심각도 변경, 점수 합산, VEX 적용 (ADR에 "VEX 미적용"으로 명시).

## 1. 데이터 (API)

### 출처

| 출처 | URL | 주기 | 실패 시 |
|---|---|---|---|
| CISA KEV | `https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json` | 24시간마다 전체 카탈로그 | 마지막 성공본을 최대 3일 사용, 그 뒤 `unknown` |
| FIRST EPSS | `https://api.first.org/data/v1/epss?cve=A,B,...` | CVE별 24시간 캐시, 요청당 최대 100개 | 해당 CVE는 `unknown` |

확인한 응답 형식 (2026-11-07):
- KEV: 최상위 `catalogVersion`, `dateReleased`, `count`, `vulnerabilities[]`; 항목은 `cveID`, `dateAdded`, `dueDate`, `knownRansomwareCampaignUse` (`Known`/`Unknown`).
- EPSS: `data[]` 항목 `cve`, `epss`, `percentile`, `date`. 숫자는 **문자열**이다. 점수가 없는 CVE는 `data`에 없다 → `not_scored`.

### 구성

- 새 파일 `internal/security/exploit.go`: `ExploitIntel` (KEV 캐시 + EPSS 캐시, 둘 다 mutex 보호, base URL·HTTP client·now 주입 가능).
- 보고서를 만들 때 (조회 시점) 취약점 목록의 CVE를 모아 한 번에 붙인다. 전체 대기 상한 **8초**. 넘거나 실패하면 그 값만 `unknown`, 목록은 그대로 반환한다.
- CVE 결정: `advisory`가 `CVE-`로 시작하면 그것, 아니면 `aliases` 중 첫 `CVE-`. 없으면 둘 다 `unknown`이 아니라 **`no_cve`** 로 표시하고 조회하지 않는다 (화면: "CVE 없음").

### API 계약 (openapi)

`SecurityVulnerability`에 선택 필드 `exploitation` 추가:

```yaml
exploitation:
  cve: string | null
  kev:
    state: listed | not_listed | unknown | no_cve
    catalogVersion: string | null
    dateAdded: date | null
    dueDate: date | null
    ransomware: boolean | null   # knownRansomwareCampaignUse == "Known"
  epss:
    state: scored | not_scored | unknown | no_cve
    probability: number | null   # 0..1
    percentile: number | null    # 0..1
    date: date | null
```

취약점 보고서에 `exploitSources` 추가 (KEV, EPSS 두 행): `name`, `state` (ok/stale/unavailable), `version` (KEV catalogVersion 또는 EPSS 모델 날짜), `collectedAt`, `message`. 화면의 수집 상태 탭에 출처 행으로도 나온다.

### 오류 처리

- KEV fetch 실패 + 마지막 성공본이 3일 이내: 값은 사용, 출처 행 `stale` + 메시지.
- KEV 성공본 없음 또는 3일 초과: 모든 KEV `unknown`, 출처 행 `unavailable`.
- EPSS 배치 실패: 그 배치의 CVE만 `unknown`. 캐시에 있는 CVE는 캐시 값 사용.
- 어떤 실패도 취약점 목록 응답을 실패시키지 않는다.

## 2. 화면 (frontend)

### 취약점 목록

- 기본 정렬: KEV `listed` → 실행 중 → 심각도 → EPSS 내림차순. KEV `unknown`은 `not_listed`와 같은 순위지만 "KEV 미확인" 칩을 단다.
- 행마다 우선순위 이유 한 줄: 예) `KEV 등재 · 실행 중 · 수정 버전 있음`.
- 칩: `KEV 등재 (기한 11/18)`, `랜섬웨어 사용`, `EPSS 4.2% (상위 8%) · 11/06 기준`, `CVE 없음`, `KEV 미확인`, `EPSS 미확인`.

### 수집 상태 탭의 시각

두 줄로 표시하고 수집을 굵게:

```
**수집 11/07 09:12**
검사 11/07 08:00          ← 검사 시각이 있을 때
(검사 시각 미확인) ⓘ      ← 없을 때, 회색
```

ⓘ 툴팁: "이 출처는 자체 검사를 언제 했는지 알려주지 않습니다. 수집 시각은 플랫폼이 결과를 가져온 시각입니다."

## 3. 진행 방식과 테스트

독립 작업 (병렬, subagent):
1. API `exploit.go` + 단위 테스트 (httptest: KEV 정상/실패/3일 경과, EPSS 배치 100개 분할·문자열 숫자·누락 CVE·타임아웃).
2. API 계약 + 보고서 연결 (`openapi.yaml`, `go generate ./pkg/apiv1`, posture 연결 테스트).
3. 프런트 시각 표시 (두 줄, 굵게, "(검사 시각 미확인)" + 툴팁, 기존 "검사 미확인" 테스트 수정).

순차 작업 (2 이후): 4. 프런트 정렬·이유·칩 + 테스트.

마무리: ADR 0034 SEC-07 절 (VEX 미적용 명시), PR·머지·배포, 화면 확인.

완료 기준: `go test -race ./...`, `npx vitest run`, `npx tsc -b --noEmit` 통과; 배포 후 취약점 화면에 KEV/EPSS 칩과 수집 상태 탭에 KEV·EPSS 행이 보인다.
