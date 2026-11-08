# 0064. 세 환경 DB 그룹에서 환경 하나를 빼고 남은 두 환경을 짝으로 만든다

- 상태: 제안
- 날짜: 2026-11-08
- 관련 ADR: [0043](0043-add-environment-to-registered-app.md)(환경 더하기와 빼기, `peer_generation`), [0044](0044-three-environment-database-group.md)(세 환경 DB 그룹), infra ADR 0078(세 멤버 연결 목록과 primary의 source 변경 정책), infra ADR 0098(멤버 레이블이 있는 Cluster의 연결 목록), migration-engine ADR 0018(`allowSourceRewire`)

## 상황

ADR 0044는 세 환경 DB 그룹의 환경 제거를 409 `db_group_removal_unsupported`로 거절했다. 콘솔도 빼기 버튼을 막았다. 그룹에서 환경 하나를 빼려면 앱을 지우고 다시 등록해야 했다. 운영자는 콘솔의 빼기 버튼 하나(그리고 같은 API를 쓰는 CLI `charlie app env remove`)로 세 환경 중 하나를 빼고, 남은 두 환경 사이에서 계속 이전하고, 나중에 세 번째 환경을 다시 더할 수 있어야 한다.

확인한 사실:

- 멤버는 셋이다. P는 처음 primary로 태어난 환경(`PrimaryEnv`, 앱의 첫 환경)이고 Q는 처음 짝(`PeerEnv`), T는 나중에 더한 세 번째다. Q와 T는 P에서 `pg_basebackup`으로 태어나고 `replica.source`가 P다. P의 `replica.source`는 Q다(강등되면 따라갈 곳).
- `replica.source`가 바뀌는 길은 둘뿐이다. 태어날 때 차트가 정하는 값과, 엔진이 `allowSourceRewire`로 이전 직전에 현재 primary의 source를 이전 대상으로 바꾸는 것이다. 그래서 남는 replica가 따라가는 곳은 늘 P이거나 현재 primary다. 지우지 않는 P를 제외하면, 빼는 멤버를 가리킬 수 있는 것은 현재 primary의 `replica.source` 하나다.
- ApplicationSet은 Cluster의 `replica`, `certificates`, `bootstrap`을 무시하고(`RespectIgnoreDifferences=true`, client-side apply), `externalClusters`는 레이블 `hopcharlie.dev/db-topology-members: "3"`인 Cluster에서만 Git 값으로 맞춘다. 그래서 primary의 `replica.source`는 플랫폼이 직접 고쳐야 한다.
- 이 jq(`!= "3"`)는 Git 쪽과 라이브 쪽에 따로 적용된다. Git 파일에 레이블이 없고 라이브가 `"3"`이면 Argo CD는 Git 쪽 목록만 지우고 라이브 값을 채우지 않는다. apply가 last-applied 기준으로 `externalClusters`를 지우려 하고 CNPG가 거절한다. kind의 실제 Argo CD(argo-cd 차트 10.9.6)에서 sync가 `External cluster ... not found`로 재시도를 반복하는 것을 재현했다. 남는 두 환경을 레이블 없는 두 환경 값으로 다시 그리면 안 된다.
- workload admission 정책(infra ADR 0078)은 레이블이 `"3"`이고 `replica.primary == replica.self`인 Cluster에서 `replica.source`만, 이미 있는 다른 externalCluster로 바꾸는 것을 플랫폼 서비스 계정에 허용한다. 레이블이 빠진 뒤에는 거절한다. replica의 source 변경도 거절한다.
- CNPG는 `replica.source`가 `externalClusters`에 없는 Cluster를 거절한다(`External cluster ... not found`). source를 먼저 고치지 않고 두 환경 파일로 다시 그리면 Argo CD sync가 실패를 반복한다.
- 엔진의 두 환경 이전은 source의 `replica.source`가 target이고 target의 `replica.primary`, `replica.source`가 source여야 시작한다(`checkTopology`). readiness의 `pairTopology`도 같은 규칙이다.
- 외부 replica Cluster는 primary에 replication slot을 만들지 않는다. 빼는 멤버 때문에 primary에 WAL이 쌓이지 않는다.
- 백업 경로는 환경별 백업 버킷 안의 경로다. 지금은 `PeerEnv`만 `peer_generation`으로 경로를 새로 받는다. T는 언제나 `BackupPath(등록 시각)`을 쓴다.

## 결정

### 거절 조건

`DELETE /v1/apps/{name}/environments/{env}`는 앱이 DB 그룹을 가지고 환경이 셋일 때도 받는다. 두 환경 경로의 검사는 그대로 한다. 앱의 첫 환경이나 `PrimaryEnv`면 409 `primary_environment`, 끝나지 않은 배포가 있으면 409 `deployment_in_progress`, 파일 검사는 409 `environment_has_files`, 진행 중인 이전 run이나 게이트웨이의 쓰기 붙잡기는 409 `migration_in_progress`, 게이트웨이가 그 환경으로 쓰기나 읽기를 보내면 실행 중 409 `write_backend`다. 온보딩이 아직 컷오버하지 않은 DB(`source_kind` external)도 409 `migration_in_progress`로 거절한다.

이전 run 기록으로 정하는 `write_backend`(그 환경이 이전의 source나 target이었던 적이 있음)는 그룹 멤버에는 쓰지 않는다. 대신 살아 있는 복제 상태를 읽어 남는 두 멤버가 모든 쓰기를 가진다는 것을 확인한다. 이전 run은 승격 전에 대상이 따라잡을 때까지 쓰기를 붙잡으므로, 지금 primary가 아닌 멤버의 데이터는 모두 현재 primary에 있다. 그래서 예전에 이전의 대상이었던 멤버도 뺄 수 있다.

플랫폼은 그룹 멤버 세 Cluster `<app>-<pg>`를 병렬로 읽는다(각 3초, `DBClusterTimeout`). 다음 중 하나라도 맞지 않으면 아무것도 바꾸지 않고 거절한다.

- 남는 두 Cluster를 모두 읽는다. 못 읽으면 502 `db_topology_unknown`, 서버가 workload 클러스터를 읽거나 쓸 수 없으면 503 `db_topology_unknown`이다.
- 남는 두 Cluster가 모두 healthy다. 아니면 409 `db_topology_unready`다.
- 남는 둘 중 정확히 하나가 지정 primary(`replica.primary == replica.self`)이고, 다른 하나의 `replica.primary`와 `replica.source`가 그 primary다. 아니면 409 `db_topology_unready`다.
- 빼는 환경의 Cluster를 읽었다면 primary가 아니다. primary면 409 `write_backend`다. 읽지 못하면 막지 않는다. 게이트웨이가 그 환경으로 아무것도 보내지 않는 것은 실행이 확인하고, 그 DB는 지워진다. 노트북 onprem처럼 꺼진 환경도 뺄 수 있어야 하기 때문이다.
- primary의 `replica.source`가 남는 replica가 아니면 primary의 `externalClusters`에 남는 replica가 있다. 없으면 409 `db_topology_unready`다.

### 순서

1. (API, 위 검사 뒤) primary의 `replica.source`가 남는 replica가 아니면 남는 replica로 바꾼다. 엔진의 `cnpg.Client.RewireSource`를 그대로 쓴다. UID, resourceVersion, self, primary를 test하는 JSON patch라 읽은 뒤 바뀐 Cluster는 바꾸지 않는다. 이때 Cluster는 아직 레이블 `"3"`을 가지므로 admission 정책이 허용한다. 실패하면 502 `db_topology_unreachable`이고 다른 것은 바뀌지 않았다. 같은 요청을 다시 보내면 다시 읽고 이미 바뀌었으면 건너뛴다. 이 변경은 primary가 지금 하는 일을 바꾸지 않고, 세 멤버일 때도 엔진이 이전 직전에 하는 것과 같다. 그래서 이 단계 뒤 다른 단계가 실패해도 그룹은 그대로 안전하다.
2. (실행, 인스턴스 잠금 안) 각 인스턴스의 게이트웨이 앱에서 그 환경 백엔드를 뺀다(ADR 0043과 같다).
3. 모든 인스턴스의 `apps/<instance>/<env>.yaml`을 한 커밋으로 지우고 ApplicationSet을 refresh한다. Argo CD가 그 환경의 Application과 네임스페이스, DB, PVC, tailnet ingress를 지운다.
4. `store.RemoveDatabaseMember(app, env, peer)`로 그룹 행을 한 트랜잭션에서 바꾼다. 그 환경의 세대를 하나 올린다. 그 환경이 `PeerEnv`였고 환경이 셋이면 남는 다른 환경(첫 환경이 아닌 쪽)을 새 `PeerEnv`로 하고 그 환경의 세대는 그대로 둔다. `PrimaryEnv`는 바뀌지 않는다.
5. `SetAppEnvironments`로 환경 목록에서 뺀다(ADR 0043과 같다).
6. (API) `DBRoutes.Changed`가 관리 클러스터의 그 환경 DB 경로를 지운다. 기본 인스턴스의 healthy 배포가 있으면 남는 두 환경으로 trigger `environment` 배포를 넣는다. 이 배포가 두 파일을 `members: [P, 남는 다른 환경]`(`peer`는 남는 다른 환경)으로 다시 쓴다. 차트는 레이블 `"2"`, 두 항목의 `externalClusters`를 그리고, 빼는 환경으로 가는 egress Service와 NetworkPolicy 규칙은 그리지 않아 Argo CD prune이 지운다. tailnet egress proxy와 device는 Tailscale operator가 지운다.

infra ADR 0098은 ApplicationSet의 jq를 `select(.metadata.labels["hopcharlie.dev/db-topology-members"] == null) | .spec.externalClusters`로 바꾼다. 멤버 레이블이 있는 Cluster(`"2"`, `"3"`)는 목록을 Git이 관리하므로 `"3"`에서 `"2"`로 바뀌는 sync가 레이블과 두 항목 목록을 함께 적용한다. 1에서 source를 먼저 바꿨으므로 CNPG가 받아들인다. 이 infra 변경이 플랫폼 배포보다 먼저 적용되어야 한다.

이 선택은 그룹 행의 `explicit_members`에 남긴다. 한 번 그룹에서 멤버가 빠진 앱은 그 뒤 두 환경이어도 늘 `members`를 그린다. 그래서 다음 배포가 레이블 없는 값으로 돌아가지 않는다. 그 짝에서 peer까지 빼 한 환경이 되어도 primary 파일은 `members: [P, peer]`와 `peerPresent: false`로 레이블 `"2"`를 유지한다. 이 조합은 infra ADR 0098의 차트 변경이 두 멤버에만 허용한다.

### 그룹 행과 백업 경로

- migration `0020_app_database_member_generations.sql`이 `app_databases`에 `member_generations jsonb NOT NULL DEFAULT '{}'`와 `explicit_members boolean NOT NULL DEFAULT false`를 더한다. 앞의 것은 환경별로 그 환경을 앱에서 뺀 횟수다.
- 환경의 세대(`AppDatabase.Generation`)는 `member_generations`의 값(없으면 0)이고, `PeerEnv`는 그 값과 `peer_generation` 중 큰 값이다. 지금 있는 행은 고치지 않아도 경로가 같고, 이전 바이너리가 `peer_generation`만 올린 경우에도 세대가 낮아지지 않는다.
- 백업 경로는 모든 멤버에 같은 규칙을 쓴다. 세대가 0이면 `BackupPath(등록 시각)`, 아니면 그 뒤에 `-<세대>`를 붙인다. 다시 더한 멤버는 자기 환경 버킷의 비어 있는 새 경로를 받는다. 남는 멤버의 경로는 바뀌지 않는다(새 `PeerEnv`는 바뀌기 전 세대를 `member_generations`에 고정한다).
- `RemoveDatabaseMember`는 `peer_generation`을 늘 `PeerEnv`의 세대와 같게 쓴다. 이 변경 전 바이너리로 되돌리는 것은 세 환경 그룹에서 멤버를 한 번도 빼지 않았을 때만 안전하다. 뺀 뒤에는 이전 바이너리가 `members` 없이 그려 위의 Argo CD 문제가 생기고, 다시 더한 세 번째 멤버의 경로를 `-<세대>` 없이 그린다.
- 두 환경 빼기(ADR 0043)도 이 메서드를 쓴다. `peer`를 비우면 `PeerEnv`는 그대로이고 세대만 오르므로 `NextPeerGeneration`과 결과가 같다. `NextPeerGeneration`은 없앤다.

### 뺀 뒤

남는 두 환경은 `PrimaryEnv`와 새 `PeerEnv`로 된 짝이다. 파일은 두 멤버를 명시한다(위). readiness(`pairTopology`), 관리 DB 경로, 이전 초안(`allowSourceRewire` 없음), 보존 정리는 이 짝으로 동작한다. 남는 primary의 source가 남는 replica이고 replica가 primary를 따르므로 엔진의 두 환경 이전을 어느 방향으로든 바로 시작한다. 세 번째 환경을 다시 더하는 것은 ADR 0044 그대로다(첫 환경이 현재 primary여야 하고, 예전 것이 다 지워진 뒤에만 된다).

## 실패와 다시 하기

- 1의 검사나 source 변경에서 끝나면 아무것도 지우지 않았다.
- 실행은 인스턴스 잠금을 잡은 뒤 아무것도 바꾸기 전에 두 가지를 다시 본다. 앱의 이전 run이 진행 중이거나 게이트웨이가 쓰기를 붙잡고 있으면 409 `migration_in_progress`, 빼는 환경의 Cluster가 그 사이 primary가 됐으면 409 `write_backend`다. 1의 검사와 잠금 사이에 시작된 이전이 이것으로 막힌다. 잠금을 잡은 뒤 시작되는 이전은 막지 못하지만, 엔진의 precheck가 그 시점 상태로 다시 판단한다.
- source 변경의 조건부 patch가 test에서 실패하면(읽은 뒤 Cluster가 바뀜) 409 `db_topology_changed`, admission 정책이나 RBAC가 거절하면 502 `db_topology_denied`(인프라 문제라 다시 보내도 같다), 그 밖의 실패는 502 `db_topology_unreachable`이다. kind에서 오래된 resourceVersion의 test는 422 Invalid로 돌아왔다.
- 2, 3이 실패하면 ADR 0043처럼 502 `gateway_unreachable`, `github_unreachable`이고 같은 요청을 다시 보낸다. 다시 보내면 1의 검사를 다시 하고, source는 이미 맞으므로 바꾸지 않는다.
- 4 뒤 5가 실패하면 환경은 앱에 남고 그룹 행의 `PeerEnv`와 그 환경의 세대는 이미 바뀌었다. 다시 보내면 그 환경은 이제 세 번째 멤버로 보여 세대만 하나 더 오른다. 경로는 비어 있기만 하면 되므로 상관없다. 알려진 틈: 다시 보내기 전에 다른 배포(push)가 실행되면, 이미 gitops 파일이 지워진 그 환경의 파일을 새 세대 경로로 다시 쓴다. 그 Application과 DB는 Argo CD가 지우는 중이거나 지운 뒤라, 새 DB가 빈 새 경로로 다시 생길 수 있다. 같은 빼기 요청을 다시 보내면 끝난다. 이 경우는 store 실패 뒤에만 생기므로 렌더에서 막지 않고 기록만 한다.
- 6의 배포를 넣지 못하면 로그만 남기고 202다(ADR 0043과 같다). 남는 두 파일은 다음 배포 때 두 환경 값으로 바뀐다. 그때까지 빼는 환경으로 가는 egress Service가 남지만 아무도 쓰지 않는다.
- 파일 검사(`environment_has_files`, s3 리소스가 있고 공개 서비스가 없는 인스턴스)는 그룹 멤버에도 그대로 적용한다.

## 검토한 대안

- 계속 거절: 앱을 지우고 다시 등록해야 해 DB와 파일을 잃을 수 있다.
- `PeerEnv`를 그대로 두고 남는 두 환경을 `members` 둘로 그리기: 차트가 `members`에 `peer`를 요구하고, 짝 규칙을 쓰는 readiness, DB 경로, 초안, 보존 정리를 모두 바꿔야 한다. 남는 두 환경을 보통 짝으로 만드는 쪽이 지금 코드 경로를 그대로 쓴다.
- `peer_generation`을 새 `PeerEnv`로 넘기기: 남는 멤버의 백업 경로가 바뀌어 동작 중인 DB가 다른 경로에 WAL을 쓰기 시작한다. 환경별 세대로 바꾼다.
- 차트나 정책을 바꿔 replica의 source도 플랫폼이 고치기: 위 사실처럼 남는 replica는 빼는 멤버를 가리킬 수 없으므로 필요 없다. 정책 범위를 넓히지 않는다.
- 빼는 멤버가 이전에 쓰인 적이 있으면 계속 거절: 세 환경 시연 앱은 모든 멤버가 이전의 source나 target이었으므로 기능이 쓸모없어진다. 살아 있는 복제 상태 확인으로 바꾼다.

## 결과

- 인프라 변경 하나가 먼저 필요하다: infra ADR 0098(ApplicationSet jq와 두 멤버 `peerPresent: false` 허용). 관리 EKS의 hopcharlie-apps 차트 동기화 한 번이고 앱별 단계는 없다. 플랫폼 서비스 계정의 migration 역할과 세 멤버 source 변경 정책(infra ADR 0078)은 AWS, GCP, onprem 워크로드 클러스터 모두에 이미 있다. DB migration `0020`은 플랫폼이 시작할 때 적용한다.
- 콘솔은 세 환경 그룹의 빼기 버튼을 두 환경과 같은 조건(쓰기를 받는 환경, DB primary가 있는 환경, 배포 중)으로만 막는다. 새 오류 코드의 한국어, 일본어 문구를 더한다.
- 지금 있는 두 환경 짝과 세 환경 그룹의 렌더, 이전, 백업 경로는 바뀌지 않는다(테스트로 확인). 멤버를 뺀 앱만 두 멤버 레이블로 그려진다.

## 검증

- 2026-11-08 kind(Kubernetes 1.34, CNPG 1.30.1) 리허설: 네임스페이스 셋에 차트와 같은 모양의 Cluster 셋(공통 CA, `verify-full`, egress 대신 cluster-local ExternalName Service, infra의 workload 접근 정책과 VAP 적용)을 만들었다.
  - Q(처음 짝)를 뺐다. 쓰기 부하(0.05초 간격 INSERT) 중에 플랫폼 서비스 계정으로 P의 source를 Q에서 T로 바꿨고 정책이 허용했다. Q 네임스페이스를 지우고 P, T를 두 환경 값으로 바꿨다. 쓰기 404건 성공, 실패 0, P와 T 모두 404행, 빈 번호 없음, P 재시작 없음, T는 계속 streaming, P에 replication slot 없음.
  - source를 고치기 전에 P의 `externalClusters`에서 Q를 빼면 CNPG가 거절했다. replica의 source 변경은 정책이 거절했다.
  - 남은 짝에서 P에서 T로, 다시 T에서 P로 switchover했다(각 6초에서 7초). 행 수가 그대로였다(481행).
  - P가 primary일 때 Q를 세 번째로 다시 더했다(쓰기 353건 중). 세 멤버 모두 834행, 실패 0.
  - P에서 Q로 세 멤버 이전(source 변경 뒤 switchover)을 한 뒤, Q가 primary이고 T가 P를 거쳐 따라가는 상태에서 T를 뺐다(쓰기 174건, 실패 0, 1008행). 남은 짝에서 Q에서 P로 switchover했고, T를 세 번째로 다시 더했다(쓰기 180건, 세 멤버 모두 1188행).
  - 백업(Barman)과 Tailscale은 kind에 없다. 백업 경로는 단위 테스트로 확인한다.
- 같은 날 실제 Argo CD 리허설: kind에 argo-cd 차트 10.9.6을 설치하고, 멤버마다 Application을 하나씩 만들었다(차트 저장소는 클러스터 안 HTTP, 운영 ApplicationSet과 같은 `ignoreDifferences`, `RespectIgnoreDifferences=true`, automated prune/selfHeal, retry, client-side apply). Cluster 모양은 위와 같다.
  - 리뷰가 지적한 설계(기존 jq, 레이블 없는 두 환경 값)는 primary의 sync가 CNPG 거절로 계속 실패했다.
  - 고친 설계(새 jq, `members` 둘, 레이블 `"2"`): 세 번째로 다시 더한 뒤 peer를 뺐다. 플랫폼 SA로 primary의 source를 바꾸고, 빼는 Application을 지우고, 남는 두 Application의 값을 바꾸자 둘 다 손대지 않고 Synced, Succeeded, Healthy가 됐다. 레이블은 `"2"`, 목록은 두 항목, 빼는 환경의 egress Service는 prune됐다. 다음 재배포(값만 바꿈)도 Synced였다. 남은 짝에서 양방향 switchover 뒤에도 두 Application은 Synced였다.
  - 두 번의 빼기와 다시 더하기 동안 쓰기 3462건 성공, 실패 0, 두 멤버 모두 3462행이었다.
- 단위 테스트: `internal/api/group_removal_test.go`(source 변경, 변경 없음, 읽지 못하는 멤버, 거절 열 가지), `internal/deploy/group_removal_test.go`(peer 이동, 남는 멤버 경로 유지, 다시 더한 멤버의 `-1`, `-2`), store conformance(`RemoveDatabaseMember`, PostgreSQL과 메모리).
