# 0022. 동등성 검사는 먼저 기록 전용으로 시작 경로에 연결한다

- 상태: 제안
- 날짜: 2026-11-04

## 상황

`parity.Evaluate`와 프로브 두 개(`probes.RBAC`, `probes.Prometheus`)는 [ADR 0011](0011-preflight-core.md) 때부터 있었지만 어느 실행 경로에서도 부르지 않았다. 이전한 앱이 대상에서 원본과 같은 권한과 모니터링을 받는지는 아무도 확인하지 않았다.

바로 게이트로 걸 수는 없다.

- **원본 발견이 없다.** 원본 앱이 실제로 무엇을 쓰는지 찾아내는 수집기가 없다.
- **프로브가 두 개뿐이다.** secret, audit, backup-restore 프로브가 없어서, 필수로 두면 항상 `unknown`이고 `Evaluate`는 통과하지 않는다.
- **알림 경로 검증이 없다.** Prometheus 프로브가 scrape를 확인해도 metrics-alerting은 `unknown`이다.

게이트로 걸면 모든 이전이 막힌다. 반대로 필수 항목을 줄이면 확인하지 않은 것을 통과로 보이게 만든다.

## 결정

1. **`plans.Service.Reserve`의 마지막 단계에서 parity를 실행하고, 결과를 감사 로그에만 남긴다.** 레지던시, 버전, 용량을 모두 통과해 실제로 시작하는 run에만 실행한다. `checkParity`는 오류를 돌려주지 않는다. 결과가 `passed=false`여도, 검사 자체가 실패해도 시작한다. 롤백은 다른 게이트처럼 면제한다.
2. **설정은 서버가 가진다**(`PLATFORM_PARITY_FILE`). `mode`는 `audit`만 받는다. 차단 모드를 설정 하나로 켤 수 없도록, 지금은 그 값을 아예 거부한다.
   - 환경마다 Prometheus 주소를 둔다.
   - 앱마다 namespace(Plan의 대상 namespace와 대조), ServiceAccount, Prometheus job, RBAC 규칙을 둔다.
   - RBAC 규칙은 허용과 거부를 각각 하나 이상 둬야 하고, `allowed`는 필수다. 빠뜨린 값이 "거부"로 읽히지 않게 하려는 것이다.
3. **다섯 capability를 모두 필수로 요청한다.** 원본 발견이 없어도 필수 항목이 빠질 수 없으므로 baseline을 complete로 둔다. 프로브가 없는 세 항목은 `probe_not_configured`로 정직하게 남고, 보고서는 통과하지 않는다. 이 상태가 기록 전용이어야 하는 이유다.
4. **실제 값으로 검사한다.**
   - identity: 대상 클러스터의 kubeconfig context(환경 설정)로 SubjectAccessReview를 만든다. 대상은 앱 ServiceAccount다.
   - metrics: 대상 Prometheus에 `up{namespace,job}`와 `timestamp(up)`를 묻는다.
   - 만들 수 없는 프로브(예: kube context 없음)는 "설정되지 않음"이 아니라 `probe_failed`로 남긴다.
5. **감사 로그** `parity`: `{outcome, mode: audit, passed, results}`. `results`는 capability마다 한 줄이다. 예: `identity fail rbac_mismatch: read-own-config=pass(authorization_matches), list-pods=fail(authorization_mismatch)`. 프로브는 SDK 오류나 응답 원문을 결과에 넣지 않는다.
6. 검사 전체는 30초를 넘지 않는다.

## 결과

- 이전을 시작할 때마다 대상의 권한과 scrape 상태가 실제 값으로 감사 로그에 남는다. 권한이 모자라거나 남는 대상, scrape 대상이 내려간 대상을 시작 시점에 알 수 있다.
- 시작이 최대 30초 늦어질 수 있다(보통 1초 미만).
- 플랫폼의 kube 자격 증명이 대상 클러스터에서 SubjectAccessReview를 만들 수 있어야 한다. 없으면 identity는 `unknown`(`rbac_incomplete` 또는 `probe_failed`)으로 기록된다.

## 2026-11-06 보완

[ADR 0029](0029-parity-resource-evidence.md)에서 Secret 메타데이터, 감사 수집기 Deployment 가용성, Barman 백업 상태의 선택적 프로브를 추가했다. 위의 "프로브가 없다"는 도입 당시 상황이다. 설정이 없으면 계속 `probe_not_configured`이며, 부분 증거가 정상이어도 시크릿 사용·감사 전달·복구 시험은 `unknown`으로 남는다. 기록 전용 결정은 유지한다.

## 차단 모드로 가려면

- secret, audit, backup-restore 프로브
- 알림 경로 검증(테스트 알림이 Alertmanager에 도착하는지)
- 원본 dependency 발견, 또는 조직 최소 기준을 capability별로 고르는 설정
- 승인 화면에 보고서 표시(승인자가 보기 전에는 막지 않는 편이 맞다)
