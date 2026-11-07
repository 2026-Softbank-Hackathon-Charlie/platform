# 원본 DB 보존 만료 및 자동 정리 검증

## 결과

- Linux Go 1.26.5, CGO race 전체: 통과 (`linux-full-race.log`, `status.log`).
- 안전 경계 보강 후 retention/runs/API/store/cmd API 관련 race: 통과 (`final-related-race.log`, `final-status.log`).
- Linux `go vet ./...`, `go build ./...`: 통과 (`final-vet.log`, `final-build.log`).
- 실제 PostgreSQL 17 두 독립 pool의 cluster lease: 통과, SKIP 아님 (`postgres-locks.log`). 잠금 경합, 부분 획득 실패의 해제, 중복 키와 해제 뒤 재획득을 확인했다. 관련 race에서도 실제 PostgreSQL store 검사를 실행했다.
- 마지막 시작 전 Capture timeout 상한 추가 뒤 Windows 현재 소스의 `go test -count=1 ./internal/runs ./internal/retention`: 통과 (`windows-final-related.log`).
- 프론트엔드 전체 `npm test`: 31개 파일, 402개 테스트 통과. `npm run build` 통과, 기존 500 kB 초과 bundle 경고 유지. 실제 API 대신 통제된 fixture를 사용한 UI 테스트이며 운영 삭제 증거가 아니다.
- `git diff --check`: 통과.

## 확인한 안전 경계

원본/대상 UID 교체, 원본 primary, 대상 불건강, 보존 hold/연장, gateway 이전 상태·오래된 응답·진행 요청, SLO 미측정/위반, SQL 확인 불가·다른 client session, 백업 부재/이전 종료보다 오래된 백업, 복구 검증 부재·다른 실행의 기록은 삭제를 차단했다. Kubernetes client-go fake를 사용했다.

삭제는 UID·resourceVersion 전제조건과 Orphan propagation을 사용하며 PVC를 직접 삭제하지 않음을 검사했다. 실제 API server의 GC 동작을 검증한 것은 아니다. source Cluster와 기록된 Pod 부재를 나눠 재확인한다.

영속 기한의 재시작 복원, 검사 불능 시 보류와 재시도, 새 이전의 기존 예약 취소, 삭제 중 원본 재사용 차단, 삭제 완료 후 즉시 롤백 거부, 별도 성공한 이전으로 재생성한 환경의 재사용, 과거 실행 미등록, 관찰 미완료, deleting 저장 실패 시 삭제 금지를 확인했다. API 인증/인가·보존 단축 거절·연장·계속 보존·취소 후 재활성화 거절을 계약 검증과 함께 확인했다.

프론트엔드는 실제 기한 표시·연장 POST·계속 보존 POST·종료 상태 조작 숨김·조회 오류·viewer 읽기 전용·예약 없음·지도에서 오래된 롤백 명령 거절을 확인했다.

## 환경과 실패

호스트 Docker 포트 publish는 Windows WSAEFAULT(`invalid pointer address`)로 실패했다. 포트 없는 전용 `hop-retention-test` 네트워크의 PostgreSQL과 Go 컨테이너에서 검증했다. Windows API 전체 실행도 동일한 소켓 오류로 한 테스트가 실패했으며 이를 성공으로 집계하지 않는다. Linux 전체와 관련 API race는 통과했다.

Linux는 read-only checkout을 임시 사본으로 복사해 Go/YAML/Markdown 텍스트를 LF로 정규화했다. 실제 코드와 assertion을 바꾸지 않았다. `linux-runner.sh`, `final-runner.sh`는 당시 runner다. /source·/evidence·Windows module download cache와 Go cache mount, 전용 PostgreSQL 접속 환경변수가 필요하다. `linux-initial-race.log`와 `initial-status.log`는 앞선 검증으로 최종 결과와 구분한다. 로그 인코딩은 UTF-8로 정규화했다.

## 미검증 및 적용 범위

브라우저 도구는 `The browser access host is unavailable`, native pipe unavailable로 실패했다. 실제 화면의 desktop/mobile/keyboard 동작은 미검증이다.

실제 Kubernetes garbage collector·CNPG operator·admission/CNI에서의 삭제, 외부 백업 bytes의 가용성, 독립 복구 파이프라인의 실행, 운영 migration/자동 삭제는 수행하지 않았다. 복구 ConfigMap은 독립 검증자/운영자의 선언이며 이 컨트롤러가 복구를 수행했다는 뜻이 아니다. PVC 비용은 계속 남는다. 기존 Recover의 단일 활성 API 전제를 유지한다.

신규 cloud 리소스와 운영 변경은 없다. 전용 로컬 검증 컨테이너·PostgreSQL 데이터 볼륨·네트워크는 검증 뒤 제거하고 기존 공유 Go cache는 유지한다. 기능 기본값은 off이며 운영 적용 전 실제 환경의 삭제/복구 리허설이 필요하다.
