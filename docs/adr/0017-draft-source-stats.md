# 0017. Plan 초안이 요청하면 원본 실측값을 돌려주고, 대상 설정이 원본보다 작으면 시작을 막는다

- 상태: 채택
- 날짜: 2026-11-03
- 관련: [0006 Plan 도구](0006-plan-tools.md), [0014 용량 사전 검사](0014-capacity-preflight.md), 이슈 #46

## Context

AI 플래너(ai-sandbox)는 승인 전에 Plan과 대상 CNPG 매니페스트를 쓴다. 대상 사양을 원본에 맞추려면 원본의 세션 수, `max_connections`, `max_worker_processes`, 크기, WAL 속도가 필요하다. 대상은 원본을 따르는 hot standby라서, `max_connections`와 `max_worker_processes`가 원본보다 작으면 뜨지 않는다. 0014의 용량 검사도 `max_connections` ≥ 세션 × 1.2를 본다.

지금 ai-sandbox는 이 값을 얻으려고 원본 DB에 직접 붙는다(ai-sandbox#23). 그래서 AI 실행기에 운영 DB 비밀번호와 tailnet 접속이 필요하고, 측정 쿼리와 기준을 ai-sandbox가 복사해 갖고 있어 플랫폼과 어긋날 수 있다. 플랫폼은 이미 비밀번호와 DB 접근 경로를 갖고 있다.

## Decision

1. `POST /v1/plans/draft` 요청에 `sourceStats: true`를 주면, 플랫폼이 초안의 원본 DB를 측정해 `PlanDraftResult`로 응답한다. Plan은 `plan` 아래에, 숫자는 `sourceStats` 아래에 둔다.
   - `sessions`, `maxConnections`, `maxWorkerProcesses`(필수), `bytes`, `walBytesPerSecond`(측정 못 하면 없음), `windowSeconds`, `measuredAt`
2. 측정은 0014의 `capacity.Prober`가 하는 쿼리 그대로다(`MeasureSource`). 그래서 이 숫자로 정한 대상은 시작할 때의 용량 검사와 같은 기준으로 판단된다. `max_worker_processes`를 측정 항목에 더했다.
3. 측정하지 못하면 `sourceStats`를 비우고 `sourceStatsReason: not_measured`만 준다. 비밀번호 변수가 없거나, DB에 닿지 않거나, 쿼리가 실패한 경우를 구분하지 않는다. 비밀번호 변수가 없으면 접속을 시도하지 않는다.
4. 응답에는 숫자만 담는다. 주소, 비밀번호, DB 오류 원문은 담지 않는다. Plan의 DSN은 지금처럼 `${VAR}` 그대로다.
5. 플래그가 없으면 응답은 지금처럼 Plan 그 자체다. 측정도 하지 않는다.
6. 감사 기록에는 측정 여부(`sourceStats: true/false`)만 남긴다.

## 검토한 대안

- **응답을 항상 감싸기**(`{plan, sourceStats}`): 응답 모양이 하나라 깔끔하다. 하지만 지금 초안을 쓰는 cli, frontend, ai-sandbox가 한꺼번에 깨지고, 측정 창 때문에 모든 초안 요청이 5초씩 느려진다.
- **Plan 옆에 필드 추가**: 엔진 Plan 파서가 모르는 필드를 거부해서, 초안을 그대로 `POST /v1/plans`로 제출할 수 없게 된다.
- **별도 읽기 전용 엔드포인트**(`GET .../capacity`): 타입이 깔끔하지만 호출이 하나 더 늘고, 이슈에서 초안 응답을 선호했다.
- **ai-sandbox가 계속 직접 측정**: 운영 DB 비밀번호가 AI 실행기에 남는다.

## Consequences

- ai-sandbox는 `sourceStats: true`로 초안을 받아 dbstats를 지울 수 있다. 운영 DB 비밀번호가 실행기에서 빠진다.
- 플래그를 쓴 초안 요청은 측정 창(약 5초)만큼 느리다.
- 200 응답이 요청에 따라 두 모양이다. OpenAPI는 `oneOf`로 적고, Go 생성 타입은 `json.RawMessage`로 두고 호출자가 `EnginePlan` 또는 `PlanDraftResult`로 읽는다. Go 클라이언트는 `DraftPlan`과 `DraftPlanWithSourceStats`로 나눴다.
- 시작 전 용량 검사(0014)에 `standby_settings`를 더한다. 대상의 `max_connections`와 `max_worker_processes`가 원본 값보다 작으면 실패(`target_below_source`)다. PostgreSQL은 hot standby의 이 두 값이 primary보다 작으면 standby를 띄우지 않는다. 세션 × 1.2를 보는 `connections`와는 따로 판정한다.
  - 대상 값은 실행 중인 대상에서 읽고, 없으면 매니페스트의 `spec.postgresql.parameters`에서 읽는다. 매니페스트에 없으면 기본값을 추정하지 않고 그 항목을 건너뛴다. 두 항목 모두 비교하지 못하면 `unknown`이고 막지 않는다(0014의 원칙).
  - 그래서 AI가 매니페스트에 두 값을 적어야 검사가 실제로 동작한다. 초안의 `sourceStats`가 그 값을 준다.
  - 콘솔은 새 검사 ID(`standby_settings`)의 이름과 설명을 추가해야 한다.
- 확인: 실제 PostgreSQL 17에 `MeasureSource`를 돌려 모든 값이 채워지는 것과, 닿지 않는 DB에서 `ok=false`인 것을 봤다. API는 가짜 측정기로 비밀번호 변수 없음, 측정 성공, 측정 실패, 플래그 없음, 측정기 없음과 응답에 비밀번호가 없는 것을 테스트했다.
