# 0035. 성공한 이전의 원본 DB를 보존 기간과 재검증 뒤 정리한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

이전은 원본 DB를 standby로 남겨 역방향 복귀를 지원하지만 자동 만료가 없어 컴퓨팅 비용이 계속 발생했다. 시간만으로 삭제하면 새 이전·롤백, 같은 이름의 재생성 DB, 미검증 백업을 잘못 처리할 수 있다. 기존 decommission은 가역적인 중지·차단이며 삭제 예약 기능이 아니다.

## 결정

- 플랫폼이 실행 수명주기를 관리한다. `PLATFORM_SOURCE_RETENTION=24h`처럼 운영자가 보존 기간을 명시할 때 새 정방향 실행만 등록한다. 기본은 꺼짐이며 기존 성공 실행과 롤백 실행에 소급 적용하지 않는다. 엔진 Plan과 엔진 라이브러리의 계약은 바꾸지 않는다.
- 시작 전에 원본 Cluster UID와 소유 DB Pod UID, 이미 존재하면 대상 UID를 기록한다. 성공 직후 실행이 아직 두 환경을 예약하는 동안 cold 대상 UID를 연결한다. 실패하면 식별자를 추정해 보충하지 않고 정리를 보류한다.
- PostgreSQL run events에 `source_retention` 상태를 저장한다. 보존 기간은 등록 시 확정하며 기한은 실행 종료 시각 기준이다. 재시작으로 기한을 연장하거나 초기화하지 않는다. 감사 의도와 deleting 상태가 저장된 뒤에만 DELETE를 요청한다. 같은 상태를 반복 저장하지 않는다.
- 이전과 정리는 같은 PostgreSQL session advisory lock을 두 환경에 대해 정렬된 순서로 얻는다. 긴 실행이 store pool을 고갈시키지 않도록 별도 연결을 사용한다. 잠금 상실 후 재획득하며 계속하지 않는다. 각 엔진 단계 전에 연결을 확인하고, 승격 후 잠금 상실은 halted로 표시해 운영자 조정을 요구한다. 기존 엔진 파일은 덮어쓰지 않는다.
- 성공·검증 완료와 설정된 사후 관찰 통과를 요구한다. 이후 대상 UID·CNPG health·SQL primary, 원본 standby·다른 client session 0, 전체 gateway 대상 라우팅·진행 중 요청 0, 최근 30초 대상 요청 20개 이상·오류율 1% 이하·p95 800ms 이하를 재확인한다. 트래픽이 없거나 검사 불능이면 보류한다.
- 원본 Cluster에 운영자가 `hopcharlie.io/retention-allowed=true`를 설정해야 한다. `retention-hold` annotation은 보류, `retain-until`은 보존 연장에 사용한다. 요청 직전에도 이를 재확인한다.
- 대상의 완료된 CNPG Backup과 독립 복구 검증자의 ConfigMap 기록을 요구한다. 실행 ID·대상 UID·Backup UID·완료/검증 시각이 일치해야 하고 이전 종료 이후이며 24시간 이내여야 한다. 이 ConfigMap은 신뢰된 검증자/운영자의 선언이며, 컨트롤러가 직접 복구를 수행하거나 외부 백업 저장소를 검증한 증거로 바꾸지 않는다. 앱과 agent에는 쓰기 권한을 주지 않는다.
- 새 이전이 두 환경을 재사용하면 아직 시작하지 않은 예약을 취소한다. 롤백 계획이 존재하면 보류한다. 삭제 중인 원본으로 새 이전을 시작하지 않는다. 삭제 완료 원본으로 즉시 롤백하지 않으며, 이후 별도 성공한 이전으로 대상이 다시 만들어진 경우는 과거 incarnation의 삭제 기록과 구분한다.
- Cluster는 UID·resourceVersion DELETE 전제조건과 Orphan propagation을 사용한다. CR 부재 확인 뒤 기록된 원본 DB Pod만 UID·resourceVersion 조건으로 삭제한다. 남은 DB Pod가 있으면 완료로 표시하지 않는다. 같은 이름의 대체 자원은 삭제하지 않는다. PVC·Secret·Service·백업·앱 workload·namespace·Kubernetes 클러스터는 삭제하지 않는다. finalizer를 강제로 지우지 않는다.
- 읽기는 viewer, 보존 연장과 계속 보존은 operator만 허용한다. API는 보존 단축·즉시 삭제·기존 실행의 신규 등록을 제공하지 않는다. 정리 시작 이후에는 연장/취소가 불가능하다.

## 검토한 대안

- 메모리 timer: 프로세스 재시작 시 기한과 삭제 의도를 잃어 기각했다.
- 만료 시 전체 namespace 또는 PVC 삭제: 공유 자원과 복구 데이터를 파괴할 수 있어 기각했다.
- DB를 영구 유지: 롤백에는 유리하지만 컴퓨팅 비용을 회수하지 못한다.
- backup 설정만 보고 삭제: 실제 복구 검증과 같지 않아 기각했다.
- decommission에 TTL 추가: 기존 가역적 Undo 의미와 삭제의 비가역성을 섞지 않고 별도 수명주기로 둔다.

## 결과

기본 보존 기간을 운영자가 선택하고 원본 DB 컴퓨팅을 회수할 수 있다. PVC 비용과 앱·서비스·파일 저장소 비용은 남는다. API가 실행 중일 때 분 단위로 확인하며 정확한 만료 시각 삭제를 보장하지 않는다. API가 꺼져 있거나 근거가 없으면 자원이 더 오래 남는다.

기존 Recover는 단일 활성 API를 전제로 하므로 이 변경이 플랫폼 전체의 HA 운영을 보장하지 않는다. 공통 잠금은 별도 worker/프로세스 경합에 대한 방어다. 이전 버전·CLI 직접 실행·외부 관리자의 작업은 이 잠금에 참여하지 않는다. 삭제 전 검증은 분산 시스템 전체의 원자적 트랜잭션이 아니며 외부 변경을 통제해야 한다. 네트워크 불확실성에는 삭제 완료를 추정하지 않는다.

실제 CNPG operator·Kubernetes garbage collector·운영 CNI에서의 end-to-end 삭제와 독립 복구 검증 파이프라인 연결은 별도 검증이 필요하다. 단위 테스트와 로컬 PostgreSQL 잠금 검증을 그 증거로 대신하지 않는다.

참고: [Kubernetes garbage collection](https://kubernetes.io/docs/concepts/architecture/garbage-collection/), [API resourceVersion](https://kubernetes.io/docs/reference/using-api/api-concepts/), [PVC reclaim policy](https://kubernetes.io/docs/concepts/storage/persistent-volumes/).
