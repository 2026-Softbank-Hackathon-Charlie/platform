# 0065. API는 DB의 리더 자격으로 백그라운드 작업을 넘기고, 롤링 업데이트에서 끊기지 않는다

- 상태: 채택
- 날짜: 2026-11-08
- 관련: infra ADR 0099(차트 적용), ADR 0026과 0061(게이트웨이)

## 상황

관리 플랫폼 API(`charlie-platform/hopcharlie-api`)는 pod 하나, `strategy: Recreate`로 돈다. infra 차트의 pod 템플릿이 바뀌면(플랫폼 이미지, hopai 이미지의 `checksum/hopai`, `deploy.backendTemplates` 같은 env) Argo CD 자동 동기화가 곧바로 API를 다시 띄운다. 2026-11-08에는 관리 클러스터에서 94분 동안 ReplicaSet이 6개 바뀌었다.

한 번 바뀔 때마다 이렇게 된다.

- 옛 pod가 먼저 끝나고 새 pod가 init 컨테이너(SSM 설정 읽기)를 거쳐 Ready가 되고 ALB 헬스 체크를 통과할 때까지 `https://api.hopcharlie.dev`가 503을 돌려준다. 오늘 한 번에 1~2분이었다.
- 옛 프로세스는 SIGTERM을 받으면 HTTP를 닫고 이전 실행이 끝나기를 `-shutdown-wait`(15분)까지 기다리려 한다. 그러나 차트에 `terminationGracePeriodSeconds`가 없어 30초 뒤 SIGKILL로 끝난다. 쓰기 보류(fence부터 cutover까지) 안에서 죽은 실행은 새 프로세스의 `Recover`가 `interrupted`로 바꾸고, 사람이 resume할 때까지 게이트웨이의 보류가 남는다. 오늘 한 실행은 보류가 30초 예산에 대해 6분 56초 이어졌다.
- 두 프로세스가 겹치면 안 되는 이유가 코드에 있다. 실행은 프로세스 안(`runs.Runner.live`)에서 돌고, 시작할 때 `Recover`가 `running`과 `awaiting_switch`인 실행을 모두 `interrupted`로 바꾼다. 배포 실행기(`deploy.Executor.Start`), 온보딩 실행기, DB 경로 조정(`dbroute`), 에이전트 정리, 원본 보존 정리도 프로세스마다 하나씩 돈다. 엔진 상태 파일은 ReadWriteOnce EBS 볼륨(`-state-dir /data/runs`)에 있다.

## 결정

API 프로세스는 둘이 잠시 겹쳐도 되게 한다. 한 번에 한 프로세스만 리더가 되어 백그라운드 작업을 맡고, 리더가 바뀔 때 실행을 안전한 지점에서 넘긴다. 차트는 이 이미지가 돈 뒤에 RollingUpdate(`maxSurge: 1`, `maxUnavailable: 0`)로 바꾼다(infra ADR 0099). 평소에는 pod 하나 그대로다.

### 리더 자격

- 리더 자격은 플랫폼 PostgreSQL의 세션 advisory lock(`hashtextextended('hopcharlie-leader/api', 0)`)이다. 전용 연결로 잡고(`store.TryLeadership`), 세션이 끝나면 함께 풀린다. 클러스터 lease(`store.TryClusterLease`)와 같은 방식이다. Kubernetes Lease는 시간 만료라서 옛 프로세스가 아직 일하는 동안 새 리더가 생길 수 있고, API에 RBAC도 더 줘야 해서 쓰지 않는다.
- 잡은 뒤에 그 세션의 `application_name`을 `hopcharlie-leader/api <pod IP>:8080`으로 바꾼다. 다른 프로세스는 `pg_stat_activity`에서 이 이름을 찾아 리더 주소를 안다(`store.LeaderAddr`). 이름과 잠금이 같은 세션에 있으므로 주소가 리더와 어긋나지 않는다.
- 리더 세션은 `tcp_keepalives_idle=10`, `interval=5`, `count=3`을 건다. 노드가 사라진 리더의 잠금을 서버가 약 25초 안에 푼다.
- 리더는 2초마다 세션을 확인한다. 확인이 실패하면 다른 프로세스가 이미 리더일 수 있으므로 프로세스를 바로 끝낸다(exit 1). 평소처럼 죽은 프로세스로 취급되고, 다음 리더의 `Recover`가 남은 실행을 `interrupted`로 바꾼다. 플랫폼 PostgreSQL이 재시작하면 API 컨테이너도 한 번 재시작한다.

### 요청

- 모든 프로세스는 시작하자마자 HTTP를 연다. 읽기(GET, HEAD, OPTIONS)는 어느 프로세스든 직접 처리한다. 실행 문서와 이벤트는 리더가 DB에 쓰므로 다른 프로세스도 같은 값을 읽는다.
- 리더만 처리하는 요청은 GET과 HEAD가 아닌 모든 요청과 경로가 `/events`로 끝나는 스트림이다(`leader.LeaderOnly`). 실행 시작, switch, abort, resume, 배포 webhook, 온보딩, AI 실행이 모두 여기에 든다. 경로마다 나누지 않고 메서드로 나눈 것은 새 경로가 생겨도 빠지지 않게 하려는 것이다.
- 리더가 아닌 프로세스는 이 요청을 리더 주소로 그대로 넘긴다(`leader.Gate`, `httputil.ReverseProxy`). 넘긴 요청에는 `X-Hopcharlie-Forwarded`를 붙이고, 이 헤더가 있는 요청을 받은 비리더는 다시 넘기지 않고 503을 돌려준다.
- 리더가 없는 동안(넘겨주는 중) 요청은 250ms마다 다시 보며 최대 30초 기다린다. 그 사이에 리더가 생기면 넘기고, 자기가 리더가 되면 직접 처리한다. 30초가 지나면 503 `leader_unavailable`과 `Retry-After: 2`를 돌려준다. 넘긴 요청의 연결이 끊기면 리더가 처리했는지 모르므로 다시 보내지 않고 502 `leader_unreachable`을 돌려준다.

### 리더가 하는 일

리더 자격을 얻으면 이 순서로 한다(`cmd/api` `lead`).

1. `planSvc.RecoverStarting`, `runner.Recover`: 앞 리더가 죽으면서 남긴 것을 정리한다. 넘겨받는 경우에는 남은 것이 없다.
2. `runner.ResumeHandedOff`: 앞 리더가 넘긴 실행(상태 `interrupted`, 사유가 `runs.HandoffReason`과 정확히 같은 것)을 그 계획으로 resume한다. 실패한 실행은 `interrupted`로 남고 사람이 resume한다.
3. 원본 보존 정리, 에이전트 정리, DB 경로 조정, 배포 실행기, 온보딩 실행기를 시작한다.
4. 그 뒤에 리더 요청을 직접 받기 시작한다. 그래서 자동 resume과 사람이 보낸 요청이 섞이지 않는다.

### 넘겨주기

SIGTERM을 받은 리더는 이 순서로 한다(`cmd/api` `handOver`).

1. 리더 요청을 더 받지 않는다. 이미 처리 중인 리더 요청은 끝날 때까지 기다린다. 그 뒤의 리더 요청은 새 리더로 넘긴다.
2. `runner.HandOff`: 실행마다 아래 표대로 멈추거나 끝까지 간다.
3. 백그라운드 루프를 멈추고, 배포 실행기를 멈춰 최대 30초 기다린다. 끝나지 않은 배포는 새 리더의 `Start`가 다시 잡는다(전과 같다).
4. 실행이 모두 멈추거나 끝나기를 `-shutdown-wait`까지 기다린다. 넘으면 전처럼 취소한다(엔진이 승격 전이면 되돌리고 뒤면 halt한다). 그 뒤 3분을 더 기다린다.
5. 리더 자격을 놓는다. 새 프로세스가 1초 안에 잡는다.
6. SIGTERM부터 `-shutdown-delay`가 지날 때까지 HTTP를 계속 처리한 뒤 닫는다. ALB가 옛 pod로 요청을 보내지 않게 되는 시간을 준다.

실행 단계별로 넘길 때 하는 일은 이렇다. 기준은 쓰기 보류다. 보류가 시작되기 전에는 멈춰도 되돌릴 것이 없고, 보류가 시작된 뒤에는 멈추면 보류가 남는다.

| 실행이 있는 곳 | 넘길 때 | 새 리더 |
| --- | --- | --- |
| sync 단계(precheck, prepare, presync, delta) 실행 중 | 지금 단계는 끝까지 하고 다음 단계 전에 멈춘다 | 다음 단계부터 이어 간다 |
| fence 전, 보류와 demote와 promote가 없음 | 멈춘다 | fence부터 이어 간다(승인은 문서에 있다) |
| canary 단계 | 지금처럼 멈추고 읽기 비율을 원본으로 돌린다 | canary를 처음부터 다시 한다 |
| switch 승인 대기(`awaiting_switch`) | 멈춘다 | 다시 승인 대기로 돌아간다 |
| fence부터 finalize까지(보류, demote, promote 중 하나라도 있음) | 멈추지 않고 끝까지 한다 | 넘겨받을 것이 없다 |
| observe | 지금처럼 관찰을 멈추고 `succeeded`로 끝낸다 | 이어 가지 않는다 |

자동 resume은 이 프로세스가 표의 "멈춘다"로 멈춘 실행에만 한다(`runs.handOffBefore`가 `WritesHeld`, `SourceDemoted`, `PromoteRequested`, `SourceFenced`가 모두 거짓이고 fence가 `pending`일 때만 참). 프로세스가 죽어서 `Recover`가 바꾼 실행, 취소된 실행, halt된 실행은 전처럼 사람이 resume한다. 어디서 멈췄는지 확실하지 않기 때문이다.

## 결과

- infra의 일반 배포(pod 템플릿이 바뀌는 동기화)에서 API 읽기는 끊기지 않는다. 리더 요청은 리더가 바뀌는 순간(보통 1초 미만) 동안만 기다린다. 쓰기 보류 안에 있는 실행이 있으면 그 실행이 끝날 때까지 기다리고, 30초를 넘으면 503 `Retry-After`를 받는다.
- 쓰기 보류 안의 실행은 배포 때문에 끊기지 않는다. 승인 대기, canary, sync 단계에 있던 실행은 새 리더에서 자동으로 이어진다.
- 새 pod는 옛 pod와 같은 노드에 떠야 한다(ReadWriteOnce 볼륨). 노드에 자리가 없으면 롤아웃이 멈추지만 옛 pod가 계속 일한다.
- 첫 적용은 두 단계다. 이 코드가 든 이미지를 Recreate로 한 번 띄운 뒤 차트를 RollingUpdate로 바꾼다. 리더 자격을 모르는 옛 이미지와 겹치면 둘 다 백그라운드 작업을 한다.

## 범위 밖

- 게이트웨이. 게이트웨이는 `images.platform`과 `gateway.*` 값이 바뀔 때만 재시작한다(차트 템플릿 확인). 플랫폼 이미지 교체 때의 끊김(ADR 0026, 약 25~45초)과 보류 상태의 넘겨주기는 ADR 0026, 0061에 남은 과제다. 게이트웨이 이미지를 API와 나누는 것(ADR 0026 대안 B)이 다음으로 효과가 크다.
- 상시 두 대. 코드는 여러 대를 받지만 두 pod가 같은 노드에 묶여 노드 장애를 막지 못하므로 평소에는 한 대로 둔다. PodDisruptionBudget도 한 대에서는 노드 drain만 막으므로 두지 않는다.
- 재시작이 필요 없는 설정 변경(`checksum/hopai`, parity, onprem 설정)을 다시 읽게 하는 것. 롤아웃이 끊김 없이 되므로 이번에는 바꾸지 않는다.
- 엔진 상태를 DB로 옮기는 것. 볼륨을 없애면 노드 제약도 사라지지만 엔진을 바꿔야 한다.
- observe 단계의 이어 가기, 프로세스가 죽은 경우의 자동 resume.

## 검증

- `internal/store/leader_test.go`(PostgreSQL): 두 풀 사이의 잠금, 주소 조회, 놓은 뒤 다른 쪽이 잡는 것.
- `internal/leader/leader_test.go`: 리더 요청 분류, 넘기기, 두 번 넘기지 않기, 리더가 될 때까지 기다리기, 처리 중 요청을 기다린 뒤 비활성화.
- `internal/runs/handoff_test.go`: 같은 저장소와 상태 디렉터리를 쓰는 두 Runner로 승인 대기 실행을 넘기고 이어 가기(단계는 한 번씩만 실행), 보류 안의 실행은 끝까지 하기, sync 단계 사이에서 멈추고 이어 가기, 다른 이유의 `interrupted`는 이어 가지 않기.
- kind(단일 노드, local-path RWO PVC, postgres:17.11-alpine): 차트와 같은 RollingUpdate, 같은 노드 affinity, `-shutdown-delay 10s`로 API를 세 번 롤아웃하는 동안 Service로 GET 1311건과 로그인 POST 385건을 보냈다. 실패 0건, POST 최대 0.89초였다. 리더는 매번 0.24~0.29초 안에 넘어갔다. 같은 클러스터에서 Recreate로 한 번 롤아웃하면 GET 52건과 POST 1건이 실패했다.
