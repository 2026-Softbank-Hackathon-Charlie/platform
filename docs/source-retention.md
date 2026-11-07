# 이전 후 원본 DB 자동 정리

운영자가 `PLATFORM_SOURCE_RETENTION=24h`를 설정하면 이후 시작하는 정방향 이전에만 정리 예약을 남긴다. 기본은 꺼짐이다. 지원 기간은 1분~720시간이며 운영 환경에는 복귀 정책에 맞는 충분한 기간을 지정한다. 이 문서의 명령은 적용 예시이며 이번 작업에서 운영 환경에 실행하지 않았다.

이전·검증·선택한 사후 관찰이 끝난 시각부터 보존 기간을 센다. 기한 이후에도 대상 health·현재 SLO·라우팅·원본 사용 여부·백업/복구 근거를 확인해야 정리한다. 조건이 부족하면 `blocked`이며 다음 분에 다시 확인한다. 시간 경과만으로 통과시키지 않는다.

## 사전 설정

1. 모든 API 인스턴스를 공통 cluster lease를 사용하는 버전으로 갱신한 뒤 기능을 켠다. 별도 CLI·GitOps·관리자가 같은 DB를 변경하지 않도록 운영 절차를 정한다. 외부 조정자는 PostgreSQL 잠금을 사용하지 않는다.
2. 앱 전용 원본 DB에만 `hopcharlie.io/retention-allowed: "true"` annotation을 설정한다. 공유 DB에는 지정하지 않는다. 이 annotation은 DELETE 직전에도 확인한다.
3. 각 등록 환경의 플랫폼 신원에는 해당 namespace의 CNPG Cluster get/delete, Pod get/list/delete, 대상 Backup·ConfigMap get 권한이 필요하다. PVC delete와 namespace delete 권한은 필요하지 않다. CNPG 생성/승격의 기존 권한은 별도다.
4. 대상 DB에서 백업하고 독립적으로 복구 검증을 완료한다. 신뢰된 복구 검증자가 대상 DB namespace의 `hopcharlie-retention-recovery` ConfigMap에 아래 키를 기록한다. 앱/agent에는 이 ConfigMap의 생성·수정 권한을 주지 않는다.

| ConfigMap data 키 | 필요한 값 |
| --- | --- |
| runId | 원본 정리 예약이 속한 migration run ID |
| targetUID | 이번 이전의 대상 CNPG Cluster UID |
| backupName / backupUID | 실제 복구에 사용한 대상 CNPG Backup 이름과 UID |
| restoreRunId | 독립 복구 검증 실행의 추적 ID |
| verifiedAt | 복구 검증 완료 시각, RFC3339 |
| result | 실제 독립 검증이 성공했을 때만 `passed` |

대상 Backup CR은 `spec.cluster.name`이 대상과 일치하고 `status.phase=completed`여야 한다. `status.stoppedAt`과 검증 시각은 이전 종료 이후이며 24시간 이내여야 한다. Backup 완료 시각은 복구 검증 시각보다 늦을 수 없다. 플러그인 ObjectStore의 성공 시각만 존재하는 구성은 이 조건을 충족하지 않는다.

컨트롤러는 복구를 직접 실행하거나 위 기록을 자동 생성하지 않는다. 완료된 Backup CR 및 검증자 선언을 읽어 범위·시각·식별자를 대조한다. 실제 백업 bytes의 가용성과 복구 성공은 독립 검증자의 책임이며, 근거가 없으면 삭제하지 않는다.

## 보존 연장과 중단

콘솔의 종료된 이전 리포트에서 정리 예정 시각과 보류 상태를 볼 수 있다. 운영자는 **보존 24시간 연장**, **원본 계속 보존**을 사용할 수 있다. 계속 보존은 이 실행의 자동 정리를 취소하며 자동 재등록하지 않는다. 기간을 줄이거나 즉시 삭제하는 API는 없다.

- `GET /v1/migrations/{runId}/retention`: viewer 이상. `{}`는 예약 없음이다.
- `POST /v1/migrations/{runId}/retention`, `{"retainUntil":"<RFC3339>"}`: 기존 기한보다 늦고 현재부터 30일 이내인 기한으로 연장한다.
- 같은 POST, `{"keep":true}`: 자동 정리를 취소한다. 두 필드를 함께 보내면 거부한다.
- `deleting` 또는 `deleted` 이후에는 연장/취소와 즉시 역방향 롤백이 거부된다. 삭제 완료 원본을 다시 쓰려면 별도 복구·생성 과정을 거쳐야 한다.

Kubernetes 운영자는 원본 Cluster의 `hopcharlie.io/retention-hold`에 비어 있지 않은 값을 설정하거나 `hopcharlie.io/retain-until`에 RFC3339 시각을 설정해 보류할 수도 있다. `retention-allowed`를 제거하면 삭제를 보류한다. 이미 CR 삭제가 요청된 뒤에는 annotation으로 되돌릴 수 없다.

## 삭제 범위와 비용

원본 CNPG Cluster CR을 Orphan 방식으로 제거한 뒤, 기록된 소유 DB Pod를 개별 확인해 제거한다. 원본 CR과 DB Pod가 모두 사라져야 `deleted`로 기록한다. UID 또는 resourceVersion이 달라지거나 확인하지 못하면 삭제 완료를 추정하지 않는다.

PVC·Secret·Service·백업·앱 Deployment/StatefulSet·파일 bucket·namespace와 공유 Kubernetes 클러스터는 자동 삭제하지 않는다. 기존 decommission의 중지/차단과도 별개다. PVC 및 cloud 서비스 비용은 계속 발생하며, 별도 보존/정리 정책이 필요하다. 노드 수나 cloud VM 비용이 자동으로 줄어든다고 보장하지 않는다.

## 운영과 장애

예약과 상태는 run event, 전이는 감사 로그에 남는다. API 재시작은 기한을 초기화하지 않는다. 삭제 요청 전 `deleting`을 저장하고, 재시작 후에는 같은 UID만 확인한다. 삭제 요청의 응답이 불명확하면 `deletion_unconfirmed`로 유지한다. 동작을 끄면 신규 등록과 background 정리는 멈추지만 기존 삭제 의도가 없었던 것으로 바뀌지는 않는다.

별도 잠금 연결이 실행당 하나 추가된다. PostgreSQL 연결 용량을 고려해야 한다. 풀 연결을 장기 점유하지 않는다. 백업·복구 검증에 드는 저장소/컴퓨팅 비용은 기존 백업 정책에 따르며 이 컨트롤러는 새 cloud 리소스를 만들지 않는다.

Kubernetes의 실제 orphan GC와 CNPG operator 동작은 운영 버전과 admission 정책에서 별도 리허설해야 한다. 운영 자동 삭제를 활성화하기 전에 해당 검증과 복구 파이프라인 연결이 필요하다.
