# 0029. 환경 동등성은 리소스 증거와 end-to-end 검증을 분리해 기록한다

- 상태: 제안
- 날짜: 2026-11-06
- 관련: [0022 동등성 audit 모드](0022-parity-audit-mode.md), [0025 Barman 백업](0025-app-postgres-backups.md)
- 기여: 클라우드 활용·완성도, 데모 (b)의 여러 환경 보안·거버넌스 상태 확인.

## 상황

RBAC와 Prometheus 이외 capability는 `probe_not_configured`였다. 온프렘 구축 전에 Secret·감사·백업 확인 코드를 준비할 수 있지만, Secret 존재·수집기 가용성·최근 백업을 각각 시크릿 사용·감사 전달·복구 성공으로 취급해서는 안 된다. 아직 없는 온프렘 설정이나 실제 운영 리소스 이름도 추정하지 않는다.

## 결정

서버 소유 `PLATFORM_PARITY_FILE`의 선택적 설정으로 다음 **부분 증거**를 수집한다. 정상 부분 검사는 `pass`, 불충족이 확인된 것은 `fail`, API/권한/형식/증거 부족은 `unknown`이다. 각 capability에 end-to-end 미검증 check가 남으므로 capability 전체는 `unknown` 또는 `fail`이며, 전체 동등성 통과로 만들지 않는다. 기존 `mode: audit`만 허용하고 이전을 차단하지 않는다. 롤백 면제와 전체 30초 상한도 유지한다.

| Capability | 실제 읽기 | 확인 범위 | 끝까지 확인하지 않는 것 |
|---|---|---|---|
| secret | 대상 namespace의 지정 Secret `PartialObjectMetadata` GET | 각 참조의 존재 | 값·키 목록·앱 마운트·사용 가능 여부·암호화·회전 |
| audit | 지정 namespace/Deployment GET | 현재 generation의 status에서 desired>0, available>=desired | API 감사 정책·발생 이벤트·저장/보존/검색·앱 범위 커버리지 |
| backup-restore | 대상 CNPG Cluster GET, 참조 Barman ObjectStore GET | 활성 Barman WAL archiver, `ContinuousArchiving`, 해당 server의 최근 성공 백업 시각 | 백업 객체 실재·WAL 연속성·복구 가능 구간·실제 restore·복구된 데이터 검증 |

### Secret 데이터 최소화

`envs.NewReadClients`의 metadata client는 `Accept: application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1`만 보낸다. client-go 기본 metadata client는 일반 JSON fallback을 광고하고 전체 리소스를 metadata로 바꿀 수 있으므로, 별도 transport에서 fallback을 없애고 응답 type도 검증한다. metadata를 지원하지 않으면 `unknown`이지 일반 Secret GET으로 재시도하지 않는다. 정상 metadata 응답 본문은 최대 1MiB로 제한한다.

이는 API 서버가 content negotiation을 지킨다는 전제다. 비준수 서버가 보낸 전체 Secret 응답은 거부하지만 이미 네트워크로 받은 내용을 없던 것으로 만들 수는 없다. metadata의 annotation(예: last-applied)에 민감한 값이 있을 수도 있다. 응답 본문·annotation·리소스 실제 이름·SDK 오류는 보고서에 넣지 않고 서버 정책의 비민감 check ID와 고정 code만 기록한다. 이 클라이언트에 HTTP body 디버그 로깅을 켜지 않는다.

metadata 요청도 RBAC상 `get secrets`이며 "metadata만 허용하는 RBAC 권한"은 아니다. 최소 권한은 namespace/`resourceNames`로 좁힌다. Secret의 공급자가 Vault/SOPS/Secrets Manager인지, 앱이 실제 값을 사용할 수 있는지는 이 검사로 판단하지 않는다.

### 감사

운영자가 **실제 감사 수집기라고 확인한 Deployment**를 지정해야 한다. 임의의 건강한 Deployment를 넣어도 감사가 증명되지는 않는다. generation이 없거나 status가 구버전이면 `unknown`, 현재 status에서 scaled-to-zero/가용 replica 부족이면 `fail`이다. 이벤트 발생·로그 검색을 하지 않으므로 `audit-delivery=unknown(audit_delivery_not_verified)`를 항상 남긴다.

EKS/GKE 관리형 감사 서비스나 DaemonSet 수집기는 이 프로브 대상이 아니다. k3s의 노드 audit policy/출력 설정도 Kubernetes API만으로 알아내지 않는다. 해당 방식만 있는 환경에서는 collector 설정을 생략하고 `probe_not_configured`를 유지한다. 플랫폼 자신의 감사 로그를 대상 클러스터의 감사 전달 증거로 재사용하지 않는다.

### 백업

ADR 0025의 관측 경로와 동일하게 `spec.plugins`에서 활성 `barman-cloud.cloudnative-pg.io`, `isWALArchiver: true`, `parameters.barmanObjectName`을 읽는다. ObjectStore는 대상 Cluster와 같은 namespace에서 읽는다. `parameters.serverName`이 있으면 그 값, 없으면 Cluster 이름의 `status.serverRecoveryWindow`를 선택한다. 다른 server의 시각으로 대신하지 않는다.

- 설정된 Barman이 없거나 다른 백업 제공자이면 `unknown`: 미지원이 백업 부재의 증거는 아니다.
- WAL 조건 `True`/`False`는 각각 reported healthy/failed, 조건 없음은 `unknown`이다. archiver가 없는 Cluster의 `ContinuousArchiving=True`만으로 통과시키지 않는다.
- 성공 시각이 유효하고 미래가 아니며 `maxAge` 이내일 때만 최근 백업 check를 통과시킨다. 과거지만 한도를 넘으면 `fail`, 누락/잘못된 값/미래 시각은 `unknown`이다.
- `maxAge`는 정책의 필수 값(1m~720h)이지 추정한 RPO가 아니다. 새 DB에는 아직 백업이 없을 수 있으므로 모르는 것을 실패한 restore로 표현하지 않는다.
- `restore=unknown(restore_not_verified)`는 항상 남는다. 복구 Job/DB를 생성하지 않고 데이터를 다른 환경으로 복사하지 않는다.

조건은 컨트롤러가 보고한 현재 조회값일 뿐이며 별도의 상태 freshness나 DB UID/백업 lineage 검증은 없다. 최근 성공 백업 하나가 지속적인 백업 성공이나 서비스 복구를 보장하지 않는다.

## 설정과 연결

[3환경 템플릿](../../examples/parity-three-environments.yaml)은 EKS·GKE·온프렘 모두 별도 리소스 참조를 갖는다. 모든 `replace-me-*`는 의도적인 placeholder이며 운영 baseline이 아니다. 공통 앱 namespace는 기존처럼 Plan 대상 namespace에 맞춘다. 환경별 namespace가 다르면 이 설정 형식으로 같은 앱을 묶을 수 없으므로 별도 설계가 필요하다.

- 앱의 `environments.<id>.secrets`: `{id, name}` 목록. namespace는 앱 namespace다.
- 앱의 `environments.<id>.backup`: `{cluster, maxAge}`. Plan의 `target.cluster`와 서버 정책의 cluster가 일치해야 검사한다. 다르거나 Plan에 없으면 다른 DB의 정상 백업을 재사용하지 않고 `backup_target_mismatch`로 `unknown`이다. DSN이 실제로 그 DB에 연결되는지의 독립 검증은 아니다.
- 환경의 `auditCollector`: `{namespace, deployment}`. 그 환경에서 검사하는 모든 앱에 같은 수집기 가용성 증거를 사용하며 앱별 감사 커버리지를 증명하지 않는다.
- 이름/환경/중복 ID/기간/unknown field를 파싱 시 검증한다. `secret-usability`는 예약 check ID다.
- 설정 없음은 `probe_not_configured`; 설정은 있지만 kube context/클라이언트를 만들 수 없으면 `probe_failed`. API 인증/권한/접속 실패는 `unknown`. 명시한 필수 리소스에 API가 404를 반환하면 `resource_missing`으로 `fail`이다.
- `ResourceFactory`는 **등록된 환경의 kubeconfig/context와 플랫폼 자격 증명**만 사용한다. Plan의 kubeconfig/context, AI가 제출한 검증 결과로 바꾸지 않는다.

### infra 후속 작업 (이번 변경에서 배포하지 않음)

1. 운영자가 각 환경의 실제 namespace, Secret, DB, collector, Prometheus endpoint와 job을 확인해 baseline을 리뷰한다. 미구축 온프렘은 연결됐다고 표시하지 않는다.
2. 플랫폼 실행 계정에 필요한 읽기 권한을 환경별로 제한해서 부여한다.
   - 앱 namespace: 명시한 `secrets` GET, `postgresql.cnpg.io/clusters` GET, `barmancloud.cnpg.io/objectstores` GET.
   - 수집기 namespace: 명시한 `apps/deployments` GET.
   - 기존 identity 프로브에는 `authorization.k8s.io/subjectaccessreviews` CREATE가 별도로 필요하다.
   - list/create/patch Secret 권한은 새 프로브에 필요하지 않다. kubeconfig 안의 실제 권한은 별도 리뷰 대상이다.
3. infra의 기존 설정 전달/마운트 방식에 맞춰 검토된 파일을 플랫폼 컨테이너에 read-only로 마운트하고 `PLATFORM_PARITY_FILE`을 **컨테이너 내부 파일 경로**로 설정한다. 이 레포 예시만 고쳐서는 운영 설정이 바뀌지 않는다.
4. 변경 이미지를 승인 후 배포한다. 실제 새 run의 `parity` 감사 기록에서 부분 check와 unknown을 확인한다. 전체 `passed=true`를 기대하지 않는다.

이 PR은 infra 파일·실행 환경·IAM/RBAC·클러스터를 바꾸지 않으며, 운영에서 parity가 켜져 있다고 주장하지 않는다.

## 대안과 트레이드오프

- **설정 존재를 전체 pass로 처리:** 빠르지만 실제 전달/복구 능력과 다르므로 배제한다.
- **Secret 전체 GET 후 이름만 출력:** 보고서에 값이 없어도 클라이언트가 값을 받으므로 배제한다.
- **k3s 노드 설정/클라우드 감사 API까지 자동 탐색:** 노드 접근·클라우드 권한·보존 정책별 어댑터가 필요하다. 이번에는 보수적인 부분 증거에 한정한다.
- **이전 시작 때마다 실제 restore/이벤트 주입:** 더 강한 증거지만 쓰기 작업·비용·데이터 레지던시와 정리가 필요하다. 별도 승인된 샌드박스 검증으로 설계해야 한다.

## 검증 범위

테스트는 합성 Kubernetes HTTP 응답과 실제 client-go 클라이언트를 사용한다. 메타데이터 전용 협상/전체 객체 거부, 권한 거부·404·취소, 수집기 generation/가용성, 백업 server 선택/기간/잘못된 시각, 설정 검증과 누락 context의 unknown을 확인한다. 실제 EKS·GKE·k3s의 동등성이나 audit 전달/restore를 증명하는 테스트가 아니다. 실행 결과는 워크스페이스 `migration-engine/docs/test-records/`에 별도로 기록한다.
