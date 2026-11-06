# 0033. 동등성 증거의 수명과 승인 대상을 묶고 앱 재시도 보장은 실제 어댑터별로 구분한다

- 상태: 채택
- 날짜: 2026-11-06
- 관련: platform #91, migration-engine #19, ADR 0022·0028·0029

## 상황

프로브 관측 시각이 존재해도 오래된 결과를 다시 돌려줄 수 있고, 원본 자동 발견 없이 Complete=true만 기록하면 원본 요구사항을 확인했다는 오해가 생긴다. 앱 예약 중복 차단과 응답 유실 후 동일 결과 재전달도 다른 계약이다.

## 결정

- 기존 audit-only와 역방향 복구 면제를 유지한다. 실패·unknown 증거는 보고하되 새 차단을 몰래 도입하지 않는다.
- 서버 소유 parity 파일의 앱별 baselines에 원본 환경, 검토 revision, reviewedAt, expiresAt을 둔다. 최대 검토 기간은 30일이다. 기존 다섯 capability는 계속 필수다. baseline은 이 필수 요구사항과 앱별 RBAC·Secret·backup 설정을 검토했다는 운영자 선언이며 자동 의존성 발견이나 실행 검증이 아니다. 누락·만료·미래·원본 namespace 불일치는 전체 pass를 금지하며 부분 프로브 결과는 보존한다.
- 개별 프로브 결과는 5분 이내여야 하고 5초보다 미래이면 거부한다. 보고서 expiresAt은 가장 이른 개별 결과/검토 baseline 만료를 넘지 않는다. 서버 설정은 생성 시 deep copy하고 SHA-256을 기록한다.
- 매 새 시작에서 증거를 다시 수집한다. 감사 기록에 승인된 원문 Plan digest, 첨부 manifest digest, Plan ref/revision/reviewedAt, 서버 Policy digest, parity config digest, 관측/만료 시각을 추가한다. 암호가 확장된 DSN을 해시·기록하지 않는다. 이 digest는 실제 클러스터 전체 manifest fingerprint가 아니다. Registry의 동적 변화를 포함하는 완전한 환경 attestation도 아니다.
- 일반 이전과 planned_reverse를 구분하고 asynchronous_environment_rpo_unverified를 기록한다. 성공 응답이 독립 환경 WAL 내구성을 뜻하지 않는다. 복구 면제는 별도 outcome으로 남기며, 진행 중 복구에 신규 일반 이전 정책을 강제하지 않는다. 재개 시 새 검증을 붙이는 기능은 이 변경에 없다.
- 첫 실제 앱 어댑터는 moimroom 예약 생성이다. 별도 moimroom 변경에서 예약·슬롯·멱등 영수증이 한 트랜잭션으로 commit된다. 같은 키/다른 요청은 거부하고 결과 불명 요청은 같은 키로만 재시도한다. DB 권한과 credential 사용, 응답 유실, 동시성, 실제 pg_dump/pg_restore를 격리 환경에서 검증한다.

## 검토한 대안

- 리소스 존재를 기능 전체 pass로 승격: 실제 Secret 사용·감사 전달·알림·복원 근거가 없어 기각한다.
- 지금 전체 enforcement 적용: 미지원 capability가 많아 정상 이전과 복구를 막는다. 지원 어댑터별 실제 사용 결과와 승인 시 환경 fingerprint 비교가 준비된 뒤 서버 전용 선택 정책으로 도입한다. 필수 unknown/실패/만료는 그 모드에서 거부하고 일반 모드와 복구 전용 모드를 구분해야 한다.
- 임의 HTTP endpoint가 반환한 success를 E2E 증거로 수용: 신뢰 경계·principal·부작용을 확인할 수 없어 도입하지 않는다.
- 게이트웨이에서 POST 자동 재전송: 대상 앱의 멱등성을 모르는 상태에서 중복 부작용을 만들 수 있어 기각한다.

## 결과

증거 binding은 감사 API의 기존 details에 추가되며 기존 API/OpenAPI operation과 audit 모드는 유지된다. 신규 baseline 없는 기존 구성은 부분 관측을 계속 수행하지만 전체 pass는 될 수 없다. 관측 endpoint timeout은 기존 30초를 유지한다. baseline 갱신은 서버 소유 설정 검토·재기동이 필요하다.

로컬 DB 복원은 같은 disposable PostgreSQL 안에서 수행하며 backup 데이터를 외부로 옮기지 않는다. 이것은 제품의 cross-environment restore residency gate 구현이 아니다. 실제 cloud IAM·Kubernetes Secret 로드·감사 이벤트 전달/장기 보존·테스트 Alertmanager receiver·CNPG/Barman restore·gateway를 포함한 앱 왕복·운영 복구 RPO/RTO와 전체 환경 fingerprint 연계는 미지원/미검증이다. 해당 capability의 unknown을 pass로 바꾸지 않는다. 운영 알림 발송·배포·장애 주입·인프라 비용은 없다. 로컬 컨테이너와 백업은 검증 후 제거한다. 전체 #91 완료를 주장하지 않는다.
