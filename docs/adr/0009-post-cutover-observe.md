# 0009. 전환 뒤에도 SLO를 관찰하고, 위반하면 기존 롤백 경로로 되돌린다

- 상태: 채택
- 날짜: 2026-11-02

## 상황

본선 규칙은 SLO 위반이나 health check 실패에서 자동 또는 1클릭 롤백이 실제로 동작하기를 요구한다. 지금까지 SLO 게이트는 전환 전 읽기 카나리(0005)에만 있었다. 전환이 끝나면 run은 바로 `succeeded`로 끝나서, 새 환경이 전환 직후 쓰기를 받기 시작하며 망가져도 플랫폼이 알아채지 못했다. 성공한 run을 반대 방향으로 옮기는 1클릭 롤백(`POST /v1/migrations/{runId}/rollback`)은 이미 있었다.

쓰기는 카나리로 나눌 수 없다. 새 DB는 승격 전까지 읽기 전용 복제본이고, 승격한 뒤 원본도 쓰기를 받으면 split-brain이 된다(engine ADR 0003). 그래서 쓰기는 한 번에 넘기고, 넘긴 뒤를 관찰한다.

## 결정

- 실행 시작 요청에 `observe`를 받는다(`duration` 기본 2m, `stepDuration` 기본 20s, `gate`는 카나리와 같은 기본값, `autoRollback` 기본 false). 엔진이 finalize까지 성공하면 run은 `observe` 단계로 들어간다.
- 단계마다 게이트웨이에서 그 구간의 대상 backend 통계를 읽어 카나리와 같은 판정(`evaluateGate`)을 한다. 결과는 `ok`, `violated`, `inconclusive` 세 가지다.
  - 요청이 `minRequests`보다 적거나 게이트웨이에 닿지 않으면 `inconclusive`다. 위반으로 보지 않는다. 롤백은 반대 방향 이전 한 번이라 비용이 크고, 모니터링이 끊겼다고 앱을 옮기면 안 되기 때문이다. 카나리는 반대로 "데이터 없음은 통과가 아니다"로 멈추는데, 카나리에서 멈추는 것은 비용이 없다.
  - 첫 `violated`에서 관찰을 끝낸다.
- run은 위반해도 `succeeded`로 끝난다. 데이터는 이미 옮겨졌기 때문이다. 위반 내용은 `observe.violation`과 `statusReason`에 남는다. `rolled_back`처럼 보이게 하지 않는다.
- `autoRollback`이면 run이 끝나고 저장된 뒤, 서버가 롤백 엔드포인트와 같은 함수(`startRollback`)로 이 run의 롤백을 시작한다. 실행자는 `slo-guard`이고 감사 로그에 남는다. 결과는 원래 run의 `observe.rollbackRunId` 또는 `observe.rollbackError`와 `observe_rollback` 이벤트로 남는다. 롤백 run의 `rollbackOf`는 원래 run이다.
- `autoRollback`이 아니면 위반을 기록만 하고, 운영자가 1클릭 롤백으로 결정한다.
- 운영자의 abort나 서버 종료로 관찰이 끊기면 `stoppedReason`을 남기고 `succeeded`로 끝낸다. 재시작 때 관찰 중이던 run도 `interrupted`가 아니라 `succeeded`로 바꾼다. 엔진 결과가 이미 `succeeded`라 resume할 것이 없기 때문이다.

## 검토한 대안

- 엔진에 관찰 단계를 넣기: 엔진은 데이터 이전만 결정적으로 실행한다(engine ADR 0002, 이 문서 0005). 게이트웨이 통계와 롤백 Plan 생성은 플랫폼에 있다.
- 위반 시 run 상태를 `rolled_back`이나 새 상태로 바꾸기: 콘솔이 데이터가 원본에 있다고 오해한다. 실제로 되돌리는 것은 별도 롤백 run이다.
- 관찰 중 바로 롤백 run 시작: 원래 run이 클러스터를 잡고 있어 `cluster_busy`가 된다. 끝나고 저장된 뒤에 시작한다.
- 쓰기 카나리: 위의 split-brain 때문에 하지 않는다.

## 결과

- 로컬 E2E(kind의 CNPG 두 개, 이 API와 게이트웨이, 쓰기·읽기 부하)에서 `observe`(60s, 10s, autoRollback)로 이전한 뒤 새 환경 앱을 죽였다. 다음 구간에서 오류율 67%로 위반을 판정했고, `slo-guard`가 롤백을 시작해 11초 뒤 원래 환경으로 돌아왔다. 부하 도구가 성공으로 기록한 쓰기 1729건이 모두 되돌아간 primary에 있었다. 앱이 죽어 있던 동안의 요청은 502로 실패했다.
- 관찰 시간만큼 run이 늦게 끝난다. decommission(engine ADR 0010)은 관찰이 끝난 뒤에 한다.
- `RunPhase`에 `observe`가 추가됐다. 콘솔과 CLI는 새 값을 처리해야 한다.
- 관찰은 게이트웨이 통계만 본다. 앱 내부 오류(200으로 응답하는 실패)나 지연된 배치 문제는 잡지 못한다.
