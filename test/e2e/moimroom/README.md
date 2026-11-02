# E2E: moimroom을 데이터와 사진째 부하 중에 이전하고 원본 정리

샘플 앱 moimroom(공간 예약 서비스)을 대상으로, 이전의 처음부터 끝까지를 실제 구성요소로 돌린다. `test/e2e`가 단순한 테스트 앱으로 플랫폼과 엔진을 확인한다면, 이 테스트는 실제 앱의 DB 스키마(`app`), 오브젝트 스토리지의 사진, Helm 차트, 커넥션 풀을 가진 앱까지 포함한다.

```bash
test/e2e/moimroom/run.sh          # 실행하고 끝나면 전부 지운다
KEEP=1 test/e2e/moimroom/run.sh   # 끝난 뒤에도 그대로 둔다
test/e2e/moimroom/down.sh         # 남긴 것을 지운다
```

필요한 것: docker, kubectl, helm, rclone, go, curl. 그리고 워크스페이스에 platform과 나란히 moimroom, migration-engine 체크아웃이 있어야 한다(`MOIMROOM_DIR`, `ENGINE_DIR`로 바꿀 수 있다). moimroom 이미지는 그 체크아웃에서 빌드하고, decommission은 아직 플랫폼 API에 없어서 migration-engine에서 빌드한 `hopmigrate`로 실행한다.

## 흐름

1. kind 클러스터 `hop-moimroom`에 CNPG operator를 올린다. 네임스페이스 `moimroom`에 환경마다 MinIO 하나(`minio-src`, `minio-dst`)와 버킷 `moimroom-photos`를 만든다.
2. 원본 DB `moimroom-src-db`(primary)와 대상 DB `moimroom-dst-db`(replica)를 만든다. 대상은 실제 운영에서 infra가 미리 만들어 두는 것처럼 이전 전에 만든다.
3. moimroom Helm 차트로 `moimroom-src`(원본 DB, minio-src, 공간 16곳 seed)와 `moimroom-dst`(대상 replica, minio-dst)를 띄운다. 대상 앱은 읽기 전용 replica 위에서 떠야 한다.
4. 플랫폼 저장소 컨테이너, 게이트웨이(`src`, `dst`), 플랫폼 API를 호스트에 띄운다. API의 엔진이 rclone으로 사진을 옮긴다(`RCLONE_CONFIG_SRC_*`, `RCLONE_CONFIG_DST_*`, `PLATFORM_PLAN_BUCKETS`).
5. 드라이버가 게이트웨이로 사용자 부하를 보낸다. 예약 4명(1시간짜리 빈 슬롯을 계속 예약), 호스트 1명(2초마다 사진 업로드), 조회 4명(공간 상세와 올라간 사진). 성공한 예약은 written-records 스트림 `moimroom`에 기록한다.
6. 드라이버가 플랫폼 API로만 이전한다. 카나리 10%·50%(10초씩), 운영자 전환 승인, 전환 후 관찰 30초.
7. 엔진 `hopmigrate decommission`으로 원본을 정리한다. 옛 앱을 멈추고 20초 동안 원본 접속을 세고, 옛 앱과 옛 DB를 차단한 뒤 게이트웨이로 스모크 요청을 보내고, 남은 리소스를 분류한다.

## 통과 조건

- run이 `succeeded`, verify의 `missingWrites`와 `missingFiles`(rclone check)가 0
- 기록한 예약이 새 primary에 전부 있음
- 올린 사진이 전부 새 환경에서 같은 크기로 나옴(파일이 실제로 옮겨짐)
- decommission이 통과: 미처리 리소스 0, 관찰 중 원본 세션·요청 0, 차단 확인, 스모크 통과

## 포트 (127.0.0.1)

| 포트 | 용도 |
| --- | --- |
| 55742, 55743 | 원본·대상 DB (NodePort) |
| 55762, 55763 | minio-src, minio-dst (NodePort, rclone) |
| 18201, 18202 | moimroom-src, moimroom-dst (NodePort, 게이트웨이 backend) |
| 55752 | 플랫폼 저장소 |
| 18280 | 플랫폼 API |
| 18200, 19902 | 게이트웨이 사용자 요청, 관리 API |

## 실제 환경과 다른 점

- 두 환경이 kind 클러스터 하나, 네임스페이스 하나에 있다. 클러스터 사이 네트워크와 TLS 이름은 확인하지 않는다.
- 엔진과 rclone은 NodePort로 DB와 MinIO에 붙는다.
- decommission은 플랫폼 API가 아니라 엔진 CLI로 실행한다.
- Windows(Docker Desktop)에서 비어 있는 포트를 열 때 가끔 "invalid pointer address"(WSAEFAULT)로 실패한다. 게이트웨이, API, Docker 포트 프록시 모두에서 봤고 같은 포트로 다시 열면 된다. `run.sh`는 이 단계들을 세 번까지 다시 시도한다.

## 결과 (2026-11-02, Windows, Docker Desktop, kind 재사용)

```
run            status=succeeded
canary  10%    passed (47 requests, error rate 0%, p95 5ms)
canary  50%    passed (281 requests, error rate 0%, p95 5ms)
observe        ok x3 (약 700 requests/10s, error rate 0%, p95 약 180ms), passed=true
downtime       15.3s
gateway        requests=4645 failed=0
verify         missingRows=0 missingWrites=0 missingFiles=0
bookings       ok=1224 failed=0
photos         uploaded=32 failed=0
recheck        1224 of 1224 bookings on the new primary
photo recheck  32 of 32 photos served by the new environment with the same bytes
decommission   PASSED: unprocessed 0 / stopped 1 / retained 2 / awaiting approval 1,
               20s observe: old-database sessions 0, source requests 0, block verified, smoke 2/2
```

- `reads notFound`(약 130건)는 카나리 동안 대상이 방금 생긴 예약 행이나 사진 파일을 아직 받지 못한 경우다. 실패로 세지 않는다.
- 이 테스트로 찾은 문제: moimroom이 replica DB에서 시작하지 못함(moimroom 수정), decommission이 종료 중인 파드를 멈춘 것으로 봄(엔진 수정).

