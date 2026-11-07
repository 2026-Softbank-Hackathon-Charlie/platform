# 0061. 로컬 PVC의 단일 소유자로 동일 노드 게이트웨이 파드 복구를 제한한다

- 상태: 제안 — 코드·검증용 opt-in, 운영 적용 전
- 날짜: 2026-11-08

## 배경

게이트웨이는 앱별 hold/route/read weights/읽기 제외와 AI canary CAS를 프로세스 안에서 관리한다. 기존 상태 파일을 공유하더라도 두 프로세스가 독립 실행하면 한 파드의 hold가 다른 파드 쓰기를 막지 못한다. 현재 infra는 트래픽 게이트웨이 한 파드, Recreate, gp3 RWO PVC를 사용한다.

소스의 AWS management 노드 계획은 `2a:2, 2c:2`이고 GKE workload 정의도 존재한다. 이는 현재 실행 중인 노드 수를 증명하지 않는다. 기존 RWO 볼륨·단일 지역 ALB·관리 API/DB를 둔 채 파드를 다른 클러스터로 복제하는 것만으로 지역 HA를 얻을 수 없다. 새 비용/권한 없이 안전하게 검증할 첫 단계의 범위를 파드 실패로 제한한다.

## 결정

- 기본 동작을 보존하고 Linux `-exclusive-state`를 명시적으로 켠다. 모든 참가자는 동일 노드의 같은 로컬 파일시스템·PVC를 공유해야 한다. NFS/CIFS/FUSE는 거부한다. 별도 DB, Secret, RBAC, 리더 라벨 갱신을 추가하지 않는다.
- `<state-file>.lock` 고정 inode의 flock을 프로세스 수명 동안 보유한다. 이 파일은 unlink/rename하지 않는다. 상태 JSON은 여전히 atomic rename하며 Linux에서 파일과 부모 디렉터리를 fsync한다.
- 소유자만 gateway.New/복원을 실행한다. 모든 TCP 진입점은 공유 Unix socket의 소유자에게 전달한다. public/admin 소켓을 분리하고 권한 0600을 적용한다. 두 파드 모두 소유자의 health를 제공하여 정상 시 Ready가 된다. 상태가 없는 standby가 초기 환경으로 쓰기를 받는 경로는 없다.
- 소유자 부재·연결 실패는 503/Retry-After다. relay transport는 keepalive를 꺼서 재사용 연결 오류에 대한 자동 재시도를 막는다. 스트림은 즉시 flush하며 Host/Authorization과 기존 owner의 body/인증 제한을 보존한다. 이미 전달한 요청이 연결 오류로 끝나면 결과는 unknown이다.
- 요청을 받기 전에 `.running` 표식을 파일/디렉터리에 fsync한다. 정상 종료는 신규 owner 입장을 막고 모든 TCP/Unix HTTP 요청 drain, 최종 상태 저장 성공 뒤에만 표식을 지운다. drain 실패는 프로세스가 실제 종료될 때까지 잠금을 유지한다.
- 이전 표식이 남으면 소유자 승계가 모든 앱 hold를 저장한다. 읽기는 제공하지만 모든 쓰기·관리 mutation과 `/readyz`는 `owner_recovery_required` 503이다. 상태 조회에 보이는 새 inflight=0으로 원격 commit 완료를 추정하지 않는다. 전용 AI gateway와 이 모드의 조합은 거부하여 기존 AI 품질/CAS 경계를 변경하지 않는다.

## 복구와 한계

운영자는 실제 DB primary/이전 단계/원격 쓰기 결과·DB fence를 확인한다. 모든 공유 프로세스를 중지한 뒤 `.running`만 제거하고 재시작하며, 저장된 hold를 확인한 경로에 대해서만 명시적으로 release한다. `.lock`/JSON은 삭제하지 않는다. 별도 자동 복구 승인 API는 추가하지 않는다. `PromoteRequested` 이후 자동 DB 롤백, source fence 비활성화, 결과가 unknown인 쓰기의 재전송은 금지한다.

이 모드는 동일 노드 파드 장애 뒤 읽기 복구와 쓰기의 안전 정지를 제공한다. 노드·EBS·AZ·리전·AWS 진입점 SPOF, 메모리 ledger/통계 손실, 원격 완료의 exactly-once는 해결하지 않는다. Recreate 업그레이드는 여전히 중단을 수반한다. 기존 바이너리가 잠금을 모르므로 첫 활성화/다운그레이드 때도 Recreate가 필요하다.

다음 단계는 다중 노드 shared control state, 실제 backend fencing, 멤버십/hold-drain 합의와 지역 밖 진입점 설계를 함께 검증하는 것이다. Kubernetes Lease의 시간 기반 만료만으로 기존 프로세스의 원격 쓰기를 fencing했다고 주장할 수 없다. 새 권한·리소스·과금은 구체적인 별도 적용 승인을 받아야 한다.

## 검증

두 독립 HTTP gateway 프로세스의 실제 SIGTERM/SIGKILL, 앱 registry/route/hold 복원, 원격 commit이 아직 실행 중인 상태의 mutation 차단, 운영자 복구 후 단일 쓰기를 검증한다. 소켓 불통/권한·lock symlink 거절, 상태 rename 동안 동일 잠금, 스트리밍/인증/Host/hop-by-hop header, unknown write 재시도 금지를 별도로 검사한다. 운영 트래픽이나 클라우드에 이 변경을 적용한 증거는 아니다.

## 자료

- [Linux flock](https://man7.org/linux/man-pages/man2/flock.2.html)
- [Kubernetes volume access modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes)
- [Kubernetes pod affinity](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#inter-pod-affinity-and-anti-affinity)
- [exclusive runner](../../cmd/gateway/exclusive.go), [process tests](../../cmd/gateway/exclusive_linux_test.go)
