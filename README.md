# platform

HopCharlie 플랫폼 서버입니다. 바이너리 두 개로 이루어져 있습니다.

```
platform/
├── api/openapi.yaml   API 계약 (서버, CLI, 콘솔이 모두 이 파일을 따른다)
├── cmd/api/           플랫폼 API: Plan 접수, 사람 승인, 이전 실행, 이벤트 스트림, 환경, 감사 로그
├── cmd/gateway/       트래픽 게이트웨이: 쓰기 보류, 환경 전환, 읽기 가중치, 요청 통계
├── pkg/apiv1/         openapi.yaml에서 생성한 Go 타입 (CLI가 import)
├── pkg/client/        Go 클라이언트 (CLI가 import)
├── pkg/appspec/       앱 저장소의 platform.yaml 해석과 검사 (CLI가 import)
├── internal/
│   ├── api/           HTTP 핸들러, 역할 검사, 감사 로그, SSE
│   ├── auth/          토큰과 역할
│   ├── plans/         Plan 검증, 보안 정책, 승인과 거절
│   ├── runs/          이전 실행 단계 (동기화, 카나리, 전환 승인, 전환, 관찰), 이벤트 저장
│   ├── envs/          환경 등록, 클러스터 실시간 상태
│   ├── gateway/       게이트웨이 본체와 관리 API 클라이언트
│   ├── deploy/        GitHub 앱 배포 실행기 (빌드, gitops 커밋, Argo CD 대기, 게이트웨이 등록)
│   └── store/         PostgreSQL 저장소
├── examples/envs.yaml 로컬 개발용 환경 설정 예시
└── docs/adr/          설계 결정 기록
```

CLI는 별도 레포 [cli](https://github.com/2026-Softbank-Hackathon-Charlie/cli)에 있습니다.

이전 전 동등성·레지던시 검사는 [internal/preflight](internal/preflight/README.md)에 있습니다. **레지던시는 `PLATFORM_RESIDENCY_POLICY_FILE`을 설정하면 새 run 시작을 막는 게이트로 동작합니다**([ADR 0013](docs/adr/0013-residency-start-gate.md)). 동등성은 `PLATFORM_PARITY_FILE`을 설정하면 새 run 시작 때 대상의 RBAC와 Prometheus를 실제로 검사해 감사 로그에 남깁니다. 기록만 하고 막지 않습니다([ADR 0022](docs/adr/0022-parity-audit-mode.md)). 구현된 검사와 미검증 범위는 해당 문서에 구분합니다.

AWS 관리 호스트, EKS Pod Identity, 상태 버킷(`state/`), ACM·Cloudflare(`dns/`)의 Terraform과 운영 호스트 설정은 [infra/terraform/platform/aws](https://github.com/2026-Softbank-Hackathon-Charlie/infra/tree/main/terraform/platform/aws)에서 관리합니다. 이 레포에는 [애플리케이션 배포 도우미](deploy/aws/README.md)와 로컬 개발 예시만 남습니다. `examples/envs.yaml`, `examples/apps.yaml`을 고쳐도 운영 설정은 바뀌지 않습니다. 운영 설정은 infra의 `terraform/platform/aws/host/envs.yaml`, `host/apps.yaml`을 고칩니다.

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
    API->>GW: 읽기 1%→10%→25%→50%→75%→90%→100%를 대상으로 (단계마다 게이트 판정)
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
7. `observe`를 켜면 전환 뒤에도 대상의 오류율과 p95를 관찰합니다. 위반하면 기록하고, `autoRollback`이면 `slo-guard`가 1클릭 롤백과 같은 경로로 되돌립니다. 요청이 부족하거나 게이트웨이에 닿지 않는 구간은 위반으로 보지 않습니다 (ADR 0009).

자세한 이유는 [docs/adr](docs/adr)에 있습니다.

## API

계약은 [api/openapi.yaml](api/openapi.yaml)입니다. 서버는 `GET /openapi.yaml`로 같은 파일을 내줍니다.

| 요청 | 역할 | 설명 |
| --- | --- | --- |
| `GET /v1/me` | viewer | 토큰 주인 확인 |
| `POST /v1/auth/login` | 없음 | 관리자(`PLATFORM_ADMINS`) 아이디와 비밀번호로 콘솔 세션(operator, 12시간)을 받음. 이름마다 10분에 10번 실패하면 429 |
| `POST /v1/auth/logout` | viewer | 요청한 콘솔 세션이나 CLI 토큰을 끝냄. 정적 토큰은 그대로 |
| `POST /v1/auth/device`, `POST /v1/auth/device/token` | 없음 | CLI의 device code 로그인 시작과 폴링. 승인되면 CLI 토큰(승인한 관리자 이름과 고른 역할, 30일) |
| `GET /v1/auth/device/{userCode}`, `/approve`, `/deny` | operator, 콘솔 세션 | 기기 로그인 조회, 역할을 골라 승인(10분 더 교환 가능), 거절. CLI 토큰과 정적 토큰은 403 `console_session_required` |
| `POST /v1/plans` | agent | 엔진 Plan JSON을 그대로 제출. 보안 정책 검사 후 `pending_approval` |
| `GET /v1/plans`, `GET /v1/plans/{ref}` | viewer | DSN과 토큰은 응답에 넣지 않음 |
| `POST /v1/plans/{ref}/approve`, `/reject` | operator | 사람의 승인 또는 거절 (거절은 사유 필수, 첨부가 있는 Plan의 승인은 검토한 `revision` 필수) |
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
| `PLATFORM_ADMINS` | 콘솔에 아이디와 비밀번호로 로그인하는 관리자. `이름:bcrypt 해시`를 쉼표로 구분 ([해시 만들기](#관리자-비밀번호-해시), [ADR 0018](docs/adr/0018-admin-login-device-code.md)). 모두 operator. 이름 규칙은 토큰과 같고 로그인할 때 대소문자를 가리지 않음. 해시가 bcrypt가 아니거나, cost가 10에서 14 사이가 아니거나, 관리자마다 cost가 다르면 서버가 시작하지 않음. `PLATFORM_TOKENS`와 둘 다 비면 `-insecure-no-auth` 없이는 시작하지 않음 |
| `PLATFORM_CONSOLE_URL` | 콘솔 주소 (기본 `https://console.hopcharlie.dev`). CLI device 로그인이 사람을 `<주소>/#device`로 안내함 |
| `PLATFORM_ENVS_FILE` | 필수. 환경 설정 파일 ([로컬 개발 예시](examples/envs.yaml); 운영 설정은 infra에서 관리). 환경마다 Sealed Secrets 인증서(`sealedSecretsCert`)와 저장소 드라이버(`storage`)를 둘 수 있음 ([버킷과 키 드라이버](#버킷과-키-드라이버)) |
| `PLATFORM_SECRETS_KEY` | 앱 시크릿을 암호화하는 키. 32바이트를 base64로 ([앱 시크릿](#앱-시크릿), [ADR 0019](docs/adr/0019-app-secrets-and-redeploy.md)). 비우면 시크릿 API가 503 `secrets_not_configured`. base64가 아니거나 32바이트가 아니면 서버가 시작하지 않음 |
| `PLATFORM_GATEWAY_URL`, `GATEWAY_TOKEN` | 게이트웨이 관리 API 주소와 토큰. 비우면 카나리와 `/v1/traffic`이 꺼지고 `gateway.mode: http` Plan을 거부 |
| `PLATFORM_CORS_ORIGINS` | 콘솔 주소. 쉼표 구분, `*`는 전부 허용 |
| `PLATFORM_ADDR`, `PLATFORM_STATE_DIR` | 주소(기본 `:8080`), 엔진 상태 디렉터리 |
| `PLATFORM_PLAN_ENV_ALLOW` | 어느 Plan이든 두 DSN의 비밀번호 자리에 `${VAR}`로 쓸 수 있는 변수 (기본 `SRC_PGPASSWORD`, `DST_PGPASSWORD`). 이름은 `_PGPASSWORD`로 끝나야 하고 `PLATFORM_`, `AWS_`로 시작하면 안 되며, 어기면 서버가 시작하지 않음 |
| `PLATFORM_DSN_HOST_SUFFIXES` | 허용할 DB 호스트 접미사 (예: `.ts.net`) |
| `PLATFORM_PLAN_BUCKETS` | 앱 설정 말고도 Plan의 `files`에 쓸 수 있는 버킷 (`원격:버킷`, 쉼표 구분). 앱 설정의 `bucket`은 자동으로 들어감 |
| `RCLONE_CONFIG_<원격>_*` | rclone 원격의 종류와 자격 증명 (예: `RCLONE_CONFIG_GCS_TYPE=google cloud storage`). 등록된 버킷의 원격에 `_TYPE`이 없으면 시작할 때 경고 |
| `PLATFORM_AWS_REGION` | 보안 뷰가 읽는 AWS 리전 (기본 `ap-northeast-2`) |
| `PLATFORM_AGENT_IDLE` | 이벤트 없이 이 시간이 지난 `running` 에이전트 세션을 `cancelled`로 닫음 (기본 `30m`) |
| `PLATFORM_RESIDENCY_POLICY_FILE` | 레지던시 정책 파일 ([예시](examples/residency-policy.yaml), [ADR 0013](docs/adr/0013-residency-start-gate.md)). 설정하면 새 run을 시작하기 전에 원본 전체 테이블 목록을 수집해 검사하고, 위반이면 409 `residency_denied`. `files`는 정책의 `buckets`에 등록된 버킷끼리만, 그리고 서버가 rclone 원격으로 측정한 위치·엔드포인트·프리픽스·복제 대상이 정책과 맞을 때만 통과([ADR 0021](docs/adr/0021-bucket-residency-measured.md)). 버킷마다 `expect`가 없으면 거부. 롤백은 면제. 비우면 검사 없음(시작 시 경고) |
| `PLATFORM_CAPACITY_GATE` | `on`이면 새 run을 시작하기 전에 원본·대상 DB를 몇 초 동안 측정해, 대상 접속 한도가 부족하거나 replica가 `catchupTimeout` 안에 따라잡지 못하면 409 `capacity_insufficient` ([ADR 0014](docs/adr/0014-capacity-preflight.md)). 측정하지 못한 값은 막지 않음. 롤백은 면제. 기본 `off` |
| `PLATFORM_PARITY_FILE` | 동등성 파일 ([예시](examples/parity.yaml), [ADR 0022](docs/adr/0022-parity-audit-mode.md)). 설정하면 새 run 시작 직전에 대상 클러스터에서 앱 ServiceAccount의 SubjectAccessReview와 대상 Prometheus의 scrape 상태를 검사해 감사 로그 `parity`에 남김. **막지 않음**(mode `audit`만 있음). secret·audit·backup-restore는 프로브가 없어 항상 `probe_not_configured`. 롤백은 면제. 비우면 기록 없음 |
| `PLATFORM_VERSION_GATE` | 새 run을 시작하기 전에 원본과 대상의 PostgreSQL 메이저 버전을 비교해, 다르면 409 `version_mismatch` ([ADR 0020](docs/adr/0020-pg-major-version-gate.md)). 대상은 DB에 접속되면 그 값, 아니면 manifest의 `spec.imageCatalogRef.major`나 `spec.imageName` 태그. 읽지 못하면 unknown으로 기록하고 막지 않음. 롤백은 면제. 기본 `on`, `off`로 끔 |
| `PLATFORM_GITHUB_*`, `PLATFORM_APP_DOMAIN` | 앱을 등록하고 배포하는 GitHub App. [GitHub App 설정](#github-app-설정) 참고 |

### 관리자 비밀번호 해시

`PLATFORM_ADMINS`에 넣을 해시는 API 바이너리로 만듭니다. DB나 토큰 설정 없이 동작하고, 해시 한 줄(bcrypt, cost 12)만 출력합니다.

```bash
go run ./cmd/api -hash-password          # 터미널이면 비밀번호를 화면에 보이지 않게 입력받음
printf '%s' "$PW" | go run ./cmd/api -hash-password   # 파이프로 넣으면 첫 줄만 읽음

export PLATFORM_ADMINS='seohyun:$2a$12$...,minji:$2a$12$...'
```

- 해시에 `$`가 들어 있으므로 셸에서는 작은따옴표로 감쌉니다. infra의 환경 변수 로더는 값을 펼치지 않고 그대로 넘기므로 따로 이스케이프하지 않습니다.
- 비밀번호는 72바이트까지입니다(bcrypt가 읽는 길이). 빈 비밀번호와 더 긴 비밀번호는 거부합니다.
- Git Bash(mintty)에서는 표준 입력이 터미널이 아니어서 입력한 비밀번호가 화면에 보입니다. 이때는 `stdin is not a terminal` 안내가 나옵니다. PowerShell에서 실행하거나, Git Bash에서는 `winpty go run ./cmd/api -hash-password`로 실행합니다.

### Plan의 환경 변수 참조

엔진은 `source.dsn`, `target.dsn`, `gateway.url`, `gateway.token`에서 서버 환경 변수를 펼칩니다. 서버는 JSON을 해석한 뒤, 펼치기 전에 이 네 필드를 하나씩 검사합니다. JSON 유니코드 이스케이프로 쓴 `$`도 해석한 뒤라 그대로 걸립니다.

- DSN은 `postgres://` 또는 `postgresql://` URL이어야 합니다. 참조는 비밀번호 전체 자리 하나에만 둘 수 있습니다(`postgres://app:${SRC_PGPASSWORD}@host:5432/db?sslmode=require`). 사용자, 호스트, 포트, 경로, 쿼리에는 `$`를 쓸 수 없습니다. 비밀번호 참조가 없는 DSN도 됩니다.
- `PLATFORM_PLAN_ENV_ALLOW`의 변수는 어느 DSN에든 쓸 수 있습니다. 롤백이 두 DSN을 맞바꾸기 때문입니다.
- 앱 설정의 `passwordEnv`는 그 환경의 `dbHost:dbPort`로 가는 DSN에만 쓸 수 있습니다. 다른 앱이나 다른 환경의 DSN, 쿼리의 `host=`나 두 번째 호스트로 보내면 거부합니다.
- `gateway.url`에는 `$`를 쓸 수 없고, `gateway.token`은 비우거나 정확히 `${GATEWAY_TOKEN}`이어야 합니다.
- Plan 요약의 `dbHost`와 정책 오류 메시지는 펼치기 전의 Plan에서 만듭니다. 펼친 값은 응답에 나오지 않습니다.

### Plan의 파일 이전

rclone은 경로의 원격 이름 자리에서 저장소 정의를 바로 읽습니다(`:local:/data`, `:s3,access_key_id=...:버킷`, `gcs,service_account_file=...:버킷`). 그래서 서버는 `files.source`와 `files.target`이 각각 등록된 버킷과 정확히 같을 때만 받습니다.

- 등록된 버킷은 앱 설정 파일의 `bucket`과 `PLATFORM_PLAN_BUCKETS`입니다. 형식은 `원격:버킷` 또는 `원격:버킷/경로`이고, 원격 이름 앞의 `:`, 쉼표, `..`는 쓸 수 없습니다. 형식이 틀리면 서버가 시작하지 않습니다.
- 등록된 버킷이 하나도 없으면 `files`가 있는 Plan은 모두 거부합니다. 원본과 대상이 같은 버킷이어도 거부합니다.
- 원격의 종류와 자격 증명은 서버 환경 변수 `RCLONE_CONFIG_<원격>_*`로만 줍니다. 원격 이름은 대문자로 바꾸고 `-`를 `_`로 바꿉니다(`my-remote`는 `RCLONE_CONFIG_MY_REMOTE_TYPE`).
- `files.rclone`은 비우거나 `rclone`이어야 하고, 플래그는 `--checksum`, `--size-only`, `--fast-list`, `--transfers`, `--checkers`만 받습니다.

환경 설정의 `kubeContext`는 서버의 kubeconfig에 있어야 합니다. 서버 계정에는 `/version` 조회, `nodes` 목록, 모든 네임스페이스의 `clusters.postgresql.cnpg.io` 목록 권한이 필요합니다. 클러스터에 닿지 못하면 그 환경은 `reachable: false`와 짧은 오류로 표시되고, 서버는 그대로 뜹니다.

서버를 끌 때는 전환 승인을 기다리던 run을 바로 `interrupted`로 바꾸고, 단계를 실행 중인 run은 끝날 때까지 기다립니다(`-shutdown-wait`, 기본 15분). 다시 켜면 실행 중이던 run은 `interrupted`로 표시되고, 운영자가 `resume`으로 이어 갑니다.

## 게이트웨이

사용자 요청은 Cloudflare Tunnel을 거쳐 게이트웨이로 들어오고, 게이트웨이가 환경 하나로 보냅니다.

- 쓰기(GET, HEAD, OPTIONS가 아닌 요청)는 항상 `writeBackend` 한 곳으로 갑니다.
- 읽기는 `readWeights` 비율대로 나뉘고, 헬스 체크가 실패한 환경은 건너뜁니다.
- 헬스 체크는 프록시와 연결을 공유하지 않고 매번 새로 연결합니다(keep-alive 없음). 그래서 이름이 해석되지 않거나 주소가 바뀐 것을 한 주기 안에 알아차립니다. 이름이 해석되지 않으면 `GET /v1/state`의 `lastError`가 `dns: `로 시작합니다.
- 전환은 공개 DNS를 바꾸지 않고 게이트웨이 안에서 요청마다 환경을 고르므로, 사용자 쪽 DNS 캐시나 TTL은 전환에 영향을 주지 않습니다.
- 본문 없는 GET과 HEAD가 500, 502, 503, 504를 받거나 환경이 답하지 않으면, 다른 환경으로 한 번 더 보냅니다. 사용자는 두 번째 응답만 받습니다. 전환 중에 원본 DB의 demote나 대상 DB의 재시작 때문에 몇 초 동안 나는 읽기 실패를 사용자에게 보이지 않기 위해서입니다.
  - 다시 보낼 곳은 읽기 비율이 있는 다른 환경, 없으면 `writeBackend`입니다. 쓰기를 붙잡고 있는 동안에는 읽기 비율이 0인 다른 정상 환경도 됩니다. 그 밖의 때에 비율 0인 환경은 아직 데이터가 없을 수 있으므로 보내지 않습니다.
  - 다시 보낼 곳이 없으면 첫 응답을 그대로 돌려줍니다. 쓰기와 프로토콜 업그레이드는 다시 보내지 않습니다.
  - `/v1/state`의 `stats`(구간 통계, 카나리 게이트가 씀)에는 첫 실패도 오류로 들어갑니다. `totals`(run의 실패 요청 수가 씀)에는 사용자가 받은 결과만 들어가고, 다시 보낸 첫 시도는 `retried`로 따로 셉니다. Prometheus에는 `charlie_gateway_read_retries_total`이 있습니다.
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
| `GET /apps` | `{"domain","apps":[...]}`. 기본 앱이 먼저, 등록한 앱은 이름순. 앱마다 `name`, `default`, `host`, `backends`(비밀번호를 가린 주소), `writeBackend`, `readWeights`, `writesHeld` |
| `PUT /apps/{app} {"backends":{"gke":"http://shop.gke.ts.net"},"writeBackend":"gke","readWeights":{"gke":100}}` | 앱을 등록하면 201, 교체하면 200, 본문은 목록의 앱 하나와 같음. `readWeights`를 빼면 읽기도 전부 `writeBackend`로. 그 앱의 쓰기를 보류하는 동안 409 `writes_held`, 기본 앱 이름이면 409 `default_app` |
| `DELETE /apps/{app}` | 등록한 앱을 지움(204). 보류하는 동안 409 `writes_held`, 없으면 404 `unknown_app` |
| `/apps/{app}/healthz`, `/apps/{app}/v1/state`, `/apps/{app}/v1/writes/hold`, `/apps/{app}/v1/writes/release`, `/apps/{app}/v1/route`, `/apps/{app}/v1/weights` | 위의 같은 요청을 그 앱에만. `/apps/{app}/healthz`도 토큰이 필요하고, 없는 앱이면 404 `unknown_app` |

| 변수 (플래그) | 설명 |
| --- | --- |
| `GATEWAY_BACKENDS` (`-backends`) | `이름=URL`을 쉼표로 구분 |
| `GATEWAY_ADMIN_TOKEN` | 관리 API 토큰. 없으면 `-insecure-no-auth` 없이는 시작하지 않음 |
| `GATEWAY_LISTEN` (`-listen`), `GATEWAY_ADMIN_ADDR` (`-admin-addr`) | 사용자 요청 주소(기본 `:8000`), 관리 주소(기본 `:9901`) |
| `GATEWAY_INITIAL_BACKEND` (`-initial-backend`) | 상태 파일이 없을 때 처음 쓸 환경 (기본: 첫 번째) |
| `GATEWAY_STATE_FILE` (`-state-file`) | 상태 파일 (기본 `.hopcharlie/gateway-state.json`) |
| `GATEWAY_APP_DOMAIN` (`-app-domain`) | 앱 도메인 (예: `hopcharlie.dev`). 비우면(기본) 호스트와 관계없이 모든 요청이 기본 앱으로 감 |
| `GATEWAY_DEFAULT_APP` (`-default-app`) | `-backends`가 서빙하는 기본 앱의 이름 (기본 `app`, 그래서 `app.hopcharlie.dev`가 기본 앱) |
| `-max-hold` | 쓰기 하나가 보류 중에 기다리는 최대 시간 (기본 55s). 넘긴 쓰기는 보내지 않고 503 `hold_timeout`(`Retry-After: 1`)으로 답하고 `charlie_gateway_hold_expired_total`에 셈. 보류 자체는 release까지 이어짐. **앞단 프록시의 유휴 타임아웃보다 짧아야 합니다.** 그래야 오래 기다린 쓰기가 프록시의 504가 아니라 다시 시도할 수 있는 이 503을 받습니다. AWS ALB는 기본 60초, Cloudflare 원본 응답 제한은 약 100초입니다. 더 길게 보류하려면 이 값과 프록시 타임아웃을 함께 올립니다 |
| `-conn-recycle` | 환경으로 가는 유휴 연결을 닫는 주기 (기본 30s). 다음 요청은 새로 연결하면서 이름을 다시 해석하므로, 환경의 주소가 바뀌어도 옛 주소에 붙은 연결이 오래 남지 않음. 환경의 헬스 상태가 바뀔 때도 바로 닫음 |
| `-drain-timeout` | hold가 처리 중인 쓰기를 기다리는 시간 (기본 30s) |
| `-max-held` | 대기 쓰기 상한 (기본 2000). 쓰기마다 본문을 64KiB까지 메모리에 두며, 본문을 읽기 전에 자리를 잡음. 넘으면 503 `too_many_held_writes` |
| `-write-timeout` | 쓰기 하나를 환경에 보내고 기다리는 최대 시간 (기본 2m). 넘으면 504 `backend_timeout` |

종료와 재시작:

- SIGINT나 SIGTERM을 받으면 대기 중인 쓰기와 그 뒤에 들어온 보류 대상 쓰기에 503 `shutting_down`(`Retry-After: 1`)으로 답하고 환경에는 보내지 않습니다. 사용자 요청 서버가 처리 중인 요청을 끝낸 뒤에 관리 API를 닫습니다.
- 보류 중이었다면 상태 파일에 보류가 그대로 남고, 다시 시작해도 보류가 이어집니다. 시간이 지나도 저절로 풀리지 않으므로 엔진의 롤백이나 운영자가 release를 불러야 쓰기가 다시 흐릅니다.
- 설정에서 뺀 환경이 상태 파일에 읽기 비율 0으로 남아 있으면 지우고 시작합니다. 그 환경이 `writeBackend`이거나 읽기 비율이 0보다 크면 시작하지 않습니다.

앱마다 따로 라우팅하기 (ADR 0012):

- `-backends`의 환경이 기본 앱입니다. `/v1/...` 관리 API, `gateway.Client`의 기존 메서드, 기존 지표는 모두 기본 앱만 다룹니다. 플랫폼 API(`internal/runs`)도 기본 앱을 이전합니다.
- 다른 앱은 `PUT /apps/{app}`으로 등록합니다. 앱마다 환경, 쓰기 환경, 읽기 비율, 쓰기 보류, 통계가 따로 있어서, 한 앱의 hold는 다른 앱의 쓰기를 기다리지도 붙잡지도 않습니다. 이름은 DNS 레이블 하나(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, 63자까지)입니다. 플랫폼은 브랜치 인스턴스(`moimroom--dev`)도 이 이름으로 등록합니다.
- `-app-domain`을 정하면 `Host`가 `<앱>.<도메인>`인 요청(대소문자와 포트 무시)이 그 앱으로 갑니다. `<기본 앱 이름>.<도메인>`은 기본 앱입니다. 도메인 아래인데 등록하지 않은 이름이면 404 `unknown_app`으로 답하고 어느 환경에도 보내지 않습니다. 오타가 다른 앱의 환경에 닿지 않게 하기 위해서입니다. 도메인 밖의 호스트(IP, 다른 도메인, 도메인 자체)는 기본 앱으로 갑니다.
- `<관리 주소>/apps/<앱>`을 게이트웨이 주소로 쓰면 엔진의 HTTP 클라이언트가 그 앱에 hold, route, release를 그대로 부릅니다. Go에서는 `client.App("shop")`이 그 주소를 쓰는 `*gateway.Client`를 돌려주고, `client.Apps`, `client.PutApp(ctx, "shop", gateway.AppSpec{...})`, `client.DeleteApp`로 앱을 관리합니다.
- 상태 파일에는 기본 앱을 예전과 같은 최상위 필드에, 등록한 앱을 `apps`에 환경 주소와 함께 씁니다. 예전 상태 파일은 그대로 읽고, 앱을 등록하지 않으면 파일 내용도 예전과 같습니다. 재시작하면 앱마다 쓰기 환경, 읽기 비율, 보류가 기본 앱과 같은 규칙으로 이어집니다.
- 등록한 앱의 지표는 `charlie_gateway_app_*`이고 `app` 라벨이 맨 앞에 붙습니다(예: `charlie_gateway_app_requests_total{app="shop",backend="gke",class="read",code="200"}`). 기본 앱의 `charlie_gateway_*`에는 등록한 앱의 요청이 들어가지 않습니다. 앱을 지우면 그 앱의 시계열도 지웁니다.
- 등록한 앱의 헬스 체크도 `-health-path`를 씁니다.

## 테스트

```bash
go test ./...
PLATFORM_TEST_DATABASE_URL='postgres://charlie:charlie@localhost:55432/charlie?sslmode=disable' go test ./internal/store/...
```

- API 테스트는 모든 응답과 SSE 메시지를 `api/openapi.yaml`로 검증합니다. 명세와 구현이 어긋나면 실패합니다.
- 저장소 구현은 `storetest.Conformance`를 통과해야 합니다. 메모리 저장소(`storetest.NewMemory`)는 테스트에서만 씁니다.
- `api/openapi.yaml`을 고친 뒤에는 `go generate ./pkg/apiv1`로 타입을 다시 만듭니다.
- `minio` 드라이버 테스트는 `-tags minio`가 필요합니다. 실제 MinIO에 돌려 보려면 `PLATFORM_TEST_MINIO_ENDPOINT=http://127.0.0.1:9000 PLATFORM_TEST_MINIO_ACCESS_KEY=<관리 키> PLATFORM_TEST_MINIO_SECRET_KEY=<관리 비밀 키> go test -tags minio -run TestMinIOLive ./internal/storage`를 실행합니다. 버킷 두 개를 만들고, 한 키가 다른 버킷에 쓰지 못하는지 본 뒤 모두 지웁니다.

### CI

PR과 main push마다 GitHub Actions(`.github/workflows/ci.yml`)가 vet, gofmt, 생성 코드 확인, `go test -race`(PostgreSQL 포함, `test/charts`의 helm lint와 helm template 포함), 셸 `bash -n`, AWS 접속 없는 Terraform output 호환 테스트를 돌립니다. Terraform fmt와 validate는 infra 레포 CI가 담당합니다.

- PR 검사는 1분 안에 끝나는 것이 목표입니다. main push만 Go 캐시를 저장하고, PR은 그 캐시로 바뀐 패키지만 다시 컴파일합니다. 테스트도 입력이 바뀐 패키지만 다시 돌립니다(go test 결과 캐시).
- `go.sum`이 바뀐 PR도 main의 최근 캐시에서 시작하므로, 바뀐 의존성만 새로 컴파일하는 만큼 더 걸립니다. 캐시가 전혀 없을 때(새 레포, 캐시 만료)는 client-go와 AWS SDK를 처음부터 컴파일하느라 약 10분 걸립니다.
- private 모듈(migration-engine)을 받기 위해 레포 secret `MODULE_READ_TOKEN`(Contents read 권한의 fine-grained 토큰)을 씁니다.

### 부하 중 이전 E2E

kind 안의 CNPG 클러스터 두 개를 게이트웨이 부하 속에서 플랫폼 API만으로 이전합니다(카나리, 운영자 전환 승인, 쓰기 기록 검증). docker, kubectl, go가 있으면 한 명령으로 돌고, 끝나면 만든 것을 모두 지웁니다. 자세한 내용은 [test/e2e/README.md](test/e2e/README.md)에 있습니다.

```bash
test/e2e/run.sh          # KEEP=1이면 끝난 뒤에도 남기고, test/e2e/down.sh로 지움
```

### 배포 E2E

앱 차트 `deploy/charts/hopcharlie-app`과 AppProject, ApplicationSet `deploy/argocd`를 kind의 Argo CD v3.5.3에 올립니다. 먼저 gitops 파일을 손으로 커밋하고, 다음에는 플랫폼 API(가짜 GitHub, docker 빌드, git gitops)로 앱을 등록하고 push webhook을 보내고 지웁니다. 앱이 뜨고, 바뀌고, 지워지는지 봅니다. 헬스 체크가 실패하는 버전을 배포하는 동안 게이트웨이 요청이 하나도 실패하지 않는지도 봅니다. 자세한 내용은 [test/e2e/deploy/README.md](test/e2e/deploy/README.md)에 있습니다.

```bash
test/e2e/deploy/run.sh   # KEEP=1이면 끝난 뒤에도 남기고, test/e2e/deploy/down.sh로 지움
```

## 알려진 한계

- 게이트웨이는 한 대뿐이라 단일 장애점입니다. 상태 파일 기반이라 두 대로 늘리는 것은 가능하지만 아직 하지 않았습니다.
- 사람은 `PLATFORM_ADMINS`의 관리자 계정으로 로그인하고, 프로그램은 정적 토큰을 씁니다. 관리자를 바꾸려면 서버를 다시 띄워야 합니다. 관리자를 빼고 다시 띄우면 그 관리자의 콘솔 세션과 그가 승인한 CLI 토큰이 모두 401이 되지만, 비밀번호만 바꾸면 이미 나간 세션과 CLI 토큰은 만료될 때까지 유효합니다. 운영 환경이라면 OIDC 같은 IdP 연동이 필요합니다.
- CLI 토큰 하나만 끊는 API는 없습니다. 그 토큰으로 `POST /v1/auth/logout`을 부를 수 없으면, 관리 클러스터에서 플랫폼 DB(`charlie-platform` namespace의 `hopcharlie-postgres-0`)의 행을 지웁니다. 저장소에는 토큰이 없고 이름, 역할, `client`, 시각만 있으므로 `client`로 고릅니다.

  ```bash
  # CLI 토큰 목록
  kubectl -n charlie-platform exec hopcharlie-postgres-0 -c postgres -- psql -U charlie -d charlie \
    -c "SELECT name, role, client, created_at, expires_at FROM auth_sessions WHERE kind = 'cli' ORDER BY created_at DESC"
  # 한 호스트의 토큰을 지운다. Rin의 토큰 전부는 client LIKE '%on rin-sbx-%'
  kubectl -n charlie-platform exec hopcharlie-postgres-0 -c postgres -- psql -U charlie -d charlie \
    -c "DELETE FROM auth_sessions WHERE kind = 'cli' AND client = 'charlie v0.5.0 on rin-sbx-7-a1b2c3'"
  ```
- 클러스터 안에서 게이트웨이를 거치지 않고 DB에 쓰는 작업은 전환 중에 막을 수 없습니다. 이전하는 동안 멈춰야 합니다.
- 실제 클러스터에서 끝까지 이전해 본 것은 아직 없습니다. 클러스터 3개가 준비되면 엔진과 함께 리허설합니다.

## Plan 도구

Plan JSON을 손으로 쓰지 않고 이전을 준비하고, 검증에 쓸 쓰기 기록을 모으고, 끝난 이전을 되돌리는 API입니다.

| 요청 | 역할 | 설명 |
| --- | --- | --- |
| `GET /v1/apps` | viewer | `PLATFORM_APPS_FILE`의 앱과 앱이 있는 환경, 그리고 GitHub에서 등록한 앱 (아래 GitHub 앱 배포) |
| `POST /v1/plans/draft` | agent | `{"app","source","target"}`로 엔진 Plan 초안을 만듦. 저장하지 않으므로 그대로 또는 고쳐서 `POST /v1/plans`로 제출. `"sourceStats": true`를 더하면 원본 DB를 약 5초 측정해 `{"plan", "sourceStats"}`로 응답(세션 수, `max_connections`, `max_worker_processes`, 크기, WAL 속도. 숫자만, 측정 못 하면 `sourceStatsReason: not_measured`, ADR 0017) |
| `PUT /v1/plans/{ref}/attachments/{name}` | agent | Plan이 참조하는 파일(대상 CNPG 매니페스트) 업로드. 본문 그대로 1 MiB까지, `pending_approval`일 때만 |
| `GET /v1/plans/{ref}/attachments/{name}` | viewer | 첨부 내용. Plan 응답의 `attachments`에 이름, 크기, SHA-256이 있음 |
| `POST /v1/written-records/{stream}` | agent | 부하 생성기가 성공 응답을 받은 쓰기 `{"table","pk","at"}`. 한 번에 1개에서 5000개. `table`은 소문자로 저장하고(따옴표 없는 PostgreSQL 식별자), `table`과 `pk`에 제어 문자(NUL 포함)가 있으면 400 |
| `GET /v1/written-records/{stream}` | viewer | 스트림의 기록 수와 가장 늦은 기록 시각 |
| `POST /v1/migrations/{runId}/rollback` | operator | `succeeded` run을 온 길 그대로 되돌림. 202 + 새 runId |

### 앱 설정과 Plan 초안

- `PLATFORM_APPS_FILE`에 앱 설정 파일([로컬 개발 예시](examples/apps.yaml))을 지정합니다. 운영 파일은 infra의 `terraform/platform/aws/host/apps.yaml`입니다. 비워 두면 앱 목록이 비고 초안 요청은 400입니다.
- 앱의 환경 이름은 `PLATFORM_ENVS_FILE`에 있어야 합니다. 환경별 `passwordEnv`는 그 환경의 `dbHost:dbPort`로 가는 DSN의 비밀번호로만 쓸 수 있습니다. 이름은 `_PGPASSWORD`로 끝나야 하고 `PLATFORM_`, `AWS_`로 시작하면 안 됩니다. 그래야 `GATEWAY_TOKEN`이나 `PLATFORM_DATABASE_URL` 같은 다른 용도의 서버 변수를 비밀번호로 내보낼 수 없습니다.
- 초안의 id는 `<app>-<source>-to-<target>`이고, 컨텍스트는 환경의 `kubeContext`에서, 네임스페이스와 클러스터와 DB 호스트는 앱에서 가져옵니다.
- DSN은 `postgres://<user>:${<passwordEnv>}@<host>:<port>/<db>?sslmode=require`입니다. 비밀번호는 실행할 때 서버 환경에서 채웁니다. 값을 이스케이프하지 않고 URL에 그대로 넣으므로 비밀번호는 URL에 그대로 쓸 수 있는 문자(영문, 숫자, `-._~`)여야 합니다. CNPG가 만든 비밀번호는 그렇습니다. `@`, `/`, `:`, `#`, `?`, `%`가 들어가면 엔진이 다른 호스트로 접속하거나 접속 오류에 비밀번호 일부가 보일 수 있습니다.
- 두 환경 모두 `bucket`이 있으면 파일 이전을 넣습니다(`files: false`로 뺄 수 있음). 게이트웨이가 설정된 서버면 `gateway.mode: http`, 아니면 `none`입니다.

### 첨부와 쓰기 기록 참조

Plan은 서버의 파일을 경로 대신 이름으로 가리킵니다. 정책은 지금처럼 절대 경로와 `..`를 거부하고, 실제 경로는 서버가 정합니다.

- `target.manifest: "attachment:<name>"`: 첨부가 없으면 승인이 409로 실패합니다. Plan의 `revision`은 제출할 때 1이고 첨부를 올릴 때마다 1씩 늘어납니다. 첨부가 있는 Plan은 검토한 `revision`을 보내야 승인되고, 빠지면 409, 그 사이 첨부가 바뀌었으면 409 `changed since you reviewed it (revision X, now Y)`입니다. 확인과 승인은 저장소의 조건부 쓰기 하나라서 그 사이에 올린 첨부도 놓치지 않습니다. 승인 뒤에는 첨부를 바꿀 수 없으므로, 승인자가 본 내용 그대로 실행됩니다. 실행을 시작하거나 재개할 때 `<PLATFORM_STATE_DIR>/plans/<ref>/<name>`(디렉터리 0700, 파일 0600)에 쓰고 엔진에는 그 경로를 넘깁니다.
- `verify.writtenRecords: "stream:<name>"`: verify 단계 바로 전에 스트림의 기록 전부를 `<PLATFORM_STATE_DIR>/<runId>.written.jsonl`(0600)에 엔진 형식으로 씁니다. 전환하는 동안 들어온 기록까지 검증에 들어가고, 재개할 때도 같습니다. 파일을 쓰지 못하면 verify가 기록을 읽지 못했다고 보고하므로 통과로 넘어가지 않습니다. 스트림에 기록이 하나도 없으면 실행 시작과 재개가 400 `invalid_request`로 실패합니다. 증거 없이 검증이 통과하지 않도록, 부하 생성기를 이전보다 먼저 켭니다.
- 쓰기 기록 요청도 감사 로그에 하나씩 남습니다. 부하 생성기는 기록을 모아서 몇 초에 한 번 보냅니다.

### 1-click 롤백

- 원래 Plan의 원본과 대상을 맞바꾼 Plan(`<plan id>-rollback`)을 새로 저장하고 바로 실행합니다. env, 컨텍스트, 네임스페이스, 클러스터, DSN, 파일 위치, 게이트웨이 backend가 함께 바뀝니다. 옛 원본은 복제본으로 남아 있으므로 대상 매니페스트는 뺍니다.
- 새 Plan은 제출한 그대로의 JSON에서 만들므로 `${VAR}` 참조가 그대로 남고, 비밀번호는 저장되지 않습니다.
- 운영자의 롤백 요청이 사람 승인입니다. 새 Plan의 `reviewedBy`는 요청한 운영자이고 `reviewComment`는 `rollback of run <id>: <reason>`입니다. 감사 로그에는 fromRun, planRef, runId가 남습니다.
- 기본은 `autoSwitch: true`입니다. `false`면 `awaiting_switch`에서 `/switch`를 기다립니다.
- run 하나는 한 번만 롤백합니다. 새 Plan과 새 run에 `rollbackOf`가 붙고, run 하나를 되돌리는 Plan은 저장소에서 하나뿐입니다(거절된 Plan 제외).
- 앞선 롤백의 시작이 실패했으면(예: `cluster_busy`) 그 Plan은 `approved`로 남고, 다음 롤백 요청은 새 Plan을 만들지 않고 그 Plan을 다시 시작합니다. 그 Plan을 `POST /v1/migrations`로 직접 시작해도 run에 `rollbackOf`가 붙으므로, 그 뒤의 롤백 요청은 409 `run_state_conflict`로 그 run을 알려 줍니다.

CLI로는 `charlie apps`, `charlie plan new --app moimroom --from gke --to eks --stream moimroom-demo --submit`, `charlie plan attach REF --name target.yaml -f target.yaml`, `charlie written-records moimroom-demo`, `charlie migrate rollback RUNID --watch`를 씁니다.
## 보안 거버넌스 뷰와 에이전트 활동 기록

### `GET /v1/security` (viewer)

여러 환경의 보안 신호를 한 형식(`SecurityFinding`)으로 모아 보여 줍니다. 항목마다 환경, 공급자, 소스, 분류(identity, network, workload, data, logging, threat), 심각도(critical, high, medium, low, info), 대상, 설명, 권고가 있습니다. `id`는 환경, 소스, 검사, 대상으로 만든 해시라서 다시 읽어도 같습니다. 검사와 대상이 같은 항목이 여럿일 수 있는 곳은 공급자의 탐지 id(GuardDuty 탐지 id, Security Hub 탐지 ARN)나 보안 그룹 규칙의 프로토콜, 포트 범위, CIDR까지 해시에 넣어 `id`가 겹치지 않습니다.

| 소스 | 환경 | 읽는 것 |
| --- | --- | --- |
| `kubernetes` | 등록된 환경마다 | privileged, hostNetwork, hostPID, hostIPC, hostPath, root로 돌 수 있는 컨테이너(init, ephemeral 포함), limits 없음, NetworkPolicy 없는 네임스페이스, cluster-admin 바인딩, NodePort와 LoadBalancer 서비스. kube-system, kube-public, kube-node-lease만 뺍니다 |
| `aws-iam`, `aws-ec2`, `aws-cloudtrail`, `aws-guardduty`, `aws-securityhub`, `aws-s3` | `aws-account` | 루트 MFA와 키, MFA 없는 콘솔 사용자, 90일 넘은 액세스 키, 인터넷에 열린 보안 그룹, EBS 기본 암호화, 로깅 중인 trail, GuardDuty와 그 탐지 결과, Security Hub와 그 결과, S3 계정 퍼블릭 액세스 차단 |
| `platform-audit`, `platform-config` | `platform` | 지난 24시간 동안 401, 403으로 막힌 요청(누가 무엇을 몇 번), DB 호스트 제한과 게이트웨이 설정 |
| `gcp-scc`, `cloudflare` | `gcp-project`, `cloudflare-account` | 아직 연결하지 않았습니다. `not_configured`와 무엇이 없는지를 보여 줍니다 |

- 읽지 못한 소스는 `sources`에 `unavailable`(응답 없음), `not_configured`(설정이나 자격 증명 없음), `error`(권한 부족, 자격 증명을 가져오다 실패 등)와 이유로 표시하고, 항목을 지어내지 않습니다.
- 컨테이너는 컨테이너의 `runAsUser`, `runAsNonRoot`가 파드 값보다 우선합니다. `runAsNonRoot: true`이거나 `runAsUser`가 0이 아니면 root가 아닌 것으로 봅니다.
- `system:authenticated`, `system:unauthenticated`, `system:serviceaccounts`, `system:serviceaccounts:<ns>`에 cluster-admin을 묶으면 critical입니다.
- 401로 막힌 요청은 호출자가 경로를 마음대로 쓸 수 있으므로 경로를 보여 주지 않고 라우트(`GET /v1/plans/{ref}`)만 보여 줍니다. 403의 경로는 제어 문자를 지우고 80자에서 자르며, 3개까지만 적고 나머지는 개수만 적습니다.
- 결과는 60초 동안 캐시합니다. `?refresh=true`는 다시 읽지만 10초에 한 번까지입니다. `?environment=gke`, `?severity=high`(그 이상만)로 거릅니다.
- 수집기는 소스마다 하나이고 동시에 실행하며, 하나가 느려도 25초에서 끊습니다. 새 공급자는 `security.Collector` 구현 하나를 추가하면 됩니다.
- AWS는 SDK 기본 자격 증명 체인(환경 변수, `AWS_PROFILE`, 인스턴스 역할)을 쓰고 `PLATFORM_AWS_REGION`(기본 `ap-northeast-2`) 한 리전을 읽습니다. Describe, List, Get 호출만 하며 아무것도 켜거나 바꾸지 않습니다. 관리 호스트 역할에는 infra 레포의 Terraform이 수집기가 부르는 읽기 API 17개만 허용하는 인라인 정책(`read-security-configuration`)을 붙입니다.
- 자격 증명이 정말 없을 때(인스턴스에 역할이 없거나 메타데이터 서비스가 꺼짐)만 `not_configured`입니다. 메타데이터 서비스 시간 초과나 네트워크 오류로 가져오지 못하면 `error`이고, 메시지에는 SDK 원문 대신 실패 종류만 적습니다.
- GuardDuty 탐지는 심각한 것부터 50개까지 읽고, 더 있으면 메시지에 그렇게 적습니다.
- Kubernetes는 `GET /v1/envs`와 같은 kubeconfig와 컨텍스트를 씁니다. 서버 계정에 모든 네임스페이스의 pods, services, networkpolicies 목록과 clusterrolebindings 목록 권한이 필요합니다.
- 실제 계정 점검을 다시 해 보려면 `PLATFORM_TEST_AWS_LIVE=1 AWS_PROFILE=hopcharlie go test -run TestAWSLive -v ./internal/security`를 실행합니다.

### `GET /v1/security/ocsf` (viewer)

같은 탐지 결과를 업계 공통 형식인 [OCSF](https://schema.ocsf.io) 1.9.0 이벤트로 돌려줍니다. 수집, 캐시, 필터(`environment`, `severity`, `refresh`)는 `/v1/security`와 같고, 새로 읽는 것은 없습니다.

- 위협 탐지(`threat`, 예: GuardDuty)는 **Detection Finding**(class 2004), 나머지 설정 점검은 **Compliance Finding**(class 2003)입니다. Compliance Finding의 `compliance.status_id`는 Fail(3)이고, info 심각도는 Warning(2)입니다.
- 심각도는 info→1, low→2, medium→3, high→4, critical→5(`severity_id`)입니다. `finding_info.uid`는 `SecurityFinding.id`, `finding_info.analytic.name`은 검사 이름, `data_sources`는 소스, `cloud.provider`는 AWS, GCP 같은 공급자입니다. 모든 이벤트에 `metadata.version: 1.9.0`과 `cloud` 프로필을 적습니다.
- 테스트는 OCSF 공식 클래스 스키마(`internal/security/testdata/ocsf-1.9.0`, schema.ocsf.io에서 받은 원본)로 모든 종류의 이벤트를 검증합니다. 스키마에 없는 필드는 쓰지 않습니다.
- Security Lake 같은 특정 저장소에는 보내지 않습니다. OCSF를 읽는 도구(SIEM 등)가 이 JSON을 가져가면 됩니다.

### 에이전트 활동 기록 `/v1/agent/sessions`

AI 플래너가 작업 하나(계획, 리허설, 진단, 설명)마다 세션을 만들고, 도구 호출과 결과를 이벤트로 남깁니다. 콘솔과 CLI는 SSE로 실시간으로 봅니다.

| 요청 | 역할 | 설명 |
| --- | --- | --- |
| `POST /v1/agent/sessions` | agent | `{kind, title, planRef?, runId?, model?}`. 201, 상태 `running`, actor는 토큰 이름 |
| `POST /v1/agent/sessions/{id}/events` | agent | `{type, tool?, input?, output?, status?, durationMillis?, usage?}`. 201, `seq`가 붙은 이벤트 |
| `POST /v1/agent/sessions/{id}/finish` | agent | `{status: succeeded, failed, cancelled, summary?}` |
| `GET /v1/agent/sessions` | viewer | 최신순. `?planRef=`, `?runId=`, `?limit=`로 거름 |
| `GET /v1/agent/sessions/{id}` | viewer | 이벤트 수, 도구 호출 수, 토큰, 소요 시간 합계 포함 |
| `GET /v1/agent/sessions/{id}/events` | viewer | SSE. run 이벤트와 같은 규칙(처음과 끝에 `snapshot`, `Last-Event-ID`로 재접속). `?format=json`은 목록 한 쪽: `?after=`, `?limit=`(기본 500, 최대 2000), 더 있으면 `nextAfter` |

- 이벤트와 종료는 세션을 만든 토큰이나 operator만 쓸 수 있습니다. 끝난 세션에 쓰면 409 `agent_session_finished`입니다.
- 세션 하나에는 이벤트를 5000개까지 남깁니다. 넘으면 409 `session_full`입니다.
- `input`, `output`은 아무 JSON 값이고 각각 64 KiB까지입니다. 저장하기 전에 비밀값을 `"[redacted]"`로 바꿉니다.
  - 키 이름이 password, passwd, pwd, pass, secret, token, authorization, cookie, api key, private key, credential, access key 꼴이면 그 아래 문자열 값을 바꿉니다. 숫자와 불리언은 그대로 두므로 `input_tokens` 같은 토큰 수는 남습니다.
  - `{"name": "DB_PASSWORD", "value": "..."}`처럼 `name`이나 `key`가 비밀값 이름인 객체는 `value`를 바꿉니다.
  - 모든 문자열 안에서 `NAME=값`, `NAME: 값`(libpq `password=...` 포함), URL의 비밀번호(`postgres://u:[redacted]@host`), `Bearer` 토큰, AWS 액세스 키 id, PEM 개인 키를 바꿉니다. `${SRC_PGPASSWORD}` 같은 변수 참조는 값이 아니므로 남깁니다.
- 저장하는 문자열에서는 NUL을 지우고 잘못된 UTF-8을 U+FFFD로 바꿉니다. PostgreSQL이 받지 않는 값이어도 이벤트를 잃지 않습니다.
- 이벤트 없이 `PLATFORM_AGENT_IDLE`(기본 30분)이 지난 `running` 세션은 플랫폼이 `cancelled`로 닫고 요약에 `no activity for 30m; closed by the platform`을 남깁니다. 보고 있던 스트림은 마지막 `snapshot`을 보내고 닫힙니다.
- 쓰기 요청은 모두 감사 로그에 남습니다. 감사 로그에는 세션 id, 이벤트 종류, 도구 이름만 남고 내용은 남기지 않습니다.
- 테이블은 마이그레이션 `0003_agent_activity.sql`이 만듭니다.

## GitHub 앱 배포

앱 저장소(서비스마다 Dockerfile, 루트에 `platform.yaml`)를 등록하면, 플랫폼이 서비스마다 이미지를 빌드하고 gitops 저장소에 `apps/<app>/<env>.yaml`을 커밋합니다. Argo CD가 Application `<app>-<env>`를 네임스페이스 `<app>`에 배포합니다. 설계와 `platform.yaml` 규칙은 [ADR 0011](docs/adr/0011-github-app-deploy.md)에 있습니다.

앱은 등록한 브랜치를 기본 인스턴스(이름은 앱 이름)로 띄우고, 운영자가 더한 브랜치를 인스턴스 `<app>--<slug>`로 따로 띄웁니다. 인스턴스 이름이 네임스페이스, gitops 경로 `apps/<instance>/`, Application `<instance>-<env>`, 주소 `https://<instance>.<도메인>`이 됩니다. 앱 하나에 인스턴스는 기본 인스턴스를 포함해 3개까지입니다. 그래서 앱 이름은 `^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`에 맞고 `--`를 포함하지 않습니다. 설계는 [ADR 0015](docs/adr/0015-branch-instances.md)에 있습니다.

| 요청 | 역할 | 설명 |
| --- | --- | --- |
| `POST /v1/apps` | operator | `{repo, branch?, environments, autoDeploy?}`. 브랜치 head의 `platform.yaml`을 읽어 앱을 등록하고 첫 배포를 큐에 넣음. 201 + `{app, deployment}`. 잘못된 `platform.yaml`은 422 `invalid_app_spec` (`validation`에 위치별 오류) |
| `POST /v1/apps/validate` | viewer | `{repo, branch?}` 또는 `{spec}`의 `platform.yaml`을 검사만 함. 항상 200 + `valid` |
| `GET /v1/apps/{name}` | viewer | 앱 하나. 등록한 앱은 `deploy`에 저장소, spec, 환경별 상태, 마지막 배포, 인스턴스 목록(`instances`, 기본 인스턴스가 맨 앞). `deploy`의 상태와 마지막 배포는 기본 인스턴스의 것 |
| `DELETE /v1/apps/{name}` | operator | 등록 해제. 추가한 인스턴스도 함께 지움. gitops 파일을 지우면 Argo CD가 워크로드를 지움. 202. 파일에만 있는 앱은 409 |
| `POST /v1/apps/{name}/instances` | operator | `{branch, autoDeploy?}`. 브랜치를 인스턴스 `<app>--<slug>`로 더함. 브랜치 head의 `platform.yaml`을 등록과 같은 규칙으로 검사하고 첫 배포를 큐에 넣음. 201 + `{instance, deployment}`. 이미 있는 브랜치는 409 `branch_exists`, 인스턴스 이름이 겹치면 409 `instance_exists`, 3개를 넘으면 409 `instance_limit` |
| `DELETE /v1/apps/{name}/instances/{instance}` | operator | 추가한 인스턴스를 지움. gitops 파일, 게이트웨이 등록, 브랜치와 배포 기록을 지움. 202. 기본 인스턴스는 409 `default_instance` |
| `POST /v1/apps/{name}/deployments` | operator | `{branch?, ref?, environments?, redeploy?}`로 지금 배포. `branch`가 인스턴스를 고름(기본은 앱의 브랜치). `redeploy: true`이면 그 인스턴스의 마지막 healthy 배포를 같은 커밋, 이미지, `platform.yaml`로 빌드 없이 지금의 시크릿으로 그 배포가 간 환경에만 다시 배포(trigger `redeploy`). 202 + Deployment |
| `GET /v1/apps/{name}/deployments` | viewer | 최신순, `?limit=` (기본 20), `?branch=`로 그 브랜치의 배포만 |
| `GET /v1/deployments/{id}` | viewer | 배포 하나 (상태, 이미지 digest, 환경별 상태) |
| `GET /v1/deployments/{id}/events` | viewer | SSE. run 이벤트와 같은 규칙(처음과 끝에 `snapshot`, `Last-Event-ID`로 재접속) |
| `GET /v1/apps/{name}/secrets` | viewer | 앱 시크릿의 이름, 바꾼 시각, 바꾼 사람. 앱 단위(`app`)와 인스턴스별 덮어쓰기(`instances`). 값은 없음 |
| `PUT /v1/apps/{name}/secrets` | operator | `{instance?, set?, delete?}`로 한 범위(앱 단위, 또는 `instance`의 덮어쓰기)를 한 번에 바꿈. 200 + 바꾼 뒤의 이름 목록 |
| `GET /v1/apps/{name}/secrets/{secret}` | operator | 값 하나. `?instance=`이면 그 인스턴스의 덮어쓰기. 부를 때마다 감사 로그에 남고 `Cache-Control: no-store` |
| `GET /v1/github/repos` | viewer | GitHub App 설치가 볼 수 있는 저장소 |
| `POST /v1/github/push-tokens` | agent | `{repo}`. `scripts/import-repo.sh`로 들여온 저장소(조직 안, 토픽 `hopcharlie-imported`) 하나의 id에만 묶인 `contents: write` 1시간짜리 GitHub App 설치 토큰. 201 + `{repo, token, expiresAt}`, `Cache-Control: no-store`. 감사 로그에는 저장소와 만료 시각만 남고, 감사 로그를 쓰지 못하면 토큰을 폐기하고 503 `audit_unavailable`. 토픽이 없거나 gitops 저장소이면 403 `repo_not_imported` ([ADR 0023](docs/adr/0023-push-token-for-imported-repos.md)) |
| `POST /v1/webhooks/github` | 없음 | GitHub App webhook. `X-Hub-Signature-256`으로 확인. 앱의 브랜치나 추가한 브랜치 가운데 `autoDeploy`가 켜진 브랜치의 push만 그 인스턴스에 배포(202), 나머지는 204 |

- 배포 상태는 `queued`, `building`, `committing`, `syncing`을 거쳐 `healthy`, `failed`, `superseded`로 끝납니다. 같은 환경으로 새 배포가 생기면 끝나지 않은 이전 배포는 `superseded`가 됩니다.
- 테이블은 마이그레이션 `0005_app_deploy.sql`과 `0006_app_branches.sql`이 만듭니다. 배포가 읽은 `platform.yaml`은 `0008_app_secrets.sql`이 더한 `deployments.spec`에 남아, 재배포가 GitHub을 읽지 않고 그대로 씁니다.
- 앱 이름 `api`, `console`, `app`, `headlamp`는 플랫폼과 infra가 쓰므로 등록할 수 없습니다(409 `app_exists`).

### 앱 시크릿

GitHub에서 등록한 앱의 비밀 환경 변수를 운영자가 넣고, 고치고, 다시 봅니다. 설계는 [ADR 0019](docs/adr/0019-app-secrets-and-redeploy.md)에 있습니다.

- 시크릿은 앱 단위로 두고, 인스턴스마다 덮어쓸 수 있습니다. 인스턴스가 받는 값은 앱 단위 값 위에 그 인스턴스의 덮어쓰기를 얹은 것입니다. 덮어쓰기를 지우면 앱 단위 값으로 돌아갑니다. 기본 인스턴스의 덮어쓰기는 `instance`에 앱 이름을 씁니다.
- 이름은 `^[A-Z_][A-Z0-9_]*$`, 64자까지이고 `PORT`, `DATABASE_URL`, `S3_*`는 쓸 수 없습니다. 값은 1바이트에서 4096바이트까지이고(빈 값은 400), 앱 단위와 인스턴스마다 50개까지입니다. 한 요청에서 같은 이름을 넣고 지우면 400, 바꿀 것이 없어도 400입니다. 50개는 바꾼 뒤의 개수로 셉니다.
- 값은 `PLATFORM_SECRETS_KEY`로 AES-256-GCM 암호화해 플랫폼 DB에 저장합니다. nonce는 값마다 무작위이고, AAD는 앱, 인스턴스, 이름을 각각 길이와 함께 이어 붙인 값이라서 다른 행으로 옮긴 암호문은 복호화되지 않습니다. 행마다 키 버전(지금은 1)과 키 확인값(키에서 HKDF로 만든 16진수 12자)을 저장합니다. 다른 키로 띄운 서버는 시작할 때 그런 행의 개수를 오류 로그로 남기고, 그 값의 조회는 500이며 서버 로그에 두 키 확인값을 남깁니다. 키 자체는 어디에도 남기지 않습니다. 테이블은 `0008_app_secrets.sql`이 만듭니다.
- 앱을 지우면 시크릿이 모두, 인스턴스를 지우면 그 인스턴스의 덮어쓰기가 같은 트랜잭션에서 지워집니다. 같은 이름으로 다시 등록해도 이전 시크릿은 없습니다.
- 감사 로그에는 검증을 통과한 앱, 인스턴스, 넣은 이름(`set`), 지운 이름(`deleted`)만 남고 값은 남지 않습니다. 400으로 거절한 요청은 이름 대신 개수(`setCount`, `deleteCount`)만 남깁니다. 이름 자리에 값을 잘못 붙여 넣었을 수 있기 때문입니다. 같은 이유로 규칙에 맞지 않는 앱 이름이나 시크릿 이름은 감사 로그의 경로에 `{invalid}`로 남습니다.
- 값 조회도 GET이지만 매번 감사 로그에 남습니다. 값을 보내기 전에 감사 로그를 먼저 쓰고, 쓰지 못하면 값을 주지 않고 503 `audit_unavailable`로 답합니다. 서버 로그에도 값과 암호문을 쓰지 않습니다.
- viewer와 agent 토큰(예: Rin의 CLI 토큰)은 이름만 볼 수 있습니다.
- 배포할 때마다 실행기가 그 인스턴스가 받는 시크릿을 환경 클러스터의 Sealed Secrets 인증서로 봉인해 gitops 파일의 `values.sealedSecrets.app`에 넣고(SealedSecret `app-secrets`, 범위 strict), `platform.yaml`의 서비스 env에서 같은 이름을 뺍니다. 시크릿이 이깁니다. 차트가 모든 서비스 컨테이너에 `envFrom`으로 넣습니다.
- 봉인은 할 때마다 결과가 달라서, 앱, 인스턴스, 시크릿 이름과 값, 버킷 키 id로 만든 HMAC-SHA256(`appChecksum`)을 함께 씁니다. 차트가 파드 annotation에 쓰므로, 시크릿이나 버킷 키가 바뀔 때만 파드가 다시 뜹니다.
- 시크릿을 바꾼 뒤에는 `POST /v1/apps/{name}/deployments`에 `redeploy: true`를 보냅니다. 마지막 healthy 배포를 같은 커밋, 이미지, `platform.yaml`로 다시 배포하므로 빌드와 GitHub 읽기가 없습니다. healthy 배포가 없으면 409 `nothing_to_redeploy`, 그 인스턴스에 끝나지 않은 배포(queued, building, committing, syncing)가 있으면 409 `deployment_in_progress`입니다. 진행 중인 배포는 gitops 커밋 때의 시크릿을 쓰므로 끝난 뒤에 다시 보냅니다. `ref`와 함께 쓰면 400입니다.
- 재배포는 그 healthy 배포가 간 환경에만 갑니다. `environments`를 빼면 그 배포의 환경 중 앱이 아직 배포하는 환경 모두이고, `environments`에 그 배포가 가지 않은 환경이 있으면 409 `nothing_to_redeploy`입니다. 그 환경은 다른 버전을 돌리고 있을 수 있으므로 브랜치를 배포합니다.
- 확인과 저장 사이에 다른 배포가 들어와도 실행기가 둘을 함께 돌리지 않습니다. 재배포가 아닌 배포는 끝나지 않은 재배포를 모두 superseded로 끝내고, 재배포는 큐에 들어갈 때 재배포가 아닌 끝나지 않은 배포가 있으면 스스로 superseded로 끝납니다(그 배포가 지금의 시크릿을 씁니다).
- 시크릿이 있는 앱인데 서버에 `PLATFORM_SECRETS_KEY`가 없거나, 그 환경에 쓸 수 있는 인증서가 없으면 배포가 `failed`로 끝나고 이유가 `error`에 남습니다.

### 버킷과 키 드라이버

`platform.yaml`에 s3 리소스가 있는 앱은 인스턴스마다 버킷 하나와 그 버킷에만 쓸 수 있는 키를 받습니다([ADR 0016](docs/adr/0016-app-object-storage.md)). 키를 만드는 방법은 `PLATFORM_ENVS_FILE`의 환경별 `storage.driver`가 정하고, 만든 키는 그 환경 클러스터의 Sealed Secrets 인증서(`sealedSecretsCert`)로 봉인합니다. 필드는 [examples/envs.yaml](examples/envs.yaml)에 있습니다.

- 버킷 이름은 `<bucketPrefix><인스턴스>`이고, `bucketPrefix`는 15자까지, 인스턴스 이름은 48자까지라서 버킷 이름은 S3 상한인 63자를 넘지 않습니다. IAM 사용자 이름이 64자를 넘으면 API를 부르기 전에 거절합니다.
- 버킷과 사용자에는 태그 `hopcharlie:app`, `hopcharlie:instance`를 붙입니다. 같은 이름의 버킷이나 사용자가 다른 앱의 태그를 달고 있으면 드라이버는 그것을 쓰지도 지우지도 않습니다. IAM 사용자는 만드는 호출에서 태그가 같이 붙으므로, 태그가 없는 사용자는 플랫폼이 만든 것이 아니라고 보고 거절합니다. 태그가 없는 버킷은 같은 계정이 방금 만든 경우(만든 직후 태그 전에 끊긴 경우)에만 이어서 씁니다. 지울 때는 태그가 맞는 버킷과 사용자만 지웁니다.
- 앱 이름으로 소유를 가리므로, 지운 앱이나 인스턴스의 버킷과 키를 아직 지우는 중이면(`instance_storage`에 행이 남아 있으면) 같은 이름의 앱 등록과 인스턴스 추가는 409 `storage_cleanup_pending`입니다.
- `aws`: 그 리전에 S3 버킷을 만들고 공개 접근 차단 네 가지를 모두 켭니다. IAM 사용자 `hopcharlie-app-<인스턴스>`를 `iamPath` 아래에 `permissionsBoundary`를 붙여 만들고, 인라인 정책 `hopcharlie-bucket`으로 그 버킷의 목록, 객체 읽기, 쓰기, 지우기만 허용한 뒤 액세스 키를 만듭니다. 플랫폼의 AWS 자격 증명(pod 역할)을 씁니다.
- `minio`: MinIO 관리 API(admin API v3)로 버킷, 사용자 `hopcharlie-app-<인스턴스>`, 같은 이름의 canned policy(그 버킷만 허용)를 만들고, 키는 그 사용자의 서비스 계정으로 만듭니다. 키를 지울 때는 그 서비스 계정의 상위 사용자가 그 인스턴스의 사용자인지 먼저 확인합니다. 관리 키는 `adminAccessKeyEnv`, `adminSecretKeyEnv`가 가리키는 플랫폼 환경 변수에서 읽고, 비어 있으면 서버가 시작하지 않습니다.
- `minio` 드라이버는 AGPL-3.0인 MinIO 관리 클라이언트(madmin-go)를 링크하므로 `-tags minio`로 빌드한 API에만 들어갑니다(kind E2E, 온프렘). 기본 빌드와 게시하는 이미지에는 없고, `minio` 저장소 설정이 있으면 "built with -tags minio" 오류로 서버가 시작하지 않습니다. CI는 태그를 붙인 빌드도 vet과 테스트합니다.
- 사용자 이름은 인스턴스 이름만으로 정하므로 `aws` 드라이버는 한 환경만 쓸 수 있고, `minio` 드라이버는 환경마다 엔드포인트가 달라야 합니다. 어기면 서버가 시작하지 않습니다.
- 모든 작업은 다시 불러도 됩니다. 이미 있는 버킷과 사용자는 그대로 쓰고, 새 키는 사용자를 새로 만들었거나 요청했을 때만 만듭니다. 새 키를 만들기 전에 지금 키와 옛 키(`old_key_id`)를 빼고 나머지 키를 지워서, 중간에 끊긴 뒤 다시 해도 아무도 모르는 키가 쌓이지 않습니다. 사용자당 키는 둘까지라서, 지금 키와 옛 키가 모두 있으면 새 키를 만들지 않고 실패합니다. 삭제는 사용자(키와 정책 포함)를 먼저 지우고, 버킷을 비운 뒤 지웁니다. 이미 없는 것은 건너뜁니다.
- `gcs`는 GCS 버킷·서비스 계정·HMAC 키를 만듭니다. `storage`에 `driver: gcs`, `bucketPrefix`, 단일 리전 `region`, GCP 프로젝트 ID `project`를 지정합니다([설정 예시](examples/envs.yaml)). 인증은 ADC를 사용하며, 운영에서는 공개 WIF 설정 파일과 projected pod token을 연결합니다. `GOOGLE_APPLICATION_CREDENTIALS`는 이 API의 GCS 인증용으로, CodeBuild의 `PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG`와 별개입니다. 같은 프로젝트는 한 환경에서만 사용할 수 있습니다.
- GCS 앱 계정은 자기 버킷의 `objectAdmin`·`legacyBucketReader` 권한만 받습니다. HMAC 키 생성을 조직 정책이 막으면 정책을 바꾸지 않고 배포를 실패시킵니다. 키 사용·비활성화 전파에는 지연이 있을 수 있고, 삭제 후 soft delete·보존 정책에 따른 보관·비용이 남을 수 있습니다. 권한·지연·삭제 계약은 [ADR 0016](docs/adr/0016-app-object-storage.md)에 있습니다. 실제 GKE 설정은 드라이버 이미지·관리 권한·인증서가 준비된 뒤에 활성화합니다.
- 인증서 파일이 없거나 읽히지 않으면 시작할 때 경고만 남깁니다. 봉인할 때마다 파일의 수정 시각과 크기를 보고, 바뀌었으면 다시 읽어 내용이 바뀐 경우에만 새 인증서를 씁니다. 인증서를 나중에 넣거나 바꿔도 서버를 다시 띄울 필요가 없습니다. 만료된 인증서와 2048비트보다 짧은 RSA 키는 거절합니다.
- 배포 실행기가 커밋하기 전에, 인스턴스 잠금 안에서 환경마다 이렇게 합니다. 저장한 키가 없으면 드라이버로 버킷과 새 키를 만들고, 키를 그 환경의 인증서로 봉인(네임스페이스는 인스턴스, Secret `app-s3`)해 저장합니다. 저장한 키가 있고 인증서 지문이 같으면, 드라이버가 그 키가 아직 있는지 확인한 뒤 저장한 암호문을 그대로 씁니다. 키가 사라졌으면 새 키를 만듭니다. 지문이 다르면(평문 키는 남기지 않으므로) 새 키를 만들어 다시 봉인하고, 옛 키를 행의 `old_key_id`에 적습니다. 시크릿이 있는 앱은 키 id가 `appChecksum`에 들어가 파드가 다시 뜨므로, 그 뒤 healthy가 된 배포가 옛 키를 지우고 `old_key_id`를 비웁니다. 시크릿이 없는 앱은 파드가 다시 뜨지 않아 옛 키를 계속 쓸 수 있으므로 옛 키를 남깁니다. 시크릿을 넣고 다시 배포하면 지워지고, 그 전에 인증서가 또 바뀌면 배포가 실패합니다(운영자 처리는 [ADR 0016](docs/adr/0016-app-object-storage.md)의 알려진 한계). 저장한 행을 만든 드라이버가 그 환경의 지금 드라이버와 다르면 새 키를 만들지 않고 배포가 실패합니다. 배포 이벤트에는 키 id의 마지막 4자만 나옵니다. 봉인한 키는 `values.sealedSecrets.s3`에 들어갑니다.
- 플랫폼 DB에는 인스턴스와 환경마다 봉인한 키, 키 식별자, 인증서 지문만 저장합니다(`instance_storage`). 평문 키는 Git, DB, 로그에 남기지 않습니다.
- 인스턴스나 앱을 지우면 gitops 삭제 커밋 뒤에 실행기가 드라이버로 사용자(키와 정책 포함)를 지우고 버킷을 비운 뒤 지우고, 그다음 행을 지웁니다. 실패하면 행을 남기고 로그를 쓴 뒤, 서버가 시작할 때와 5분마다 다시 합니다. 행이 남아 있는 동안 같은 이름의 등록은 409 `storage_cleanup_pending`입니다.
- 환경을 설정에서 뺐거나 환경의 드라이버를 바꾸면 그 행은 플랫폼이 지울 수 없어서, 시도할 때마다 error 로그 `cannot delete the bucket and key of a deleted instance`(instance, env, driver, bucket 포함)를 남깁니다. 운영자는 옛 드라이버의 도구로 버킷, 사용자, 키를 지운 뒤 `DELETE FROM instance_storage WHERE instance = '<인스턴스>' AND env = '<환경>';`로 행을 지웁니다. 명령은 [ADR 0016](docs/adr/0016-app-object-storage.md)의 구현 절에 있습니다.
- `platform.yaml`에 s3 리소스가 있는데 대상 환경에 저장소 드라이버가 없으면 등록과 인스턴스 추가가 503 `storage_not_configured`입니다. 나중의 push로 s3 리소스가 생겼는데 드라이버나 인증서가 없으면 그 배포가 `failed`로 끝납니다.
- 앱 차트의 버킷 이름은 프로필의 `profile.s3.bucketPrefix`로 만들므로, 각 환경의 프로필과 `storage.bucketPrefix`를 같게 둡니다. 프로필의 `credentialsSecret`은 `app-s3`여야 합니다.

### GitHub App 설정

플랫폼은 GitHub App 하나로 앱 저장소를 읽고, gitops 저장소에 커밋하고, push webhook을 받습니다. 조직 설정에서 App을 만들어 조직에 설치합니다.

- 권한은 Repository permissions의 Contents를 Read and write로, Metadata를 Read-only로 둡니다.
- 이벤트는 Push 하나만 구독합니다.
- Webhook URL은 `https://api.hopcharlie.dev/v1/webhooks/github`, content type은 `application/json`입니다. secret에는 `PLATFORM_GITHUB_WEBHOOK_SECRET`과 같은 값을 넣습니다.
- App 설정 페이지에서 private key를 만들어 서버에만 넣습니다. 키와 webhook secret은 어느 저장소에도 커밋하지 않습니다.

| 변수 | 설명 |
| --- | --- |
| `PLATFORM_GITHUB_APP_ID` | App ID(숫자). App ID와 키를 모두 비우면 GitHub이 필요한 요청은 503 `github_not_configured`. 하나만 넣으면 서버가 시작하지 않음 |
| `PLATFORM_GITHUB_APP_PRIVATE_KEY` | private key PEM 내용. `PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE`(PEM 파일 경로)과 둘 중 하나만 |
| `PLATFORM_GITHUB_INSTALLATION_ID` | 설치 ID. 비우면 `PLATFORM_GITHUB_ORG`에 설치된 것을 찾아 씀 |
| `PLATFORM_GITHUB_ORG` | App을 설치한 조직 (기본 `2026-Softbank-Hackathon-Charlie`) |
| `PLATFORM_GITHUB_WEBHOOK_SECRET` | webhook secret. 비우면 webhook이 503으로 답하고 push가 배포되지 않음 |
| `PLATFORM_GITHUB_API_URL` | GitHub API 주소 (기본 `https://api.github.com`) |
| `PLATFORM_APP_DOMAIN` | 공개 서비스 주소 `https://<app>.<도메인>`의 도메인 (기본 `hopcharlie.dev`) |

- 서버는 App 키로 JWT를 서명해 설치 토큰을 받고, 만료 5분 전까지 같은 토큰을 씁니다.
- webhook은 bearer 토큰 대신 `X-Hub-Signature-256`을 확인합니다. 서명이 없거나 틀리면 401 `invalid_signature`이고 감사 로그에 `anonymous`로 남습니다. 배포를 만든 push는 `github`으로 남고, payload는 감사 로그에 넣지 않습니다.
- 같은 `X-GitHub-Delivery`로 다시 온 push는 204로 답하고 배포를 만들지 않습니다.

### 배포 실행기

`PLATFORM_DEPLOY_BUILDER`를 넣으면 서버가 배포 실행기(`internal/deploy`)를 띄웁니다. 비우면 실행기가 없고 배포는 `queued`에 머뭅니다.

- 작업자 `PLATFORM_DEPLOY_WORKERS`개가 큐에서 배포를 하나씩 꺼냅니다. 서버가 시작할 때 끝나지 않은 배포를 오래된 것부터 이어 갑니다. 이미지를 이미 만든 배포는 다시 빌드하지 않습니다.
- 배포는 앱의 인스턴스 하나로 갑니다(ADR 0015). 기본 인스턴스의 이름은 앱 이름이고, 추가한 브랜치의 인스턴스는 `<app>--<slug>`입니다. 인스턴스 이름이 gitops 경로, 네임스페이스, Application, 게이트웨이 앱 이름, 주소가 되고, 앱 이름은 차트의 `app`과 이미지 저장소에 그대로 씁니다.
- 새 배포를 큐에 넣을 때, 같은 인스턴스의 끝나지 않은 이전 배포 가운데 환경이 모두 새 배포에 들어가는 것을 `superseded`로 끝냅니다. 다른 인스턴스의 배포는 건드리지 않으므로, `dev`에 push 해도 `main`의 배포가 밀려나지 않습니다. 그 배포의 작업자는 다음 저장에서 멈춥니다. gitops 커밋과 게이트웨이 등록은 인스턴스마다 잠금을 잡고, 배포가 아직 끝나지 않았음을 저장소에 쓴 직후에만 합니다. 그래서 밀려난 배포가 새 배포보다 늦게 커밋하지 않습니다.
- building: 커밋의 `platform.yaml`을 등록과 같은 규칙(`deploy.CheckSpec`)으로 검사합니다. 틀리면 위치별 오류를 `log` 이벤트로 남기고 `failed`로 끝납니다. 통과했고 인스턴스의 가장 새 배포이면 그 인스턴스의 spec을 그 커밋의 것으로 바꿉니다. 기본 인스턴스는 앱의 spec을, 추가한 브랜치는 그 브랜치의 spec을 바꿉니다. 그 브랜치가 이미 지워졌으면 배포가 `failed`로 끝납니다. 그다음 서비스마다 `<registry>/hopcharlie/apps/<app>/<service>:<commit>`을 빌드해 push하고, digest(`...@sha256:...`)로 고정합니다. 이미지 저장소는 인스턴스가 함께 씁니다.
- committing: 인스턴스 잠금 안에서 버킷 키와 시크릿을 봉인하고([앱 시크릿](#앱-시크릿), [버킷과 키 드라이버](#버킷과-키-드라이버)), 대상 환경마다 `apps/<instance>/<env>.yaml`을 ADR 0011 형식으로 만들어 커밋 하나로 씁니다. 봉인할 것이 없으면 `values.sealedSecrets`가 없습니다. `namespace`는 인스턴스 이름이고, `app`과 `values.app`은 앱 이름입니다. 키 순서가 정해져 있고 문자열은 모두 큰따옴표로 감싸므로, 같은 배포를 다시 쓰면 커밋이 생기지 않습니다. 다른 커밋이 먼저 들어가 push가 거부되면 새 head에서 다시 씁니다. 여러 인스턴스가 한꺼번에 커밋해도 이렇게 다시 씁니다.
- syncing: ApplicationSet과 Application `<namespace>-<env>`에 refresh를 요청하고, 모든 환경의 Application이 그 gitops 커밋이나 그 뒤의 커밋을 `Synced`, `Healthy`로 보고할 때까지 기다립니다. multi-source Application은 `status.sync.revisions`에서 gitops 커밋을 찾습니다. Application이 아직 없으면 계속 기다립니다. `Degraded`는 환경 상태 `degraded`, 동기화 실패는 `failed`이고, `PLATFORM_ARGOCD_TIMEOUT` 안에 모두 healthy가 되지 않으면 배포가 `failed`로 끝납니다. 그 커밋에서 `Synced`, `Degraded`가 1분 이어진 환경이 있으면(파드가 계속 죽거나, progress deadline을 넘었거나, 컨트롤러가 SealedSecret을 열지 못한 경우) 제한 시간을 기다리지 않고 바로 `failed`로 끝납니다.
- 모든 환경이 healthy이고, 공개 서비스가 있고, 게이트웨이(`PLATFORM_GATEWAY_URL`)가 설정돼 있으면 환경마다 공개 서비스 주소를 찾아 게이트웨이에 인스턴스를 등록합니다(`PUT /apps/<instance>`). 처음 등록하는 앱은 첫 환경으로 쓰기를 보냅니다. 이미 있는 앱은 이전이 옮긴 쓰기 환경과 읽기 비율을 그대로 둡니다. 게이트웨이에 이미 있는 백엔드는 앱의 환경 목록에 없어도 주소와 함께 그대로 둡니다. 이전은 요청을 옮기기 전에 대상 백엔드를 먼저 넣어 두기 때문입니다. 그대로 둘 백엔드의 주소에 비밀번호가 있으면 게이트웨이가 그 비밀번호를 가려서 알려 주므로, 다시 보내지 않고 배포를 `failed`로 끝냅니다. 이전 중이라 쓰기가 보류돼 있으면 게이트웨이를 바꾸지 않고 배포를 `failed`로 끝냅니다.
- 백엔드 주소는 `PLATFORM_APP_BACKEND_TEMPLATE`가 있으면 그 틀로 만들고, 없으면 환경 클러스터에서 Service `<app>-<service>`(네임스페이스 `<instance>`)의 LoadBalancer 주소를 읽습니다. 주소를 찾는 것도 커밋 때 시작한 `PLATFORM_ARGOCD_TIMEOUT`의 남은 시간 안에서만 다시 시도합니다. 게이트웨이의 백엔드 이름은 환경 이름입니다.
- 주소를 찾은 뒤 게이트웨이를 바꾸기 전에, 그 주소에서 공개 서비스가 실제로 답할 때까지 기다립니다. Argo CD는 LoadBalancer Service에 주소가 생기면 바로 `Healthy`로 보지만, AWS가 NLB를 다 만들기까지 몇 분이 더 걸리고 그동안 게이트웨이는 502를 답하기 때문입니다. `healthCheck`가 있으면 `<주소><healthCheck>`에 GET을 보내 2xx나 3xx를 받아야 하고(리다이렉트는 따라가지 않음), 없으면 `/`에 GET을 보내 502, 503, 504가 아닌 응답을 받거나, 3초 안에 응답이 없을 때 TCP 연결이 열리면 됩니다. 그래서 `healthCheck`가 없는 앱의 `/`가 게이트웨이가 요청을 보내는 환경에서 계속 502, 503, 504를 답하면 배포가 실패합니다.
- 한 번 확인하는 데 3초(TCP 연결까지 확인하면 6초)까지 걸리고, 확인이 끝나면 5초(`PollInterval`)를 쉬고 다시 부릅니다. 환경 여러 개는 동시에 확인하고, 게이트웨이에 같은 주소로 이미 있는 환경(같은 인스턴스를 다시 배포할 때)은 확인하지 않습니다. 환경마다 처음 보는 이유와 기다린 끝에 답했을 때만 `log` 이벤트를 남깁니다. HTTP 요청은 게이트웨이처럼 `HTTP_PROXY`를 따르지만, TCP 연결 확인은 프록시를 거치지 않습니다.
- 이것도 `PLATFORM_ARGOCD_TIMEOUT`의 남은 시간 안에서만 기다립니다. 남은 시간이 없어도 한 번은 확인하고, 마지막 확인은 마감 뒤에 한 번 더 걸릴 수 있습니다. 그 안에 게이트웨이가 요청을 보내는 환경(쓰기 백엔드와 읽기 가중치가 있는 백엔드)이 답하지 않으면 게이트웨이를 바꾸지 않고 배포를 `failed`로 끝냅니다. 그 밖의 환경은 요청을 받는 환경이 모두 답한 뒤 `PollInterval` 두 번까지만 더 기다립니다. 그때까지 답하지 않은 환경은 게이트웨이 갱신에서 빼고(이미 있던 경로는 그대로 둠) 환경 `message`에 이유를 남기며, 배포는 `healthy`로 끝납니다. 빠진 환경은 다음 배포에서 다시 확인합니다. 앱 상태(`GET /v1/apps/{name}`)의 환경에는 `message`가 없어서, 빠진 이유는 배포 상세에서 봐야 합니다.
- `DELETE /v1/apps/{name}`은 앱의 끝나지 않은 배포를 모두 `superseded`로 끝내고, gitops 저장소에서 기본 인스턴스와 추가한 브랜치 인스턴스의 `apps/<instance>/`를 커밋 하나로 지우고, 게이트웨이에서 인스턴스를 모두 지우고, 앱과 브랜치와 배포 기록을 지웁니다. 게이트웨이가 한 인스턴스라도 쓰기를 보류 중이면 409 `cluster_busy`이고 앱은 남습니다. 다시 부르면 처음부터 하되, 이미 지운 것은 건너뜁니다.
- `DELETE /v1/apps/{name}/instances/{instance}`는 그 인스턴스의 끝나지 않은 배포만 `superseded`로 끝내고, `apps/<instance>/`를 지우는 커밋을 하고, 게이트웨이에서 그 인스턴스를 지운 뒤, 브랜치와 그 배포 기록을 지웁니다. 오류와 다시 부르기는 앱 삭제와 같습니다.
- 두 삭제 모두 커밋부터 기록 삭제까지 인스턴스의 잠금을 잡고 있습니다. 그래서 그사이 만든 배포는 잠금을 기다렸다가 자기 기록이 지워진 것을 보고 멈추고, 지운 디렉터리를 다시 커밋하지 않습니다. 앱을 지우는 동안 브랜치가 더해지거나 지워지면 처음부터 다시 하고, 세 번 모두 바뀌면 500으로 답하고 앱을 남깁니다.
- 빌드 출력은 배포마다 2000줄까지 `log` 이벤트로 남깁니다. 배포 이벤트 스트림(`GET /v1/deployments/{id}/events`)은 저장소를 0.5초마다 읽어 새 이벤트를 보냅니다.

| 변수 | 설명 |
| --- | --- |
| `PLATFORM_DEPLOY_BUILDER` | `codebuild` 또는 `docker`. 비우면 실행기가 없고 배포가 `queued`에 머묾. 넣으면 GitHub App 설정이 필요하고, 없으면 서버가 시작하지 않음 |
| `PLATFORM_IMAGE_REGISTRY` | 실행기를 켜면 필수. 이미지를 push할 레지스트리 (예: `<계정>.dkr.ecr.ap-northeast-2.amazonaws.com`, E2E는 `localhost:5001`) |
| `PLATFORM_CODEBUILD_PROJECT` | `codebuild`이면 필수. 빌드를 돌릴 CodeBuild 프로젝트. 리전은 `PLATFORM_AWS_REGION`(기본 `ap-northeast-2`) |
| `PLATFORM_GKE_IMAGE_REGISTRY` | 선택. GKE용 Artifact Registry prefix (`<location>-docker.pkg.dev/<project>/<repository>`). 아래 인증 설정과 함께 `codebuild`에서만 사용. 둘 다 비우면 기존 ECR 전용 동작 유지 |
| `PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG` | 위 prefix와 함께 필요한 공개 AWS WIF ADC JSON 문자열. `type=external_account`, `credential_source.environment_id=aws1`. API의 GCS용 `GOOGLE_APPLICATION_CREDENTIALS`와 별개 |
| `PLATFORM_DOCKER_CLONE_URL` | `docker`일 때 저장소 주소의 틀. `{repo}`와 `{token}`을 바꿈 (기본 `https://x-access-token:{token}@github.com/{repo}.git`). 로컬 경로도 됨 |
| `PLATFORM_DEPLOY_WORKERS` | 동시에 실행하는 배포 수 (기본 2, 최대 32) |
| `PLATFORM_GITOPS_MODE` | `github`(기본, GitHub Git Data API) 또는 `git`(git CLI) |
| `PLATFORM_GITOPS_REPO` | `github` 모드의 gitops 저장소 (기본 `2026-Softbank-Hackathon-Charlie/gitops`). App 설치 토큰으로 쓰고, API 주소는 `PLATFORM_GITHUB_API_URL`. 이 저장소에는 push 토큰을 발급하지 않음(ADR 0023) |
| `PLATFORM_GITOPS_BRANCH` | gitops 브랜치 (기본 `main`, 두 모드 모두) |
| `PLATFORM_GITOPS_URL` | `git` 모드이면 필수. clone하고 push할 주소 (E2E의 로컬 git 서버) |
| `PLATFORM_ARGOCD_KUBECONTEXT` | Argo CD가 있는 클러스터의 kubeconfig 컨텍스트. 비우면 in-cluster 설정 |
| `PLATFORM_ARGOCD_NAMESPACE` | Application과 ApplicationSet이 있는 네임스페이스 (기본 `argocd`) |
| `PLATFORM_ARGOCD_APPSET` | 커밋 뒤 refresh를 요청할 ApplicationSet (기본 `hopcharlie-apps`) |
| `PLATFORM_ARGOCD_TIMEOUT` | 커밋 뒤 모든 환경이 healthy가 되고, 게이트웨이 백엔드 주소를 찾고, 그 주소에서 공개 서비스가 답하기까지 기다리는 시간 (기본 `10m`). 그 전에 `Degraded`가 1분 이어지면 바로 실패 |
| `PLATFORM_APP_BACKEND_TEMPLATE` | 게이트웨이 백엔드 주소의 틀. `{app}`, `{instance}`, `{service}`, `{port}`, `{env}`를 바꿈 (예: `http://{app}-{service}.{instance}.svc.cluster.local:{port}`). `{instance}`는 네임스페이스이고 기본 인스턴스에서는 앱 이름과 같음. 비우면 Service의 LoadBalancer 주소를 읽음 |

- CodeBuild 프로젝트는 소스 없이 인라인 buildspec으로 돌고, docker를 쓰므로 privileged 모드가 필요합니다. 프로젝트 역할에는 ECR push와 `hopcharlie/apps/*` 저장소를 만드는 권한이, 플랫폼 역할에는 `codebuild:StartBuild`, `codebuild:BatchGetBuilds`와 빌드 로그 그룹의 `logs:GetLogEvents`가 필요합니다.
- GKE 이미지 설정을 켜면 모든 서비스 빌드가 끝난 뒤 CodeBuild 역할의 단기 자격 증명으로 GCP에 인증해 같은 이미지를 `<AR prefix>/<app>/<service>`에도 게시합니다. 원격 AR digest가 ECR digest와 다르거나 게시가 실패하면 배포를 중단합니다. GitOps의 `gke` 이미지만 AR 주소로 바꾸며 다른 환경과 저장된 빌드 결과는 ECR 주소를 유지합니다. 설정을 켜기 전에 ECR에만 올린 이미지는 새 빌드로 양쪽에 게시한 뒤 재배포해야 합니다.
- clone 토큰은 CodeBuild 환경 변수(PLAINTEXT)로 넘깁니다. 한 시간 뒤 만료되는 설치 토큰이지만 빌드 설정 화면에서는 보입니다. buildspec은 토큰을 출력하지 않고, fetch가 끝나면 remote 주소에서 토큰을 지운 뒤에 Dockerfile을 빌드합니다.
- Argo CD를 읽는 계정에는 `applications.argoproj.io`의 get, patch와 `applicationsets.argoproj.io`의 patch가 필요합니다. ApplicationSet patch가 거부되면 배포 이벤트에 한 번 남기고, ApplicationSet이 스스로 다시 도는 것을 기다립니다.
