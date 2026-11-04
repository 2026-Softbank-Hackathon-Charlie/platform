# E2E: 부하 중 이전을 플랫폼 API로 돌리기

플랫폼 API, 게이트웨이, 엔진(API 프로세스 안), kind 안의 CNPG 클러스터 두 개를 붙이고, 게이트웨이로 쓰기와 읽기를 계속 보내면서 이전을 플랫폼 API로만 진행한다. 언제든 다시 돌릴 수 있게 한 명령으로 만들었다.

```bash
test/e2e/run.sh          # 실행하고 끝나면 전부 지운다
KEEP=1 test/e2e/run.sh   # 끝난 뒤에도 그대로 둔다 (로그, API, kind 확인용)
test/e2e/down.sh         # KEEP=1로 남긴 것, 중간에 끊긴 실행이 남긴 것을 지운다
```

필요한 것: docker, kubectl, go, curl. kind는 `go install sigs.k8s.io/kind@v0.33.0`으로 작업 디렉터리(`.hopcharlie/e2e/bin`)에만 설치하고, helm은 쓰지 않는다. Windows(Git Bash, Docker Desktop), Linux, macOS에서 같은 스크립트를 쓴다. 한 번 돌리는 데 4분 안팎 걸린다(처음에는 이미지와 Go 모듈을 받는 시간이 더 든다). `KEEP=1`로 남긴 kind 클러스터를 다시 쓰면 3분이 안 걸린다.

## 흐름

1. kind 클러스터 `hop-e2e` 하나에 CNPG 1.25.1 operator를 올리고, 원본 `pg-src`(primary)를 만든다. `items` 테이블에 2만 행을 넣는다. 매니페스트는 엔진의 `test/rehearsal`과 같고, 엔진 PR #4의 수정(`smartShutdownTimeout: 5`, 대상이 원본의 app Secret을 씀)이 들어 있다.
2. 플랫폼 저장소로 쓸 PostgreSQL 컨테이너 `hop-e2e-store`를 새로 띄운다.
3. 호스트에서 `go build`한 프로세스를 띄운다. 테스트 앱 두 개(`test/e2e/app`, 원본용과 대상용), 게이트웨이(backend `src`, `dst`, 앱 도메인 `e2e.test`), 플랫폼 API. 토큰과 비밀번호는 실행할 때마다 새로 만들고 파일에 남기지 않는다. 부하는 `127.0.0.1`로 보내므로 앱 도메인 밖이고, 이전은 기본 앱에서 일어난다.
4. 드라이버(`test/e2e/driver`)가 쓰기 4개, 읽기 4개를 게이트웨이로 계속 보낸다. 쓰기는 고유한 키로 행을 넣고, 성공한 키를 1초마다 `POST /v1/written-records/e2e`로 보낸다. 읽기는 성공한 키를 무작위로 다시 읽는다.
5. 드라이버가 플랫폼 API로만 이전을 진행한다.
   - agent 토큰: `POST /v1/plans`(`plan.json`), `PUT /v1/plans/{ref}/attachments/target.yaml`
   - operator 토큰: `GET /v1/plans/{ref}`에서 본 `revision`으로 승인, `POST /v1/migrations`(카나리 1%→100% 7단계, 단계마다 5초), SSE로 이벤트를 보다가 `awaiting_switch`가 되면 `/switch`
6. run이 끝나면 5초 더 부하를 보내고 멈춘 뒤, 기록한 키를 새 primary(`pg-dst`)에서 직접 센다. verify 단계 뒤에 들어온 쓰기까지 확인하기 위해서다.
7. 게이트웨이 관리 API로 두 번째 앱 `second`(환경 `blue`, 원본 테스트 앱)를 등록하고, `Host: second.e2e.test`가 `blue`로, `app.e2e.test`와 `127.0.0.1`이 기본 앱의 `dst`로 가는지, 등록하지 않은 이름이 404인지 본다. 앱을 지운 뒤에는 `second.e2e.test`도 404여야 한다.

## 실패 시나리오

`run.sh`는 같은 클러스터에서 시나리오 세 개를 차례로 돌린다(`SCENARIOS="canary-fault observe-rollback migrate"`가 기본, 일부만 고를 수 있다). 앞의 두 시나리오는 대상 테스트 앱에 일부러 장애를 넣고(`PUT /_fault`, 앱 자신의 포트로만 부른다), 끝나면 원본이 다시 primary라서 마지막의 정상 이전이 같은 상태에서 시작한다. 시나리오마다 plan id와 written-records 스트림(`e2e-<시나리오>`)이 따로다. 코드와 판정은 `driver/scenarios.go`.

| 시나리오 | 장애 | 기대 |
| --- | --- | --- |
| `canary-fault` | 카나리 10% 단계가 시작되면 대상의 모든 읽기가 500 | run `rolled_back`(카나리 단계), fence 전이라 쓰기는 옮겨지지 않음, 게이트웨이 쓰기·읽기 모두 `src`, 실패한 쓰기 0, 성공한 쓰기가 전부 원본에 있음 |
| `observe-rollback` | 전환 후 관찰(observe)이 시작되면 대상의 모든 요청이 1.2초 (p95 기준 800ms 초과), `autoRollback` 켬 | observe 위반, `slo-guard`가 시작한 롤백 run `succeeded`, 게이트웨이 쓰기·읽기 모두 `src`, 실패한 쓰기 0, 성공한 쓰기가 전부 원본(다시 primary)에 있음 |
| `migrate` | 없음 | 아래 통과 조건 |

카나리는 기본 1%→10%→25%→50%→75%→90%→100%(단계마다 5초)다. 1% 단계는 요청이 적어 `inconclusive`로 넘어가는 것이 정상이다.

## 통과 조건

다음을 모두 만족해야 종료 코드 0이다. 하나라도 어기면 `FAIL:` 줄을 찍고 1로 끝난다.

- run의 status가 `succeeded`
- run 결과의 `missingWrites`가 0 (엔진 verify가 written-records 스트림으로 확인한 값)
- 보낸 기록 수와 스트림의 기록 수가 같음
- 기록한 키가 새 primary에 전부 있음
- 7번의 호스트마다 상태 코드와 `X-Charlie-Backend`가 기대한 값

실패한 요청 수와 다운타임은 통과 조건이 아니고 결과로 찍는다.

## 결과 읽기

2026-11-02에 Windows 11, Docker Desktop에서 처음부터(kind 생성 포함) 돌린 결과다. 전체 4분 걸렸다.

```
run            e2e-src-to-dst-20261102-073919 status=succeeded
canary  10%    passed=true 56 requests, error rate 0.00%, p95 3ms
canary  50%    passed=true 367 requests, error rate 0.00%, p95 3ms
downtime       13.906s (writes held at the gateway, engine)
run duration   1m34.216s
gateway        requests=13069 failed=48 (during the run)
verify         verified=true missingRows=0 missingWrites=0 checkedWrites=6000
client writes  ok=6384 failed=0; longest gap without a successful write 13.936s, slowest write 13.888s
client reads   ok=7696 notFound=0 failed=48; by backend map[dst:1453 src:6291]
records        sent=6384 unsent=0 stream=6384
recheck        6384 of 6384 recorded writes are on the new primary
```

- `downtime`은 엔진이 잰 쓰기 보류 시간(게이트웨이 hold부터 release까지)이다. `client writes`의 가장 긴 공백과 거의 같아야 한다.
- `gateway`는 run 동안 게이트웨이가 센 요청과 5xx다.
- `checkedWrites`는 verify 직전까지 스트림에 들어온 기록 수다. 그 뒤의 쓰기는 `recheck`가 확인한다.
- `notFound`는 카나리 동안 대상(복제본)이 방금 쓴 행을 아직 받지 못한 경우라 실패로 세지 않는다. 실패로 세는 것은 5xx, 그 밖의 4xx, 연결 오류다.
- 지금은 전환할 때마다 읽기가 50건 안팎 실패한다. 엔진은 쓰기만 붙잡고, 읽기는 CNPG가 원본을 강등하며 재시작할 때와 대상을 승격한 뒤 재시작할 때도 계속 그 환경으로 간다. 게이트웨이 헬스 체크(2초 간격)가 알아챌 때까지 1초 안팎 동안 실패한다.

## 만드는 것과 포트

| 이름 | 무엇 |
| --- | --- |
| kind 클러스터 `hop-e2e` | 컨텍스트 `kind-hop-e2e`, kubeconfig는 `.hopcharlie/e2e/kubeconfig` (`~/.kube/config`는 건드리지 않음) |
| 컨테이너 `hop-e2e-store` | 플랫폼 저장소, 라벨 `hopcharlie-e2e` |
| `.hopcharlie/e2e/` | 바이너리, 로그(`api.log`, `gateway.log`, `app-src.log`, `app-dst.log`), 엔진 상태, PID 파일 |

포트는 모두 127.0.0.1에만 연다. `lib.sh`에 모아 두었고, DB 포트는 `kind.yaml`과 `plan.json`에도 있다.

| 포트 | 용도 |
| --- | --- |
| 55642, 55643 | `pg-src`, `pg-dst` (kind NodePort) |
| 55652 | 플랫폼 저장소 |
| 18080 | 플랫폼 API |
| 18000, 19901 | 게이트웨이 사용자 요청, 관리 API |
| 18101, 18102 | 테스트 앱 `src`, `dst` |

`down.sh`는 위의 것만 지운다. 저장소 컨테이너는 라벨이 붙은 것만 지우고, docker 네트워크 `kind`는 `run.sh`가 만들었고 아무도 쓰지 않을 때만 지운다.

## 실제 환경과 다른 점

- 두 CNPG 클러스터가 kind 클러스터 하나에 있다. 클러스터 사이 네트워크(Tailscale)와 서로 다른 Kubernetes 사이의 복제는 확인하지 않는다.
- 엔진과 테스트 앱은 kind NodePort로 DB에 붙는다. 그래서 `PLATFORM_DSN_HOST_SUFFIXES=127.0.0.1`이다.
- 파일 이전(rclone)과 Cloudflare Tunnel은 없다.
