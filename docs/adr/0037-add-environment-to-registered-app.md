# 0037. 등록한 앱에 환경을 나중에 더하고, 한 환경 앱의 DB도 처음부터 짝으로 만든다

- 상태: 제안
- 날짜: 2026-11-07
- 관련 ADR: [0001](0001-openapi-single-contract.md), [0011](0011-github-app-deploy.md), [0015](0015-branch-instances.md), [0016](0016-app-object-storage.md), [0019](0019-app-secrets-and-redeploy.md), [0025](0025-app-postgres-backups.md), [0030](0030-app-db-topology.md), [0032](0032-registered-app-files.md), infra ADR 0063, 0074(차트의 `peerPresent`, 이 ADR의 짝), migration-engine ADR 0012
- 관련 PR: 이 PR(플랫폼), infra #131(차트)

## 상황

운영자는 콘솔의 앱 상세에서 한 환경에 등록한 앱에 환경을 하나 더하고(예: eks에만 있는 앱에 gke), 그 뒤 이전 버튼(`POST /v1/migrations`)으로 postgres DB와 파일까지 새 환경으로 옮길 수 있어야 한다. 버튼 하나로 끝나야 하고 앱마다 손으로 하는 단계가 없어야 한다.

확인한 사실:

- `store.App.Environments`는 등록 때 정하고 바꾸는 API가 없었다. 배포(`newDeployment`)는 모든 인스턴스를 이 목록 전체에 배포한다.
- DB 짝(ADR 0030)은 등록할 때 환경이 정확히 두 개인 앱만 받았다. 한 환경 앱의 DB는 토폴로지 없이 태어난 primary이고, primary는 나중에 replica가 될 수 없다(system id). 그래서 새 환경의 DB는 원래 DB의 replica로 태어나야 한다.
- ApplicationSet은 이미 있는 CNPG Cluster의 `replica`, `externalClusters`, `certificates`를 적용하지 않는다(`ignoreDifferences`). 이 세 필드는 Cluster를 만들 때만 들어간다.
- 2026-11-07 kind 리허설에서 `replica.self == replica.primary`이고 `source`가 아직 없는 상대를 가리키는 primary는 상대 없이도 healthy였다. 앱의 읽기와 쓰기도 그대로 됐다.
- 지금 운영에 한 환경으로 등록한 postgres 앱은 없다.
- 게이트웨이 등록(`gatewaySpec`)은 쓰기 백엔드가 이미 있으면 그대로 두고 새 백엔드를 읽기 가중치 0으로 더한다. 새 환경은 이전 전까지 요청을 받지 않는다.
- 버킷과 키(ADR 0016), 앱 시크릿 봉인(ADR 0019), 백업 버킷(ADR 0025)은 배포가 배포 대상 환경마다 만든다. 새 환경이 대상에 들어가면 따로 할 일이 없다.
- gitops 파일 `apps/<instance>/<env>.yaml`이 지워지면 ApplicationSet이 Application `<instance>-<env>`를 지우고, 템플릿의 `resources-finalizer.argocd.argoproj.io`로 Application이 만든 것을 모두 지운다. 네임스페이스는 차트가 그리므로 네임스페이스와 CNPG Cluster, PVC도 함께 지워진다. 앱 삭제(`RemoveApp`)가 쓰는 길과 같다.

이 ADR의 초안은 혼자 태어난 DB에 나중에 짝을 맞추는 방법(임시 RoleBinding으로 CNPG Secret을 읽고, CA를 다시 서명하고, 원본 Cluster를 patch한다)이었다. 검토에서 운영 DB를 patch하는 위험과 권한이 크다는 지적이 있었고, 그런 앱이 지금 없으므로 그 방법은 버린다(아래 "검토한 대안").

## 결정

### 등록: 한 환경 앱도 짝으로 태어난다 (ADR 0030 확장)

- `PLATFORM_APP_DB_TOPOLOGY=1`이고 `PLATFORM_SECRETS_KEY`가 있는 서버에서 postgres 리소스가 있는 앱을 한 환경으로 등록하면, 그 환경이 `PLATFORM_APP_DB_HOST_TEMPLATES`에 있고 템플릿이 있는 다른 환경이 정확히 하나일 때 그 환경을 peer로 `app_databases` 행을 만든다. 비밀번호, ECDSA CA, primary(등록한 환경), peer(다른 환경)는 두 환경 등록과 같다(`Server.pairEnvs`).
- 두 환경 등록은 그대로다. peer를 하나로 정할 수 없으면(템플릿이 셋 이상, 등록한 환경에 템플릿이 없음) 예전처럼 짝 없이 등록한다.
- DB는 처음부터 짝의 primary로 태어난다. 봉인한 `<cluster>-app`으로 initdb하고, CA는 `app-pg-ca`이고, `replica`는 `self = primary = <cluster>-<primary>`, `source = <cluster>-<peer>`이며, `externalClusters`에 상대가 있다. 상대는 아직 없다. Argo가 이 세 필드를 다시 적용하지 않으므로 나중에 원본 Cluster를 바꿀 일이 없다.
- 관리 EKS의 DB 경로(ADR 0030 추가)는 앱의 환경에 있는 쪽만 만든다. 등록 직후에는 primary 쪽 하나다.

### gitops 값: `values.dbTopology.peerPresent` (infra ADR 0074와의 계약)

```yaml
values:
  dbTopology:
    primary: "gke"
    peer: "eks"
    peerPresent: false   # peer가 앱의 환경이 아닐 때만, primary 파일에만 쓴다
```

- 앱의 환경(`app.Environments`)에 peer가 없으면 primary 파일에 `peerPresent: false`를 `peer` 다음에 쓴다. 차트는 Cluster, 봉인 Secret, 네임스페이스 레이블을 짝일 때와 같게 그리고, tailnet 객체(ingress Service `<cluster>-ts`, egress Service `<cluster>-to-<peer>`, 복제 NetworkPolicy)만 그리지 않는다.
- peer가 앱의 환경에 있으면 키를 쓰지 않는다. 차트는 키가 없으면 `true`로 읽는다. 그래서 두 환경으로 등록한 앱(moimroom-mig)의 파일은 바이트 단위로 그대로다.
- peer 파일은 `peerPresent`가 false일 동안 그리지 않는다. `Render`는 `PeerAbsent`인 토폴로지로 primary가 아닌 환경의 파일을 그리면 실패하고, 차트도 같은 값을 거부한다.
- 판단은 배포의 대상 환경이 아니라 앱의 환경으로 한다. 두 환경 앱의 한 환경만 배포해도 `peerPresent`는 빠진다.
- 플랫폼 저장소의 차트 사본 `deploy/charts/hopcharlie-app`을 infra #131의 차트로 맞춘다. 사본은 ADR 0030의 `dbTopology` 이후 옮겨지지 않았으므로 그 사이의 infra 변경(ingress, observability, 백업, dbTopology)도 함께 들어온다. `go test ./test/charts`의 `TestPlatformDBTopologyFile`이 `deploy.Render`가 쓴 primary 파일을 peer가 있을 때와 없을 때 차트로 그려 tailnet 객체만 다른지 확인한다.

### API: `POST /v1/apps/{name}/environments`

- operator 권한. 본문은 `{"environment": "gke"}`(`AppEnvironmentAddRequest`)다.
- 성공하면 202와 `AppEnvironmentAdded {app, deployment}`를 돌려준다. `app`은 `GET /v1/apps/{name}`과 같은 모양이고(readiness는 없다) `deploy.environments`에 새 환경이 있다. `deployment`는 큐에 넣은 기본 인스턴스 배포이고 trigger는 `environment`다. `Location`은 `/v1/apps/{name}`이다.
- 배포는 기본 인스턴스의 마지막 healthy 배포의 커밋, 이미지, platform.yaml을 그대로 쓰고 빌드하지 않는다(redeploy와 같다). 시크릿은 지금 값으로 다시 봉인한다. redeploy와 달리 앱의 모든 환경으로 간다. supersede는 redeploy와 같다. 진행 중인 push나 수동 배포가 있으면 이 배포가 스스로 물러나고, 나중에 온 push나 수동 배포는 이 배포를 대체한다. 그래서 예전 커밋이 새 커밋을 덮지 않는다. push는 앱의 모든 환경으로 가므로 새 환경도 그 push로 배포된다.
- 이미지는 digest로 고정되어 있다. gke 이미지는 빌드 때 Artifact Registry에도 올라간다(infra ADR 0042). 레지스트리에 이미지가 있는지 미리 보는 싼 방법이 없으므로 확인하지 않는다. 이미지가 지워졌으면 새 환경 파드가 ImagePullBackOff로 Degraded가 되어 배포가 `DegradedTimeout` 뒤 실패하고, 운영자는 브랜치를 배포해(빌드) 다시 시도한다.
- 검사 순서와 오류:

| 상태 | code | 언제 |
|---|---|---|
| 400 | `invalid_request` | `environment`가 없거나 비었다 |
| 404 | `app_not_found` | 등록한 앱이 아니다 |
| 409 | `app_not_registered` | `PLATFORM_APPS_FILE`에만 있는 앱이다 |
| 404 | `env_not_found` | 서버에 없는 환경이다. 메시지에 서버의 환경 목록을 쓴다 |
| 409 | `environment_exists` | 이미 앱의 환경이고, 기본 인스턴스의 배포 중 그 환경을 담은 가장 새 배포가 healthy로 끝났다 |
| 409 | `deployment_in_progress` | 기본 인스턴스에 끝나지 않은 배포가 있다(앞선 환경 추가 배포 포함) |
| 409 | `migration_in_progress` | 앱 네임스페이스의 이전 run이 `running`, `awaiting_switch`, `halted`, `interrupted`이거나 게이트웨이가 앱의 쓰기를 붙잡고 있다 |
| 409 | `nothing_to_redeploy` | 기본 인스턴스에 healthy 배포가 없다. 먼저 브랜치를 배포한다 |
| 409 | `db_pair_unavailable` | 마지막 healthy 배포의 platform.yaml에 postgres가 있는데, 짝 행이 없거나 짝이 이 환경과 상관없거나, 앱 이름이 `PLATFORM_APPS_FILE`에도 있다 |
| 503 | `storage_not_configured` | `s3` 리소스가 있는데 새 환경에 storage 드라이버가 없다 |
| 503 | `sealing_not_configured` | s3, postgres(짝), 앱 시크릿 중 하나가 있는데 새 환경에 쓸 수 있는 sealed-secrets 인증서가 없다 |

- postgres 앱은 이 환경을 peer로 둔 짝이 있을 때만 환경을 더한다. 짝 없이 더하면 새 환경 DB가 독립 primary로 태어나 다시는 이전할 수 없기 때문이다. 정적 카탈로그에도 있는 이름은 이전이 카탈로그 항목을 쓰므로 같은 이유로 거절한다. postgres가 없는 앱은 짝 없이 아무 환경이나 더한다.
- 다시 보내기: 환경이 이미 있고 그 환경을 담은 가장 새 배포가 healthy면 409 `environment_exists`다. 환경이 있지만 그 배포가 실패했거나 대체됐고 진행 중인 배포가 없으면 환경 목록은 그대로 두고 새 배포를 넣어 202를 돌려준다. 그래서 실패한 추가는 같은 요청으로 다시 한다.
- 백업 버킷(ADR 0025)의 행은 환경을 빼도 남고 경로는 앱 등록 시각이다. 그래서 aws 환경을 뺐다가 다시 더하면 새 replica가 예전 replica가 남긴 WAL이 있는 경로를 쓴다. Barman의 빈 보관소 검사에 걸릴 수 있으나 확인하지 않았다(아래 "남은 일").
- 브랜치 인스턴스(ADR 0015)는 짝 대상이 아니다. 앱의 환경 목록이 바뀌므로 다음 배포(push 또는 수동) 때 새 환경에 독립 DB로 간다. 이 API는 브랜치 인스턴스 배포를 넣지 않는다. 콘솔에는 그 인스턴스의 새 환경이 `pending`으로 보인다.

### 동기로 하는 일과 순서

1. 검사를 모두 통과하면 `store.SetAppEnvironments(name, from, to)`로 환경 목록을 바꾼다. `to`는 지금 목록 뒤에 새 환경을 붙인 것이다. 첫 환경은 늘 처음 등록한 환경(짝의 primary)으로 남고, 빼기는 이 첫 환경을 거절한다. 등록할 때의 목록만 `PLATFORM_ENVS_FILE` 순서다. 읽은 목록(`from`)과 지금 목록이 다르면 쓰지 않고(`ErrConflict`) 409 `environment_exists`로 끝낸다.
2. 배포를 저장하고 큐에 넣는다. 배포 저장이 실패하면 환경은 더해진 채 남고, 같은 요청을 다시 보내면 위의 "다시 보내기"로 배포만 넣는다.
3. 짝이 있으면 `DBRoutes.Changed`로 관리 EKS의 DB 경로를 앱의 환경에 맞춘다(백그라운드).

API는 워크로드 클러스터에 아무것도 쓰지 않는다. 클러스터의 변화는 모두 배포 잡이 인스턴스 잠금 안에서 저장 뒤에 한다(`deploy.go`의 계약).

- `store.UpdateApp`은 이제 `Environments`를 바꾸지 않는다. 배포 잡의 `updateSpec`은 잡을 시작할 때 읽은 앱을 통째로 다시 쓰므로, 그 사이 더하거나 뺀 환경을 되돌릴 수 있었다. 환경은 `SetAppEnvironments`만 바꾼다.
- 배포 잡은 커밋 직전, 인스턴스 잠금 안에서 앱을 다시 읽는다. 배포가 앱에 없는 환경을 담고 있으면 아무것도 쓰지 않고 실패하고, 봉인과 `peerPresent`는 다시 읽은 앱의 환경으로 정한다.

### API: `DELETE /v1/apps/{name}/environments/{env}` (빠져나가는 길)

잘못 더했거나 새 환경 배포가 healthy가 되지 않을 때 쓴다.

- 검사 순서와 오류: 404 `app_not_found`, 409 `app_not_registered`, 404 `env_not_found`(앱의 환경이 아니다), 409 `primary_environment`(앱의 첫 환경이거나 짝의 primary다. 앱은 환경을 하나 이상 가진다), 409 `write_backend`(아래), 409 `deployment_in_progress`(앱의 어느 인스턴스든 끝나지 않은 배포), 409 `migration_in_progress`(진행 중인 run 또는 게이트웨이가 쓰기를 붙잡음), 502 `github_unreachable`(gitops), 502 `gateway_unreachable`, 503 `github_not_configured`(배포기 없음).
- `write_backend`: 그 환경이 쓰기 백엔드였던 적이 있으면 거절한다. 게이트웨이가 어느 인스턴스든 쓰기나 읽기를 그 환경으로 보내고 있거나, 앱 네임스페이스의 이전 run 중 `failed`, `aborted`가 아닌 것이 그 환경을 source나 target으로 가졌으면 그렇다. 한 번이라도 승격된 DB는 앱의 데이터를 가지므로 이 API로 지우지 않는다. 지우려면 앱을 삭제한다.
- 실행(`Executor.RemoveEnvironment`): 앱의 모든 인스턴스 잠금을 잡고, 잠금 안에서 앱과 인스턴스 목록을 다시 읽고, 끝나지 않은 배포가 없음을 다시 확인한다. 그 다음 순서대로 한다.
  1. 모든 인스턴스의 `apps/<instance>/<env>.yaml`을 한 커밋으로 지운다(`GitOps.Delete`가 디렉터리와 함께 파일 하나도 받는다). ApplicationSet을 refresh한다.
  2. 각 인스턴스의 게이트웨이 앱에서 그 환경 백엔드를 뺀다. 쓰기 백엔드와 다른 읽기 가중치는 그대로다. 비밀번호가 가려진 백엔드가 있으면 다시 보내면 비밀번호가 바뀌므로 실패한다.
  3. 마지막에 `SetAppEnvironments`로 환경 목록에서 뺀다. 앞 단계가 실패하면 환경이 남으므로 같은 요청으로 다시 한다.
- 그 뒤 API는 `DBRoutes.Changed`로 그 환경의 관리 경로를 지우고, 기본 인스턴스에 healthy 배포가 있으면 남은 환경으로 trigger `environment` 배포를 넣는다. 이 배포가 primary 파일에 `peerPresent: false`를 다시 쓴다. healthy 배포가 없으면 넣지 않고 primary 파일은 다음 배포 때 바뀐다. 그때까지 tailnet 객체가 없는 상대를 가리킨 채 남지만 앱에는 영향이 없다.
- 지워지는 것: Argo CD가 Application `<instance>-<env>`를 지우고 finalizer로 네임스페이스와 그 안의 DB, PVC, 봉인 Secret을 지운다. 앱 삭제와 같다. 인스턴스의 버킷과 키는 storage 정리(`CleanupStorage`)가 지운다. 정리는 앱, 인스턴스, 그리고 이제 행의 환경이 앱의 환경인지로 행이 쓰이는지 정하고, 지우기 직전 인스턴스 잠금 안에서 다시 확인한다. 그 사이 환경을 다시 더했으면 버킷을 남긴다. 백업 버킷(ADR 0025)은 앱 삭제 때처럼 남는다.
- `app_databases` 행은 남긴다. 원본 Cluster의 태어날 때 토폴로지가 그 peer 이름을 가리키므로 나중에 같은 환경을 다시 더하면 그대로 짝이 된다.

### DB 경로, 초안, 정책

- `dbroute`: 짝의 두 환경 중 앱의 환경에 있는 쪽만 경로를 만든다. `Sync(app)`은 짝과 앱을 읽어 없는 경로를 만들고 앱에 없는 환경의 경로를 지운다. 짝이 없으면 앱의 경로를 모두 지운다. 주기 실행(`Reconcile`)도 짝이 있는 앱의 경로 중 앱에 없는 환경의 것을 지운다. 짝 목록이나 앱 목록을 읽지 못하면 아무것도 지우지 않는다. `Pairs`는 `GetApp`, `ListApps`도 읽는다.
- `apps.Registry`: 카탈로그 항목(`GET /v1/apps`의 환경별 cluster와 dbHost), 이전 초안, 정책의 비밀번호 변수와 허용 호스트, 파일 버킷, `SetRegistryPasswords`는 짝의 환경 중 앱의 환경에 있는 쪽만 쓴다. peer가 아직 없는 앱의 초안은 target 환경이 없어 `invalid_request`로 끝난다.
- `GET /v1/apps`의 `deploy.dbTopology`에 `replicaPresent`(replica가 앱의 환경인지)를 더한다.

### 준비 상태: `GET /v1/apps/{name}`의 `deploy.dbTopology.readiness`

`GET /v1/apps/{name}`에서만 채운다. 목록은 클러스터를 읽지 않는다. `AppDBReadiness {ready, reason, message, environments[]}`이고 `environments`는 primary 먼저 두 환경이다(`env`, `inApp`, `deployed`, `gatewayBackend`, `role`, `healthy`, `phase`).

검사는 이 순서이고 처음 실패한 것이 `reason`이다. 모두 통과하면 `ready`다.

1. `replica_missing`: 짝의 환경 중 앱의 환경이 아닌 것이 있다. 그 환경의 Cluster는 읽지 않는다.
2. 환경마다 기본 인스턴스 배포 중 그 환경을 담은 가장 새 배포를 본다. healthy면 `deployed`다. 아니면 진행 중이거나 대체됐거나 없으면 `deploy_pending`, 그 환경이 Degraded로 실패했으면 `app_degraded`, 그 밖의 실패는 `deploy_failed`다. 여러 환경이 다르면 `deploy_pending`, `app_degraded`, `deploy_failed` 순으로 고른다. `app_degraded`의 메시지는 앱이 기동할 때 DB에 쓰는 것 같으니 엔진의 cold 모드(migration-engine ADR 0012)로 옮기라고 쓴다.
3. 공개 서비스가 있고 게이트웨이가 있으면 게이트웨이 앱에 두 환경 백엔드가 있어야 한다. 읽지 못하면 `gateway_unreachable`, 없으면 `gateway_backend_missing`이다.
4. 두 환경의 CNPG Cluster `<app>-<pg>`를 네임스페이스 `<app>`에서 환경마다 3초 안에 병렬로 읽는다(플랫폼의 workload kubeconfig, `cnpg.Client.Get`). 읽지 못하면 `cluster_unreadable`, phase가 healthy가 아니거나 준비된 인스턴스가 모자라면 `cluster_not_healthy`다.
5. 한쪽은 primary(`replica.primary`가 자기이거나 없음)이고 다른 쪽은 그것을 따라야 한다. 엔진 precheck의 `checkTopology`는 내보내지 않는 함수라 같은 규칙을 플랫폼에 둔다(`pairTopology`). 서로의 `self`, `primary`, `source`가 맞지 않으면 `topology_mismatch`다. 이전한 뒤 역할이 바뀐 짝도 이 규칙으로 `ready`다.

WAL lag와 streaming 연결은 보지 않는다. Cluster 상태에 없고, 엔진의 prepare(대상 replica 연결과 recovery 확인)와 presync(`maxLagBytes`)가 이전할 때 다시 확인한다. 콘솔은 `readiness.ready`일 때만 이전 버튼을 켠다.

### OpenAPI 계약(ADR 0001)

`api/openapi.yaml`이 유일한 계약이고 이 PR의 첫 커밋이다.

- `POST /v1/apps/{name}/environments`(operationId `addAppEnvironment`), `DELETE /v1/apps/{name}/environments/{env}`(`removeAppEnvironment`), tag apps.
- `AppEnvironmentAddRequest {environment}`, `AppEnvironmentAdded {app, deployment}`, `AppEnvironmentRemoved {app, deployment?}`.
- `DeploymentTrigger` enum에 `environment`.
- `AppDBTopology`에 `replicaPresent`(필수)와 `readiness`(`AppDBReadiness`, 선택). `AppDBReadinessEnvironment.role`은 `primary`, `replica`, `unknown`이다.
- `pkg/apiv1`은 `go generate`로 다시 만든다. 콘솔의 `src/api/schema.d.ts`는 콘솔 저장소에서 다시 만든다.

### 권한

- 워크로드 두 곳의 플랫폼 계정: Cluster `get`만 쓴다. 이미 있는 권한이다(infra ADR 0063). Secret 권한과 RoleBinding 권한은 필요 없다.
- 관리 EKS: 바뀌지 않는다. DB 경로 Service의 `create`, `delete`는 ADR 0030 추가와 같다.

## 결과

- 한 환경으로 등록한 postgres 앱도 콘솔 버튼 하나로 다른 환경에 더하고 이전할 수 있다. 원본 DB는 처음부터 짝의 primary이므로 환경을 더할 때 원본 Cluster를 건드리지 않는다.
- 새 환경 DB는 `pg_basebackup`으로 원본을 따라 태어나고, 새 환경의 앱 파드는 replica 위에서 뜬다(ADR 0030과 같다). 기동할 때 스키마를 바꾸는 앱은 그 환경에서 Degraded가 되고, readiness가 `app_degraded`로 알려 준다. 이런 앱은 엔진의 cold 모드로 옮긴다.
- 한 환경 앱도 CA 키와 앱 비밀번호를 플랫폼이 정해 봉인한다. 환경을 더하지 않는 앱에도 같은 Secret이 생긴다. 범위는 앱 하나다.
- 이 기능 전에 한 환경으로 등록한 postgres 앱은 짝이 없어 `db_pair_unavailable`이다. 지금 운영에는 없다. 생기면 삭제하고 다시 등록한다.
- `GET /v1/apps/{name}`이 짝이 있는 앱에서 두 클러스터와 게이트웨이를 읽어 최대 약 3초 느려질 수 있다.
- 환경을 빼면 그 환경의 데이터가 지워진다. 그래서 쓰기 백엔드였던 적이 있는 환경은 빼지 않는다.

## 남은 일

- 환경을 뺐다가 다시 더한 aws 환경의 WAL 보관(위 백업 버킷). 필요하면 다시 더할 때 백업 경로를 새로 정한다.
- 빼기가 gitops 파일을 지운 뒤 게이트웨이 단계에서 실패하면 환경은 앱에 남고 파일은 없다. 같은 요청을 다시 보내면 끝난다. 그 사이의 push는 그 환경 파일을 다시 쓴다.

## 적용 순서

1. infra #131(차트 `peerPresent`) 병합. 그 전에 한 환경 postgres 앱을 등록하면 차트 스키마가 `peerPresent`를 거부해 sync가 실패한다. 키가 없는 파일의 출력은 그대로다.
2. 플랫폼 병합과 이미지 교체. 콘솔과 CLI 병합.
3. 운영에서 새 한 환경 postgres 앱을 등록하고, 환경 추가, `readiness.ready`, 이전 버튼, 환경 빼기를 확인한다.

## 검증

- 단위 테스트: 한 환경 등록의 짝 만들기와 짝이 없는 경우(`internal/api/appdb_test.go`), 환경 추가와 빼기의 성공과 오류마다(`app_envs_test.go`), readiness 이유마다(가짜 Cluster 읽기와 게이트웨이), `peerPresent` 렌더링과 배포 잡(`internal/deploy/environment_test.go`), 파일 하나 지우기(`gitops_git_test.go`, `gitops_github_test.go`), DB 경로 거르기(`internal/dbroute`), 초안과 정책 거르기(`internal/apps`), `SetAppEnvironments`와 바뀐 `UpdateApp`의 store conformance(메모리와 PostgreSQL).
- 차트: `go test ./test/charts`의 `TestPlatformDBTopologyFile`.
- kind E2E: 짝이 있는 앱을 다루는 E2E(`test/e2e/deploy`)가 없어 이 PR에서는 더하지 않았다. 환경 추가와 이전의 끝에서 끝까지는 적용 순서 3에서 확인한다.

## 검토한 대안

- 혼자 태어난 DB에 나중에 짝을 맞춘다(이 ADR의 초안): 원본 환경에 임시 RoleBinding을 만들어 CNPG가 만든 `<cluster>-app` 비밀번호와 `<cluster>-ca` 키를 읽고, 같은 키로 CA를 다시 서명하고, 운영 중인 원본 Cluster의 `replica`, `externalClusters`, `certificates`를 patch한다. 이미 등록한 앱에도 쓸 수 있지만, 플랫폼 계정에 Secret 읽기와 RoleBinding 권한이 필요하고 운영 DB를 patch한다. 그런 앱이 지금 없으므로 하지 않는다.
- 지금 DB를 새 환경의 replica로 바꾼다: primary는 replica가 될 수 없다(system id).
- 새 환경에 독립 DB를 만들고 이전 때 dump와 restore로 옮긴다: 엔진의 이전(CNPG 분산 토폴로지)과 다른 길이고 쓰기 정지가 데이터 크기에 비례한다.
- 앱을 삭제하고 두 환경으로 다시 등록한다(지금까지의 방법): DB와 파일이 사라진다.
- peer 파일도 그리되 차트가 Cluster를 빼게 한다: 그 환경에 빈 네임스페이스와 Application이 생기고, 게이트웨이 등록이 응답하지 않는 백엔드를 기다린다. 파일을 그리지 않는 쪽이 단순하다.
- `peerPresent: true`를 늘 쓴다: 두 환경으로 등록한 앱의 파일이 바뀌어 아무 변화 없는 커밋과 sync가 생긴다. 없으면 true로 정한다.
- 환경을 뺄 때 Application만 지우고 네임스페이스를 남긴다(`preserveResourcesOnDeletion`): 템플릿이 finalizer를 직접 쓰므로 Argo CD가 따르지 않고(infra의 ApplicationSet 주석), 남긴 DB는 아무도 관리하지 않는다. 앱 삭제와 같은 길로 지운다.
