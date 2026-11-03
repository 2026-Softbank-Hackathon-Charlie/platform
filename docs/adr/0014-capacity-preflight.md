# 0014. 이전 전에 원본·대상을 실측해 명백한 용량 실패만 막는다

- 상태: 채택
- 날짜: 2026-11-03
- 관련: [0013 레지던시 시작 게이트](0013-residency-start-gate.md)

## Context

이전의 대역폭과 접속 수는 미리 정하기 어렵다. 지금은 문제가 생긴 뒤에야 대응한다.

- **복제가 따라잡지 못하는 경우**: 엔진이 `catchupTimeout`(기본 10분)을 기다린 뒤 실패한다.
- **대상 DB 접속 한도가 부족한 경우**: 전환 직후 접속 거부가 나고, observe가 롤백한다.

두 경우 모두 이전을 시작하기 전에 실제 숫자를 보면 예측할 수 있다.

## Decision

1. **측정**(`internal/preflight/capacity`)은 레지던시와 같은 `plans.Service.Reserve`에서 한다. 순서는 manifest를 파일로 쓴 다음이다. 확장된 DSN으로 읽기 전용 세션을 열고, 다음 값을 5초 측정 창 동안 읽는다.
   - 원본: 전체 크기, WAL 생성 속도, client backend 수, `max_connections`
   - 대상: `max_connections`. 대상이 아직 없으면 manifest의 `spec.postgresql.parameters.max_connections`를 쓰고, 기본값을 추정하지 않는다.
   - 대상이 streaming replica이면 측정 창 앞뒤의 지연(원본 `pg_current_wal_lsn` − 대상 `pg_last_wal_replay_lsn`). 엔진의 `pg.Lag`와 같은 방식이며 app 계정으로 읽힌다.
2. **판정**(순수 함수 `Evaluate`)
   - `connections`: 대상 `max_connections` < ⌈원본 접속 수 × 1.2⌉이면 실패
   - `catchup`: 지연이 `maxLagBytes`보다 큰데 줄지 않거나, 줄어드는 속도로 계산한 도달 시간이 `catchupTimeout`을 넘으면 실패. 엔진이 실제로 실패하는 조건을 그대로 예측한다.
   - `initial_copy`: 원본 크기와 변경 속도. 정보로만 제공한다.
3. **측정하지 못한 값은 `unknown`이고 막지 않는다.** 측정 자체가 오류여도 막지 않는다. 엔진의 지연 기준과 observe 롤백이 계속 안전망 역할을 한다. 측정해서 명백히 실패하는 경우만 409 `capacity_insufficient`로 막고, Plan은 `approved`로 되돌린다.
4. **롤백은 면제한다.** 레지던시와 같은 이유다. 레지던시에서 거부된 Plan은 측정하지 않는다.
5. **`PLATFORM_CAPACITY_GATE=on`일 때만 켠다(opt-in).** 시작할 때마다 몇 초가 더 걸리기 때문이다.
6. **응답·감사 기록에는 측정 숫자와 고정 코드만 남긴다.** DSN, 호스트, 오류 원문은 남기지 않는다.

## Alternatives

| 대안 | 판단 |
|---|---|
| `pg_stat_replication`으로 전송량 측정 | 전송량 칼럼은 슈퍼유저나 `pg_read_all_stats` 권한이 있어야 보인다. app 계정으로 동작하는 엔진 방식(지연 추세)을 택함 |
| "전송 속도 > 변경 속도 × 2" 규칙 | 이미 따라잡은 replica에서는 회선의 최대 속도를 알 수 없다. 엔진의 실패 조건(도달 시간 대 `catchupTimeout`)을 직접 예측하는 쪽이 정확하다 |
| 측정 실패도 차단(fail-closed) | 측정 대상이 아직 없는 정상 이전(새 대상)까지 막는다. 기존 안전장치가 있으므로 기각 |
| 기본으로 항상 켬 | 기존 데모·테스트의 시작 시간이 늘어난다. opt-in으로 결정 |

## Consequences

- **효과**: 접속 한도 부족과 따라잡기 불가를 이전 전에 숫자로 보여주고 막는다.
- **한계**
  - 접속 수는 지금 이 순간의 값이다. 7일 피크(Prometheus)는 2차 작업이다.
  - 5초 측정 창은 순간적인 부하 변동에 흔들릴 수 있다.
  - 대상이 없으면 두 환경 사이의 전송 속도를 몰라 초기 복사 시간을 예측하지 못한다.
- **검증 범위**: 원본 측정은 실제 PostgreSQL로 검증했다. replica 지연 추세는 단일 PostgreSQL로 재현할 수 없어 판정 함수 테스트로만 검증했다.
- **후속 작업**
  - 7일 피크 접속 수
  - 과거 run으로 처리량 추정
  - 승인 화면 미리보기 API와 UI
