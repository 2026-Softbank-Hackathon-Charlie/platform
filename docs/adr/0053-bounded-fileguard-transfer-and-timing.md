# 0053. 파일 검사 범위를 유지하고 전송 병렬성과 단계별 시간을 명시한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

온프렘 이전 실행 `moimroom-mig-gke-to-onprem-20261107-141917`의 저장된 실제 증거에서 쓰기 보류는 116.828초였다. 보류 중 최종 파일 동기화가 77.466초(66.3%), DB 승격부터 writable 확인까지가 32.030초(27.4%)였다. prepare 권한 오류 후 재시도까지 포함한 전체 실행 시간 17분 34초와 쓰기 보류를 구분한다.

파일 934개, 합계 85,898바이트의 manifest digest는 presync/delta/fence에서 같았다. 그러나 같은 크기·목록·과거 digest만으로 현재 원본 또는 대상 바이트의 불변성을 증명할 수 없다. 기존 fileguard는 매 pass 새 스냅샷을 내려받고, 전체 검사·승인·재검사 후 모든 객체를 다시 올리고 대상 바이트를 다운로드 비교한다. 다운로드 pool은 4, 업로드 및 다운로드 비교는 rclone 기본 pool에 의존했다. 기존 evidence는 내부 단계 시간을 나누지 않아 다운로드/업로드/대상 비교 중 지배 구간은 확정할 수 없다.

## 결정

- 완성된 스냅샷과 전체 검증 순서는 유지한다. `--ignore-times`, `check --download --one-way`, source 재열거, 승인 후 staging 재검사, 총 바이트/객체 제한과 실패 시 차단을 보존한다.
- 다운로드와 업로드 `--transfers`, 각 전송과 대상 비교 `--checkers`를 16으로 제한한다. `--buffer-size 1Mi`, `--multi-thread-streams 0`을 명시한다. 동시에 실행되는 전송의 설정된 buffer 합계는 16MiB다. 이는 backend별 추가 버퍼, Go heap, 프로세스 전체 RSS의 상한을 뜻하지 않는다. object/total/manifest 제한을 바꾸지 않는다.
- 전역 rclone 환경 옵션을 거르는 기존 필터를 유지하고 pool은 서버가 구성한 명령 인수에만 넣는다. 사용자 Plan이나 환경 플래그로 검사 범위를 바꾸지 않는다.
- Sync/Copy/Check의 output JSON은 기존 content report 필드를 유지하고 `timingMillis`를 추가한다. 단위는 정수 ms이며 각 작업의 실제 경과시간만 기록한다. 기존 SyncResult.Duration은 Go duration의 기존 JSON ns 단위를 유지한다.
- `prepare`는 스냅샷 생성부터 승인 후 재검사까지의 전체 시간이다. `sourceInventory`와 `sourceDownload`는 그 안의 rclone 호출 경과시간이다. `prepareOther`는 나머지이며 로컬 정렬/검사/디스크 작업/정책 callback을 모두 포함한다. 독립 scan 시간으로 해석하지 않는다. 이 네 값은 겹치므로 모두 합산하지 않는다.
- `upload`, `targetCheck`, `cleanup`, `total`은 각각 전송, 전체 대상 비교, staging 제거, 전체 작업 시간이다. 실패한 호출도 누적한다. 실행하지 않은 단계는 0이다. Check의 upload는 0이다. 작업별 측정기는 공유 Syncer를 변경하지 않는다.
- 파일명, 개별 hash, 원문 또는 rclone stderr를 출력하지 않는다. timing을 승인 자료나 manifest digest에 섞지 않는다.

## 검토한 대안

- 메타데이터나 과거 검사 결과로 unchanged 파일을 생략: 원본 version pin/immutable snapshot 증명이 없어 기각했다. 동일 크기 변조와 대상 손상을 놓칠 수 있다.
- 전체 재업로드를 checksum 기반 건너뛰기로 교체: backend마다 hash 지원이 다르고 복구 동작도 바뀌므로 이번 최소 변경에서 제외했다.
- 무제한 병렬 처리: 메모리, 연결 수, backend throttling 위험 때문에 기각했다.
- 검사나 DB fence를 없애기: 데이터 레지던시/무손실 증명 범위를 약화하므로 기각했다.
- DB 승격 32.030초와 settle 4.146초를 함께 조정: operator/PG/네트워크 관측이 더 필요하며 별도 원인이다. 현재 paired chart의 smartShutdownTimeout=1, WAL 재연결 1초를 임의로 낮추지 않는다.

## 결과

대량의 작은 객체에 대한 요청 동시성을 늘리지만, 실제 운영 보류시간의 절감률은 아직 확인하지 않았다. pool 증가가 backend throttling이나 메모리 사용을 늘릴 수 있다. 원격 CI의 race 회귀와 실제 로컬 rclone 손상 검출·복구는 안전성/지원 여부를 검증하며, 운영 네트워크 성능을 증명하지 않는다. 새 운영 이전이나 부하는 실행하지 않는다. 향후 승인된 실행에서는 각 단계 timing, backend 오류/재시도, 메모리와 보류시간을 함께 비교해야 한다.

공식 동작 근거: [rclone transfers/checkers/buffer 문서](https://rclone.org/docs/), [check --download](https://rclone.org/commands/rclone_check/).
