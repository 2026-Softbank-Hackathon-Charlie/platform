# 0011. Preflight는 플랫폼의 독립 패키지로 시작하고 검증되지 않은 항목은 통과시키지 않는다

- 상태: 채택 — 독립 코어 범위. 운영 실행 게이트 연결은 후속 작업
- 날짜: 2026-11-03

## Context

이전 전에 대상 환경이 앱의 보안·운영 요구를 만족하는지(동등성), 데이터가 목적 국가로 이동해도 되는지(레지던시)를 판단할 필요가 있다. 본선의 클라우드 활용·완성도와 데모 (a) 안전한 이전, (b) 거버넌스에 기여한다. 초기 구현을 바로 운영 실행 경로에 연결하면 기존 승인·재개·롤백의 안전 경계에 영향을 줄 수 있다.

현재 프로젝트는 모노레포가 아니다. 플랫폼은 승인·실행·환경·감사 로그를 이미 소유하고, 별도 migration-engine은 CNPG 물리 복제와 승격을 실행한다. `verify.tables`는 검증 범위이며 복제 범위를 제한하지 않는다. 원안의 logical Publication 테이블 필터를 현재 엔진에 있는 기능처럼 사용할 수 없다.

원본에서 발견한 기능만 대상에 요구하면 원본에 빠진 보안까지 그대로 복제한다. 조회 실패나 미구현 검사를 성공으로 표시하면 No Mock 및 fail-closed 원칙에도 맞지 않는다.

## Decision

1. `internal/preflight`를 기존 platform Go 모듈 안에 둔다. 새 서비스/레포/go.mod를 만들지 않는다. `plans`, `runs`, engine, store에 의존하지 않는 작은 코어다. 이번에는 운영 API·DB 스키마·승인·이전 동작을 변경하지 않는다.
2. 레지던시는 내장된 실제 OPA/Rego로 판단한다. SDK 버전은 고정하고, 국가 허용 규칙을 Go와 Rego에 중복 작성하지 않는다. Go는 입력·정책 검증과 안전한 결과 해석을 담당한다. 외부 Rego 업로드나 정책 서버는 없다.
3. 신뢰된 서버 catalog에서 데이터 분류를, 환경 레지스트리에서 국가를 가져온다. 요청에는 label/country를 넣지 않는다. APAC 국가 목록은 명시적 조직 설정이며 미분류 데이터/미등록 국가/불완전 inventory는 거부한다.
4. 첫 지원 경로는 전체 클러스터 `physical-replication`이다. 모든 DB/스키마의 데이터 제약을 만족해야 허용한다. catalog와 inventory는 서로 누락이 없어야 한다. logical/테이블 선택·백업/restore·오브젝트 목적지 해석은 미지원이다.
5. 평가기 생성 시 설정을 복사해 불변 snapshot을 만들고, 내장 Rego와 정규화한 전체 설정을 SHA-256 digest로 묶는다. 정책 거부와 평가 오류는 구분하지만 둘 다 `Allowed=false`다. 이 digest는 실행 Plan/첨부/승인 digest가 아니다.
6. 동등성 필수 집합은 `원본에서 발견한 기능 ∪ 조직 최소 기준`이다. 분류는 identity, secret, audit, metrics-alerting, backup-restore의 다섯 가지로 고정한다. required probe 없음/오류/unknown/not_applicable/다른 대상 응답은 전체를 통과시키지 않는다. 원본 발견이 불완전하면 검사를 실행하지 않고 unknown으로 표시한다.
7. 첫 실제 probe는 Kubernetes SubjectAccessReview와 Prometheus 조회다. RBAC는 앱 SA의 실제 사용자/그룹으로 필수 동작 허용과 불필요 동작 거부를 모두 검사한다. Prometheus는 `up`뿐 아니라 `timestamp(up)`으로 실제 scrape 시각을 검사한다. instant query의 외부 timestamp를 scrape 시각으로 오인하지 않는다.
8. 알림 전달은 아직 구현하지 않았으므로 scrape가 정상이어도 metrics-alerting은 unknown이다. 감사 로그·Secret 주입·실제 백업 복구도 미연결 시 unknown이며 성공하는 stub을 두지 않는다. live source discovery와 실제 데이터 inventory collector도 후속 작업이다.
9. client/endpoint/요구사항은 서버가 대상 환경에 묶는다. probe에는 임의 Plan URL이나 자격증명을 넘기지 않는다. 리다이렉트·응답 크기·실행 시간은 제한하고 SDK 오류·HTTP body·비밀값을 보고서에 노출하지 않는다.

## Alternatives

| 대안 | 판단 |
|---|---|
| migration-engine 내부에 parity/정책 판단 추가 | 복제·전환 로직과 조직 정책이 결합한다. 기각 |
| 별도 preflight 레포 + 서비스 | 독립 배포 장점은 있지만 인증·상태 동기화·버전 관리가 추가된다. 현재는 기각 |
| Go if문과 Rego를 함께 사용 | 정책이 두 곳에 존재해 불일치 가능성이 생긴다. 국가 판단은 Rego 한 곳으로 제한 |
| 한 번에 5종 live probe와 실행 gate까지 연결 | 검증하기 전에 운영 경로 변경 범위가 너무 커진다. 작은 단계로 분리 |
| 미구현/권한 부족 검사를 skip하여 성공 | 실패를 숨기고 보안 상태를 허위로 표현한다. 기각 |

## Consequences

- 장점: 기존 실행 동작을 보존하면서 정책·검사 코드를 독립 테스트할 수 있다. source→target 방향에 대한 공급자별 분기가 없다. 새 대상은 적합한 client/probe 연결로 확장한다.
- 비용: OPA 의존성과 최소 필요 transitive dependency 업데이트가 추가된다. 플랫폼 전체 회귀 테스트로 영향 범위를 확인해야 한다.
- 제한: 코어의 입력 자체가 신뢰된다는 전제가 있다. `Complete=true`는 탐색 증명이 아니며, inventory와 catalog가 함께 빠뜨린 데이터는 잡지 못한다. 심사/데모에서 live 수집을 했다고 주장하지 않는다.
- 제한: RBAC 성공은 AWS IAM·실제 앱 동작 성공을 뜻하지 않는다. Prometheus 성공은 반환된 타깃의 scrape 상태만 증명하며 기대 replica 수·알림 전달·사용자 SLO를 증명하지 않는다.
- 제한: API에 연결되지 않았으므로 현재 서비스에서 이전을 차단하지 않는다. 테스트 fixture의 통과는 실제 환경 동등성의 증거가 아니다.
- 후속: 신뢰된 collector·운영 catalog, 미구현 probes, 검사 저장/API, 실행 digest·리허설·승인 연결과 증거 TTL을 추가한다. 최종 gate는 시작·재개·역방향 롤백·자동 롤백에 적용하되, fence/승격 중 안전 복구를 훼손하지 않도록 설계한다.
- 후속: 새 replica 생성 전에도 정책 판단을 수행해야 한다. 이미 존재하는 replica는 이전 버튼을 누르기 전에 데이터가 이동했을 수 있다. restore 검증도 데이터 이동이므로 레지던시를 먼저 확인한다.
