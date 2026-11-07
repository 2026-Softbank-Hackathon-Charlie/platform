# 0049. DB가 없는 앱도 이전 대상으로 두고, 트래픽과 파일만 옮기는 계획을 만든다

- 상태: 제안
- 날짜: 2026-11-07
- 관련 ADR: [0001](0001-openapi-single-contract.md), [0016](0016-app-object-storage.md), [0030](0030-app-db-topology.md), [0032](0032-registered-app-files.md), [0043](0043-add-environment-to-registered-app.md), migration-engine ADR 0012(target.app), 0017(`source.kind`), 0018(DB 없는 앱)
- 관련 PR: 이 PR(플랫폼 #124), migration-engine `feat/stateless-migration`(계획의 `kind: none`, `FileSyncer.Copy`, `Readable`)

## 상황

콘솔의 이전 화면은 `GET /v1/apps`에서 `environments`가 2개 이상인 앱만 고를 수 있게 한다. 지금 `environments`를 채우는 앱은 앱 파일(PLATFORM_APPS_FILE)의 앱과, GitHub에서 등록했고 DB 짝(`app_databases`)이 있는 앱뿐이다.

확인한 사실:

- docker-doom은 GitHub에서 등록했고 platform.yaml에 postgres 리소스가 없다. eks와 gke에 배포되어 정상이지만 `environments: []`로 나오므로 이전 화면에서 고를 수 없다.
- 계획 초안(`internal/apps/draft.go`)은 두 환경의 CNPG 클러스터와 DSN을 반드시 채운다. 정책(`internal/plans/policy.go`)은 게이트웨이 범위의 앱을 DB 호스트로 정한다. DB가 없는 계획은 앱을 정할 근거가 없다.
- 팀 요구: DB가 없는 앱도 버튼 하나로 이전할 수 있어야 한다.

엔진(ADR 0018, `feat/stateless-migration`)은 계획의 `source.kind: "none"`과 `target.kind: "none"`(둘 다)을 DB 없음으로 받는다. 이때 두 끝점은 `env`만 갖고, `gateway`가 필요하며, `files`는 지금처럼 고를 수 있고, `target.app`은 warm(기본)이나 cold이며 canary도 된다.

## 결정

### 이전 대상: postgres 리소스가 없고 환경이 2개 이상인 등록 앱

`apps.Registry.app`은 저장된 기본 인스턴스 spec을 먼저 읽는다.

- postgres 리소스가 있으면 지금과 같다. DB 짝이 있을 때만 이전 대상이다.
- postgres 리소스가 없고, DB 짝이 없고, 공개 서비스(`public: true`)가 있고, 등록 환경이 2개 이상이면 이전 대상이다. 게이트웨이가 옮기는 것은 공개 서비스의 트래픽뿐이라 공개 서비스가 없는 앱(worker만 있는 앱)은 옮길 것이 없다. DB 짝이 있는데 spec에서 postgres가 빠진 앱은 어느 쪽에도 맞지 않으므로 초안과 정책 모두 대상에서 뺀다. `environments`는 앱의 등록 환경 전부(PLATFORM_ENVS_FILE 순서)이고 각 항목은 `env`와, s3 리소스가 있고 서버가 그 환경의 파일을 옮길 때(ADR 0032) `bucket`만 갖는다. `cnpgCluster`와 `dbHost`는 없다.
- 환경이 1개면 지금과 같다(`environments: []`).
- 저장된 spec이 decode되지 않거나 NULL이면 `GET /v1/apps`를 실패시키지 않는다. 로그를 남기고 `resources`를 둘 다 true(모름, 콘솔이 아무것도 숨기지 않음)로 보이며 이전 대상이 아니다.

레지스트리는 PLATFORM_APP_DB_HOST_TEMPLATES와 상관없이 항상 켠다. 템플릿이 없으면 DB 짝의 끝점이 없을 뿐이고(DB 앱은 `environments: []` 그대로), DB 없는 앱은 템플릿이 필요 없다.

이런 앱의 `database`는 `{name: "", user: "", schemas: []}`이다.

### API: `App.resources`

모든 앱에 `resources: {postgres: bool, s3: bool}`를 둔다. 콘솔은 이것으로 이전 지도의 DB와 S3 노드를 숨긴다.

- 앱 파일의 앱: `postgres: true`, 어느 환경이든 `bucket`이 있으면 `s3: true`.
- 등록 앱: 기본 인스턴스의 저장된 platform.yaml에 postgres, s3 리소스가 있는지.

`AppEnvironment`의 `cnpgCluster`, `dbHost`는 필수에서 뺀다(DB 없는 앱에는 없다). Go 타입은 그대로 `string`(`omitempty`)이라 기존 앱의 JSON은 바뀌지 않는다.

### 계획 초안

DB 없는 앱의 `POST /v1/plans/draft`는 다음 계획을 만든다.

- `source`, `target`은 `{kind: none, env}`뿐이다. context, namespace, cluster, dsn이 없다.
- `gateway`는 mode `http`, url은 앱 범위 `<PLATFORM_GATEWAY_URL>/apps/<app>`, token은 `${GATEWAY_TOKEN}`, backend는 각 환경의 `gatewayBackend`다. 트래픽 전환이 이전의 전부이므로 게이트웨이가 없는 서버는 400으로 거절한다.
- 초안 API는 게이트웨이의 앱 목록을 읽어 그 앱의 `backends`에 두 backend가 모두 있는지 본다. 없으면 400 `gateway_backend_missing`이다(배포가 아직 그 환경에 닿지 않은 앱). 게이트웨이를 읽을 수 없는 서버는 503 `gateway_not_configured`다.
- `files`는 s3 리소스가 있고 두 환경 모두 버킷이 있으면 넣는다(ADR 0032 규칙, `files: false`면 뺀다).
- `target.app`은 넣지 않는다(warm). 정책이 아직 `target.app`을 받지 않기 때문이다.
- `verify`는 없다. `sourceStats`와 `writtenRecordsStream`은 측정하거나 검증할 DB가 없으므로 400이다.

`EnginePlanEndpoint`에 `kind`(enum `cnpg`, `external`, `none`)를 더하고, `namespace`, `cluster`, `dsn`은 필수에서 뺀다(`kind: none`이 아니면 엔진이 요구한다). 값은 엔진 `feat/external-source`의 이름과 같다.

docker-doom eks에서 gke로의 초안:

```json
{
  "id": "docker-doom-eks-to-gke",
  "source": {"kind": "none", "env": "eks"},
  "target": {"kind": "none", "env": "gke"},
  "gateway": {"mode": "http", "url": "http://<gateway>/apps/docker-doom", "token": "${GATEWAY_TOKEN}", "sourceBackend": "eks", "targetBackend": "gke"}
}
```

### 정책

`Policy.StatelessApps`(앱에서 환경별 `StatelessEnv {Backend, Bucket}`으로)를 둔다. 서버는 계획을 읽을 때마다 `apps.Catalog.RegistryStateless`로 채운다(`plans.AppDatabases`에 메서드 추가).

- 제출한 그대로의 JSON에서 `source.kind`와 `target.kind`를 읽는다. 하나만 `none`이면 거절한다(DB를 DB 없는 앱으로, 또는 그 반대로 옮기는 계획은 없다). 둘 다 `none`이면 DSN이 있으면 거절한다.
- DB 없는 계획은 gateway.mode `http`가 필요하고, `verify.writtenRecords`는 거절한다.
- 게이트웨이 범위: DB 호스트가 없으므로 범위만이 앱을 정한다. `gateway.url`은 정확히 `StatelessApps`에 있는 앱의 `GatewayAppURL`이어야 하고, 그 앱이 `AppDBHosts`에 있으면(DB가 있는 앱) 거절하고, `source.env`와 `target.env`가 둘 다 그 앱의 등록 환경이어야 한다. 루트는 안 된다. 그래서 DB 있는 앱의 트래픽을 DB 없이 옮기는 계획은 통과하지 못한다. DB 계획의 범위 규칙은 그대로라 DB 계획이 DB 없는 앱의 범위를 쓸 수도 없다.
- backend: `gateway.sourceBackend`, `targetBackend`는 정확히 두 환경의 `gatewayBackend`여야 한다. 다른 앱의 backend로 트래픽을 보낼 수 없다.
- 파일: `files.source`, `files.target`은 정확히 그 앱의 `source.env`, `target.env` 버킷이어야 한다. 등록된 버킷이기만 하면 되는 DB 계획과 달리, 범위가 앱을 정하므로 다른 앱의 버킷을 쓸 수 없다. `RegistryBuckets`에도 DB 없는 앱의 버킷을 더한다.

### 앱 식별: `PlanSummary.app`, `Run.app`, `PlanEndpoint.kind`

DB 있는 실행은 끝점의 namespace가 앱(인스턴스)을 가리킨다. DB 없는 실행은 namespace가 비어 있으므로 앱을 따로 적는다.

- `PlanSummary.app`: DB 없는 계획이면 정책이 범위에서 정한 앱. DB 계획에는 없다(지금과 같다).
- `Run.app`: 시작할 때 `PlanSummary.app`을 옮겨 적는다.
- `PlanEndpoint.kind`: DB 없는 계획의 끝점은 `none`, 그 밖에는 없다.
- `appRuns`(환경 추가와 제거의 `migration_in_progress`, 제거의 `write_backend`, DB 준비 상태)는 namespace에 더해 `Run.app`으로도 앱의 실행을 찾는다. 그래서 DB 없는 이전이 진행 중이면 환경을 더하거나 뺄 수 없고, 이전한 환경은 뺄 수 없다.

### 게이트

`plans.NoDatabase(p)`로 DB 없는 계획을 알아본다. 엔진의 `Plan.Stateless()`(두 kind가 모두 `none`)와 같다.

| 게이트 | DB 없는 계획 | 기록 값 |
|---|---|---|
| PostgreSQL 버전 | 건너뛴다 | `no_database`("DB 없음") |
| 용량 | 건너뛴다 | `no_database` |
| 동등성(감사) | 건너뛴다(CNPG 클러스터를 보는 탐침) | `no_database` |
| 데이터 거주 | 파일이 없으면 건너뛴다. 파일이 있으면 파일만 평가한다 | `no_database` 또는 평소 결과 |
| 원본 보존(retention) | 잡지 않는다. 지울 원본 DB가 없다 | 없음 |

거주 정책에는 이동 종류 `object-copy`를 더한다. source는 환경만, inventory는 비어 있고 `files`가 꼭 있어야 한다(Go 입력 검사). rego의 클러스터와 테이블 규칙(`unclassified_cluster`, `unclassified_dataset`, `inventory_catalog_mismatch`, `country_not_allowed`)은 `physical-replication`에만 적용하고, 버킷 규칙은 그대로 적용한다.

실행기의 동시 실행 키는 DB 없는 계획이면 클러스터 대신 `gateway:<범위 URL>`이다. 같은 앱의 두 이전이 겹치지 않는다. canary와 observe는 다른 게이트웨이 실행과 같다.

### 파일 복사(fileguard)

엔진(ADR 0018)은 DB 없는 계획의 파일을 `FileSyncer.Copy`로 옮기고(대상의 객체를 지우지 않는다), 옮기기 전에 `Readable`로 두 버킷을 읽을 수 있는지 본다. `internal/fileguard.Syncer`는 다음처럼 구현한다.

- `Copy`: `Sync`와 같은 경로에 `rclone copy`를 쓴다. 매 패스 원본의 새 스냅샷을 받아 검사하고, `--ignore-times`로 올리고, `check --download --one-way`로 확인한다.
- `Readable`: 가드의 두 버킷 중 하나에만 `rclone lsf <remote> --max-depth 1`을 같은 rclone 설정과 환경으로 실행한다. 그 밖의 remote는 `ErrDenied`다.

주의: fileguard는 쓰기를 붙잡은 동안의 마지막(fence) 복사를 포함해 매 패스 원본 전체를 스냅샷으로 받고 검사한다. 버킷이 크면 그 시간이 `maxDowntime`을 넘어 엔진이 되돌릴 수 있다. 큰 버킷을 가진 DB 없는 앱은 지금은 `files: false`로 옮기거나 버킷을 줄여야 한다.

### AI 실행

`POST /v1/ai-runs`는 `resources.postgres`가 false인 앱이면 409 `no_database`로 거절한다. AI 실행기는 DB 이전을 계획하고 리허설하는 도구다. 콘솔은 이런 앱에서 AI 단계를 건너뛰고 초안을 그대로 제출한다.

### OpenAPI 계약(ADR 0001)

- `App.resources`(필수, `AppResources {postgres, s3}`)
- `AppEnvironment.cnpgCluster`, `dbHost` 선택
- `EnginePlanEndpoint.kind`(`cnpg`, `external`, `none`), `namespace`, `cluster`, `dsn` 선택
- `PlanEndpoint.kind`, `PlanSummary.app`, `Run.app`(선택)
- `POST /v1/ai-runs` 409 `no_database`, `POST /v1/plans/draft`의 DB 없는 앱 설명과 400 `gateway_backend_missing`

## 결과

- docker-doom처럼 DB가 없고 두 환경에 등록한 앱이 이전 화면에 나오고, 초안부터 `POST /v1/migrations`까지 같은 버튼 흐름을 탄다.
- 기존 앱의 `GET /v1/apps` 응답은 `resources`만 늘고, 초안은 바뀌지 않는다.

## 남은 일

- 엔진 의존성은 엔진 main 72400cd(외부 원본 #24, DB 없는 앱 #26, 스키마 없는 verify #27이 합쳐진 커밋)다.
- 콘솔: `resources`로 DB, S3 노드를 숨기고 `no_database` 앱은 AI 단계를 건너뛴다.

## 검증

- `internal/apps/stateless_test.go`: docker-doom 같은 앱의 목록, 초안 JSON, 거절, 파일, `resources`
- `internal/plans/stateless_test.go`: kind 검사, 범위, backend, 버킷 규칙, 게이트 건너뛰기, 요약의 app과 kind
- `internal/runs/stateless_test.go`: canary와 observe를 단 DB 없는 실행, `Run.app`, 보존 없음, 범위 키로 같은 앱의 두 번째 실행 거절
- `internal/api/app_envs_test.go`: DB 없는 실행이 환경 추가와 제거를 막음
- `internal/fileguard/transfer_test.go`: `Copy`, `Readable`
- `internal/preflight/residency/bucket_test.go`: `object-copy`
- `internal/api/stateless_test.go`: `GET /v1/apps/{name}`, 초안, `gateway_backend_missing`, AI 실행 409, 제출, 승인, `POST /v1/migrations` 202, `Run.app`, 다른 앱 범위 거절

## 검토한 대안

- DB 없는 앱에 빈 DB 짝을 만든다: 없는 클러스터를 가리키는 DSN이 생기고 엔진이 복제를 시도한다. 버렸다.
- 끝점에 namespace를 넣어 앱을 정한다: 엔진 계약이 `env`만 갖기로 했고, 게이트웨이 범위가 이미 앱 하나만 가리킨다. 범위로 정했다.
