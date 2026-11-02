# platform

HopCharlie 플랫폼 서버입니다. 바이너리 두 개로 이루어져 있습니다.

```
platform/
├── api/openapi.yaml   API 계약 (서버, CLI, 콘솔이 모두 이 파일을 따른다)
├── cmd/api/           플랫폼 API: Plan 접수, 사람 승인, 이전 실행, 이벤트 스트림, 환경, 감사 로그
├── cmd/gateway/       트래픽 게이트웨이: 쓰기 보류, 환경 전환, 읽기 가중치, 요청 통계
├── pkg/apiv1/         openapi.yaml에서 생성한 Go 타입 (CLI가 import)
├── pkg/client/        Go 클라이언트 (CLI가 import)
├── internal/
│   ├── api/           HTTP 핸들러, 역할 검사, 감사 로그, SSE
│   ├── auth/          토큰과 역할
│   ├── plans/         Plan 검증, 보안 정책, 승인과 거절
│   ├── runs/          이전 실행 단계 (동기화, 카나리, 전환 승인, 전환), 이벤트 저장
│   ├── envs/          환경 등록, 클러스터 실시간 상태
│   ├── gateway/       게이트웨이 본체와 관리 API 클라이언트
│   └── store/         PostgreSQL 저장소
├── examples/envs.yaml 환경 설정 예시
└── docs/adr/          설계 결정 기록
```

CLI는 별도 레포 [cli](https://github.com/2026-Softbank-Hackathon-Charlie/cli)에 있습니다.

## 이전 한 번의 흐름

```mermaid
sequenceDiagram
    participant AI as AI 플래너 (agent 토큰)
    participant Op as 운영자 (콘솔, CLI)
    participant API as 플랫폼 API
    participant Eng as migration-engine
    participant GW as 게이트웨이

    AI->>API: POST /v1/plans
    Op->>API: POST /v1/plans/{ref}/approve
    Op->>API: POST /v1/migrations {planRef, canary}
    API->>Eng: precheck, prepare, presync, delta
    API->>GW: 읽기 10%, 50%를 대상으로 (게이트 판정)
    API-->>Op: status awaiting_switch
    Op->>API: POST /v1/migrations/{id}/switch
    API->>Eng: fence ... finalize
    Eng->>GW: hold, route, release
    API-->>Op: SSE로 단계, 다운타임, 실패 요청 수, 누락 수
```

1. AI 플래너가 agent 토큰으로 Plan을 제출합니다. agent 토큰으로는 승인할 수 없습니다.
2. 운영자가 승인하면 실행을 시작할 수 있습니다.
3. 동기화 단계 동안 사용자는 계속 원본을 씁니다.
4. 카나리를 켜면 읽기 요청 일부를 대상으로 보내고, 대상의 오류율과 p95로 게이트를 판정합니다. 실패하면 읽기를 원본으로 돌리고 끝냅니다.
5. 데이터가 따라잡히면 `awaiting_switch`에서 운영자 승인을 기다립니다. `autoSwitch`를 켜면 기다리지 않습니다.
6. 전환 단계는 엔진이 실행합니다. 대상 승격을 요청하기 전에 실패하면 원본으로 자동 원복하고, 그 뒤에 실패하면 멈추고 사람을 기다립니다.

자세한 이유는 [docs/adr](docs/adr)에 있습니다.

## API

계약은 [api/openapi.yaml](api/openapi.yaml)입니다. 서버는 `GET /openapi.yaml`로 같은 파일을 내줍니다.

| 요청 | 역할 | 설명 |
| --- | --- | --- |
| `GET /v1/me` | viewer | 토큰 주인 확인 |
| `POST /v1/plans` | agent | 엔진 Plan JSON을 그대로 제출. 보안 정책 검사 후 `pending_approval` |
| `GET /v1/plans`, `GET /v1/plans/{ref}` | viewer | DSN과 토큰은 응답에 넣지 않음 |
| `POST /v1/plans/{ref}/approve`, `/reject` | operator | 사람의 승인 또는 거절 (거절은 사유 필수) |
| `POST /v1/migrations` | operator | 승인된 Plan 실행. 202 + runId. `Idempotency-Key` 지원 |
| `GET /v1/migrations`, `GET /v1/migrations/{runId}` | viewer | run 문서 (상태, 단계, 결과 숫자, 엔진 상태) |
| `GET /v1/migrations/{runId}/events` | viewer | SSE. `Last-Event-ID`로 재접속하면 빠진 이벤트를 다시 받음 |
| `POST /v1/migrations/{runId}/switch` | operator | 전환 승인 |
| `POST /v1/migrations/{runId}/abort` | operator | 중단. 결과는 엔진의 원복 경계를 따름 |
| `POST /v1/migrations/{runId}/resume` | operator | interrupted, failed, aborted run 이어서 실행 |
| `GET /v1/envs`, `GET /v1/envs/{name}` | viewer | 환경과 클러스터 실시간 상태 (노드, 버전, CNPG) |
| `GET /v1/traffic` | viewer | 게이트웨이 상태와 환경별 요청 통계 |
| `GET /v1/matrix` | viewer | 6개 경로 결과표 |
| `GET /v1/audit` | viewer | 감사 로그 (바꾸는 요청과 거부된 요청 전부) |

오류는 항상 `{"error": {"code": "...", "message": "..."}}` 형식입니다. 코드 목록은 명세의 `components.responses`에 있습니다.

### run 상태

| status | 뜻 |
| --- | --- |
| `running` | 단계를 실행하는 중 |
| `awaiting_switch` | 데이터가 따라잡힘. `/switch`를 기다림 |
| `succeeded` | 트래픽과 데이터가 대상으로 옮겨졌고 검증을 통과함 |
| `rolled_back` | 원본이 다시 트래픽을 받음. 잃은 데이터 없음 |
| `halted` | 대상 승격을 요청한 뒤에 실패함. 사람이 판단 |
| `failed`, `aborted` | 되돌릴 수 없는 일이 생기기 전에 멈춤. 재개 가능 |
| `interrupted` | 서버가 재시작됨. 재개 가능 |

### 콘솔에서 쓰기

```bash
npx openapi-typescript ../platform/api/openapi.yaml -o src/api/types.ts
```

브라우저의 `EventSource`는 Authorization 헤더를 붙일 수 없으므로, SSE는 fetch 기반 클라이언트(예: `@microsoft/fetch-event-source`)로 받습니다. 콘솔 주소는 `PLATFORM_CORS_ORIGINS`에 넣어야 합니다.

## 실행

엔진 레포가 private이라 Go 모듈을 받으려면 한 번 설정이 필요합니다.

```bash
go env -w GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie
```

로컬에서 PostgreSQL을 띄우고 API와 게이트웨이를 실행합니다.

```bash
docker run -d --name charlie-pg -e POSTGRES_USER=charlie -e POSTGRES_PASSWORD=charlie -p 55432:5432 postgres:17-alpine

export PLATFORM_DATABASE_URL='postgres://charlie:charlie@localhost:55432/charlie?sslmode=disable'
export PLATFORM_TOKENS='seohyun:operator:<32자 이상 임의 문자열>,planner:agent:<...>,screen:viewer:<...>'
export PLATFORM_ENVS_FILE=examples/envs.yaml
export PLATFORM_GATEWAY_URL=http://127.0.0.1:9901 GATEWAY_TOKEN=<게이트웨이 관리 토큰>
export SRC_PGPASSWORD=... DST_PGPASSWORD=...
go run ./cmd/api

export GATEWAY_ADMIN_TOKEN=$GATEWAY_TOKEN
export GATEWAY_BACKENDS='gke=http://shop.gke.ts.net,eks=http://shop.eks.ts.net,onprem=http://shop.onprem.ts.net'
go run ./cmd/gateway
```

### 플랫폼 API 환경 변수

| 변수 | 설명 |
| --- | --- |
| `PLATFORM_DATABASE_URL` | 필수. 플랫폼 DB. 시작할 때 스키마를 자동으로 적용 |
| `PLATFORM_TOKENS` | `이름:역할:토큰`을 쉼표로 구분. 역할은 viewer, agent, operator. 토큰은 16자 이상 |
| `PLATFORM_API_TOKEN` | 첫 버전 호환용. operator 토큰 하나로 추가됨 |
| `PLATFORM_ENVS_FILE` | 필수. 환경 설정 파일 ([examples/envs.yaml](examples/envs.yaml)) |
| `PLATFORM_GATEWAY_URL`, `GATEWAY_TOKEN` | 게이트웨이 관리 API 주소와 토큰. 비우면 카나리와 `/v1/traffic`이 꺼지고 `gateway.mode: http` Plan을 거부 |
| `PLATFORM_CORS_ORIGINS` | 콘솔 주소. 쉼표 구분, `*`는 전부 허용 |
| `PLATFORM_ADDR`, `PLATFORM_STATE_DIR` | 주소(기본 `:8080`), 엔진 상태 디렉터리 |
| `PLATFORM_PLAN_ENV_ALLOW` | Plan이 `${VAR}`로 참조할 수 있는 환경 변수 (기본 `SRC_PGPASSWORD`, `DST_PGPASSWORD`, `GATEWAY_TOKEN`) |
| `PLATFORM_DSN_HOST_SUFFIXES` | 허용할 DB 호스트 접미사 (예: `.ts.net`) |

환경 설정의 `kubeContext`는 서버의 kubeconfig에 있어야 합니다. 서버 계정에는 `/version` 조회, `nodes` 목록, 모든 네임스페이스의 `clusters.postgresql.cnpg.io` 목록 권한이 필요합니다. 클러스터에 닿지 못하면 그 환경은 `reachable: false`와 짧은 오류로 표시되고, 서버는 그대로 뜹니다.

서버를 끌 때는 전환 승인을 기다리던 run을 바로 `interrupted`로 바꾸고, 단계를 실행 중인 run은 끝날 때까지 기다립니다(`-shutdown-wait`, 기본 15분). 다시 켜면 실행 중이던 run은 `interrupted`로 표시되고, 운영자가 `resume`으로 이어 갑니다.

## 게이트웨이

사용자 요청은 Cloudflare Tunnel을 거쳐 게이트웨이로 들어오고, 게이트웨이가 환경 하나로 보냅니다.

- 쓰기(GET, HEAD, OPTIONS가 아닌 요청)는 항상 `writeBackend` 한 곳으로 갑니다.
- 읽기는 `readWeights` 비율대로 나뉘고, 헬스 체크가 실패한 환경은 건너뜁니다.
- 응답에는 처리한 환경 이름이 `X-Charlie-Backend` 헤더로 붙습니다.
- cloudflared가 보낸 `X-Forwarded-Proto`(보통 `https`)는 바꾸지 않고 환경에 넘깁니다.
- 쓰기는 사용자가 연결을 끊어도 취소하지 않습니다. 환경이 답할 때까지(최대 `-write-timeout`) 처리 중으로 세므로, hold는 환경이 실제로 끝낸 뒤에 200을 돌려줍니다.

관리 API (`GATEWAY_ADMIN_TOKEN`, `/healthz`와 `/metrics`는 토큰 없이):

| 요청 | 설명 |
| --- | --- |
| `GET /healthz` | 설정을 읽었으면 200 |
| `POST /v1/writes/hold` | 새 쓰기를 대기시키고, 처리 중인 쓰기가 끝나면 200. `-drain-timeout` 안에 끝나지 않거나 호출이 끊기면 보류를 되돌리고(대기 중이던 쓰기는 지금의 `writeBackend`로 보냄) 504 `drain_timeout`. 기다리는 사이 release가 오면 409 `hold_cancelled`. 200이 아니면 쓰기는 보류되지 않은 상태입니다 |
| `POST /v1/writes/release` | 대기 중인 쓰기를 현재 `writeBackend`로 보냄. 보류는 release로만 끝남 |
| `POST /v1/route {"backend":"eks"}` | 쓰기와 읽기 100%를 그 환경으로 |
| `POST /v1/weights {"readWeights":{...}}` | 읽기 비율만 바꿈 (합 100) |
| `GET /v1/state?window=10s` | `TrafficState`: 환경별 요청 수, 오류율, p50/p95/p99, 누적 수. 지난 10초와 지금 진행 중인 초를 합쳐 세고, rps는 그 실제 길이로 나눔 |
| `GET /metrics` | Prometheus 지표 (`charlie_gateway_*`) |

| 변수 (플래그) | 설명 |
| --- | --- |
| `GATEWAY_BACKENDS` (`-backends`) | `이름=URL`을 쉼표로 구분 |
| `GATEWAY_ADMIN_TOKEN` | 관리 API 토큰. 없으면 `-insecure-no-auth` 없이는 시작하지 않음 |
| `GATEWAY_LISTEN` (`-listen`), `GATEWAY_ADMIN_ADDR` (`-admin-addr`) | 사용자 요청 주소(기본 `:8000`), 관리 주소(기본 `:9901`) |
| `GATEWAY_INITIAL_BACKEND` (`-initial-backend`) | 상태 파일이 없을 때 처음 쓸 환경 (기본: 첫 번째) |
| `GATEWAY_STATE_FILE` (`-state-file`) | 상태 파일 (기본 `.hopcharlie/gateway-state.json`) |
| `-max-hold` | 쓰기 하나가 보류 중에 기다리는 최대 시간 (기본 90s, Cloudflare의 원본 응답 제한 약 100초보다 짧게). 넘긴 쓰기는 보내지 않고 503 `hold_timeout`(`Retry-After: 1`)으로 답하고 `charlie_gateway_hold_expired_total`에 셈. 보류 자체는 release까지 이어짐 |
| `-drain-timeout` | hold가 처리 중인 쓰기를 기다리는 시간 (기본 30s) |
| `-max-held` | 대기 쓰기 상한 (기본 2000). 쓰기마다 본문을 64KiB까지 메모리에 두며, 본문을 읽기 전에 자리를 잡음. 넘으면 503 `too_many_held_writes` |
| `-write-timeout` | 쓰기 하나를 환경에 보내고 기다리는 최대 시간 (기본 2m). 넘으면 504 `backend_timeout` |

종료와 재시작:

- SIGINT나 SIGTERM을 받으면 대기 중인 쓰기와 그 뒤에 들어온 보류 대상 쓰기에 503 `shutting_down`(`Retry-After: 1`)으로 답하고 환경에는 보내지 않습니다. 사용자 요청 서버가 처리 중인 요청을 끝낸 뒤에 관리 API를 닫습니다.
- 보류 중이었다면 상태 파일에 보류가 그대로 남고, 다시 시작해도 보류가 이어집니다. 시간이 지나도 저절로 풀리지 않으므로 엔진의 롤백이나 운영자가 release를 불러야 쓰기가 다시 흐릅니다.
- 설정에서 뺀 환경이 상태 파일에 읽기 비율 0으로 남아 있으면 지우고 시작합니다. 그 환경이 `writeBackend`이거나 읽기 비율이 0보다 크면 시작하지 않습니다.

## 테스트

```bash
go test ./...
PLATFORM_TEST_DATABASE_URL='postgres://charlie:charlie@localhost:55432/charlie?sslmode=disable' go test ./internal/store/...
```

- API 테스트는 모든 응답과 SSE 메시지를 `api/openapi.yaml`로 검증합니다. 명세와 구현이 어긋나면 실패합니다.
- 저장소 구현은 `storetest.Conformance`를 통과해야 합니다. 메모리 저장소(`storetest.NewMemory`)는 테스트에서만 씁니다.
- `api/openapi.yaml`을 고친 뒤에는 `go generate ./pkg/apiv1`로 타입을 다시 만듭니다.

## 알려진 한계

- 게이트웨이는 한 대뿐이라 단일 장애점입니다. 상태 파일 기반이라 두 대로 늘리는 것은 가능하지만 아직 하지 않았습니다.
- 인증은 정적 토큰입니다. 운영 환경이라면 OIDC로 토큰을 발급해야 합니다.
- 클러스터 안에서 게이트웨이를 거치지 않고 DB에 쓰는 작업은 전환 중에 막을 수 없습니다. 이전하는 동안 멈춰야 합니다.
- 실제 클러스터에서 끝까지 이전해 본 것은 아직 없습니다. 클러스터 3개가 준비되면 엔진과 함께 리허설합니다.
