# 0041. 앱 커밋 기록을 수집하고 실행별 검증 입력을 동결한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

성공한 이전에도 written-records stream을 설정하지 않으면 missingWrites 측정값이 없다.
HTTP 성공 횟수만으로는 대상 DB의 실제 테이블과 PK 존재 여부를 검증할 수 없다.
빈 입력을 누락 0건으로 표시하면 증거 없는 성공을 만들게 된다.

## 결정

Moimroom은 예약 INSERT와 같은 트랜잭션에 실제 app.reservations PK를 outbox에 저장한다.
기록에는 재시도에 안정적인 eventId, outbox 기록 시각, WAL로 보존되는 DB generation이 있다.
시각은 커밋 완료 시각이 아니다. scope는 application_commits, coverage는 reservation_inserts다.
클라이언트 성공 응답 수신, 전체 CRUD, 행 내용, 세션 정합성을 증명하지 않는다.

PLATFORM_WRITTEN_RECORD_TOKENS는 stream:token 형식의 전용 자격증명이며 token은 32자 이상이다.
해당 stream POST만 허용하고 application_commits/reservation_inserts/app.reservations와
양의 정수 PK, 일치하는 reservation-<pk> eventId, 시각을 요구한다. 조회·Plan 제출·이전 실행은
허용하지 않는다. 앱에 일반 operator/agent token을 주입하지 않는다. 인증된 앱의 보고이며
DB 실행에 대한 독립적인 암호학적 증명은 아니다.

중앙 저장소는 stream별 transaction advisory lock으로 수집 트랜잭션을 직렬화한다.
동일 eventId 재시도는 기존 행을 갱신하지 않는다. 내용·generation·scope·coverage 변경은
배치 전체를 거부한다. 요청 취소를 유지하면서 저장 시도에 명시적인 5초 제한을 적용한다.

기본 stream은 63자 제한 내의 app-<App.name>이다(앱 이름 최대 59자). draft는 실제 기록이
있고 최신 이벤트 시각이 10분 이내이며 source DB generation이 일치할 때만 자동 연결한다.
그 외에는 미설정 상태로 두며 writtenRecordsStream을 명시할 수 있다. Start/Resume에서도
application_commits stream의 source generation을 확인한다. 조회는 읽기 전용이며 DSN이나
원본 공급자 오류를 API에 노출하지 않는다.

엔진 verify 직전에 중앙 수집 count와 최대 seq를 고정하여 해당 seq까지만 내보낸다.
빈 입력·읽기 오류·count 불일치는 실패로 처리한다. RunResult.writeVerification은 실행별
동결 입력 수, cutoff, 최신 이벤트 시각, scope/coverage 및 상태·사유를 저장한다.
완전한 비어 있지 않은 오류 없는 검사에만 missingWrites를 숫자로 제공한다.
현재 stream 통계와 과거 실행의 동결 입력은 서로 다른 값이다.

source reporter는 SELECT만 수행하며 delivery 상태를 DB에 쓰지 않아 gateway fencing을
우회하지 않는다. sweep 시작에 upper seq를 고정하고 100건씩 그 범위까지 읽은 뒤 0부터
반복한다. seq는 커밋 순서가 아니므로 지속적인 유입 중에도 늦게 커밋한 낮은 seq를 재수집한다.
1초 간격, DB pool/SQL/HTTP 전체 3초 제한, redirect 거부, 전체 accepted 수 확인을 적용한다.
실패·재시작은 재전송하고 중앙 eventId가 중복을 제거한다.

## 검토한 대안

- HTTP 성공 횟수만 수집: 실제 PK 및 DB 커밋과 연결되지 않아 배제했다.
- worker가 전송 완료를 source DB에 UPDATE: gateway 밖 쓰기를 만들어 배제했다.
- seq cursor를 영구 증가: 역순 커밋을 영구 누락하므로 고정 범위 재스캔을 선택했다.
- 빈 입력을 누락 0건으로 표시: 검증 증거가 없어 미측정/실패 상태로 유지한다.

## 결과

현재 수집된 비어 있지 않은 입력의 대상 PK 존재 여부를 설명할 수 있다. 늦은 보고는 중앙
cutoff 이후 제외될 수 있으므로 모든 앱 커밋/클라이언트 승인 쓰기 수집의 완전성을 보장하지
않는다. latestAt은 이벤트 기록 시각이며 마지막 도착 시각이나 heartbeat가 아니다.

append-only outbox와 중앙 기록의 자동 retention/compaction은 없다. 이력이 길면 저장량과
재스캔 읽기·재전송 부하 및 수집 지연이 증가한다. outbox 자체도 WAL 및 앱 schema 검증 대상이다.
정상 예약 취소는 행을 보존한다. 수동 DELETE/TRUNCATE, reseeding, PK 재사용 전에는 승인된
정비 과정에서 이전 stream을 폐기하고 identity/stream을 교체해야 한다. 새 DB 설치는 새
DB generation과 새 stream/token이 필요하고 동일 dataset의 복원/이전은 identity를 유지한다.

운영 자격증명·환경변수·배포는 변경하지 않았다. source primary에서 새 identity/outbox 스키마를
먼저 생성하고 WAL로 target에 전달된 것을 확인한 뒤 target collector를 활성화해야 한다.
standby MigrateSkip은 테이블을 만들지 않는다. 자세한 활성화 순서는 앱 운영 문서를 따른다.

회귀 검증은 stream 권한·불변 metadata·중복·트랜잭션 rollback·역순 커밋·고정 upper 범위·
중앙 cutoff·빈/불완전 검사·HTTP 재시도와 redirect 거부를 다룬다. 기존 PostgreSQL 테스트
환경변수를 사용하며 Moimroom CI에 PostgreSQL을 연결하여 API 통합 테스트가 실행되게 했다.
