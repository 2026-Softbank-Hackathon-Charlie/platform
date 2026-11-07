# 0042. 플랫폼 밖에서 compose로 돌던 앱을 조사하고, 계약 PR과 물리 복제로 무중단 가져온다

- 상태: 채택
- 날짜: 2026-11-07
- 관련 ADR: [0011](0011-github-app-deploy.md), [0016](0016-app-object-storage.md), [0019](0019-app-secrets-and-redeploy.md), [0020](0020-pg-major-version-gate.md), [0023](0023-push-token-for-imported-repos.md), [0024](0024-service-contract-workers-resources-db-env.md), [0028](0028-write-retry-contract.md), [0030](0030-app-db-topology.md), [0032](0032-registered-app-files.md), [0040](0040-gateway-durability-and-write-bounds.md), platform #111(환경 추가와 born-pairable, 머지 전), infra ADR 0036, 0048, 0064, migration-engine ADR 0009, 0012, 0013

## 상황

콘솔의 "기존 앱 가져오기"는 시안 화면(`frontend/src/pages/Onboard.tsx`)만 있고 API, 엔진, 차트 어디에도 구현이 없다.

여기서 기존 앱은 우리 계약 파일(`platform.yaml`)이 git에 없고, docker compose와 GitHub Actions로 VM 한 대에 직접 배포하던 앱을 말한다. 예를 들면 moimroom에서 `platform.yaml`과 `deploy/`를 빼고, `compose.yaml`을 ssh로 VM에 올리는 workflow만 남긴 레포다. 플랫폼은 AWS나 GCP에 실제로 떠 있는 상태와 GitHub 상태를 확인하고, 레포가 우리 계약을 쓰도록 PR을 만든 뒤, 서비스를 멈추지 않고 우리 클러스터로 옮겨야 한다.

2026-11-07에 확인한 사실은 다음과 같다.

### DB 복제 (kind, CNPG 1.30.1, 원본 `postgres:17.11-alpine` 컨테이너)

- CNPG distributed topology의 `replica.primary`에 CNPG가 아닌 외부 클러스터 이름(`legacy`)을 써도 받아들여진다. `bootstrap.pg_basebackup.source: legacy`로 태어나 streaming으로 따라간다.
- `externalClusters`에 자기 자신 항목이 없으면 webhook이 Cluster를 거부한다.
- `spec.replica.primary`를 자기 이름으로 바꾸면 promotionToken 없이 승격된다. 본 cutover 실행에서 패치 후 1.57초에 첫 쓰기가 커밋됐다(다른 실행에서는 `pg_is_in_recovery()`가 false가 되기까지 2.0초). 그 직후 CNPG가 `archive_mode` 변경 때문에 postgres를 한 번 재시작해서 약 0.4초 동안 연결이 끊겼다.
- 원본에서 `ALTER SYSTEM SET default_transaction_read_only = on`, reload, client backend 종료를 하고 `pg_current_wal_flush_lsn()`을 기록하면, 대상의 replay LSN이 412ms 안에 그 값에 정확히 도달했다. CHECKPOINT나 `pg_switch_wal()`은 필요 없다. 원본이 커밋한 id 3293개가 모두 대상에 있었고 sequence도 충돌 없이 이어졌다.
- 승격 전에 `ALTER SYSTEM RESET default_transaction_read_only`와 reload를 하면 원본이 바로 다시 쓰기를 받고, 대상은 계속 replica로 따라간다.
- 공식 이미지의 기본 hba(`host all all all scram-sha-256`)는 replication 연결에 맞지 않는다. 복제 계정을 만들고 PGDATA 안 hba 파일에 한 줄을 더한 뒤 `pg_reload_conf()`를 하면 된다. 재시작은 필요 없었다.
- 공식 이미지를 `POSTGRES_USER=app`으로 띄우면 `postgres` role이 없고, `app`이 superuser다. `postgres` role이 없으면 CNPG instance manager가 `role "postgres" does not exist`로 실패해서 대상이 Ready가 되지 않는다.
- role 변경(`CREATE ROLE`, `ALTER ROLE`)은 WAL로 대상에 복제된다. 원본을 `NOLOGIN`으로 막으면 승격 뒤 앱 계정도 막힌다. `ALTER SYSTEM`은 복제되지 않는다.
- bootstrap 시점에 원본의 `postgresql.auto.conf`가 대상으로 복사된다. CNPG는 ALTER SYSTEM을 막고, 이 값이 `spec.postgresql.parameters`보다 우선한다. 따라서 fence가 걸린 채로 bootstrap하면 대상은 read-only로 태어난다. `postgresql.conf`는 CNPG가 다시 만들므로 복사되어도 남지 않는다.
- `bootstrap.pg_basebackup.secret`(basic-auth)은 승격 시점에 앱 계정 비밀번호로 적용된다. CNPG는 replica로 태어난 클러스터에 `-app` Secret을 만들지 않는다.
- major가 다르면(이미지 18, 데이터 17) webhook과 pg_basebackup은 통과하고 인스턴스가 CrashLoopBackOff에 빠진다. CNPG는 경고하지 않는다.
- musl(alpine) 원본이 `en_US.utf8` 같은 libc locale을 쓰고 대상이 glibc이면, text btree 인덱스가 경고 없이 깨진다. equality 조회가 100건 대신 0건을 돌려줬고 `bt_index_check`가 실패했다. 대상에서 REINDEX하면 고쳐지지만 정렬 순서는 원본과 달라진다. glibc 릴리스가 같으면(Debian bookworm과 `-standard-bookworm`) 문제가 없었다. glibc 릴리스가 다르면 연결마다 collation version 경고가 난다.
- 외부 원본에서 태어난 Cluster(사용자 CA `certificates`, 짝 항목까지 세 개의 `externalClusters`)를 승격한 뒤 `replica.source`를 짝 이름으로 바꾸면, CNPG가 재시작 없이 받아들인다. `streaming_replica` role은 승격 전에는 없고(카탈로그가 원본 것이다) 승격 뒤 재시작 때 생긴다. `-replication`과 `-server` 인증서는 처음부터 사용자 CA로 발급되고 승격 때 다시 발급되지 않는다. 그 뒤 차트 방식의 짝(`pg_basebackup`, `verify-full`)이 이 Cluster에서 태어나 streaming했고, token 방식 전환을 양방향으로 해서 응답한 쓰기 3126건 중 잃은 것이 없었다(두 번째 kind 증명, 원본 `postgres:17.11-bookworm`). 강등 patch가 남은 `promotionToken`을 지우지 않으면 다음 전환이 webhook에서 거부된다. 엔진의 `SetPrimary`는 이미 `null`을 보낸다.
- CNPG 기본 태그 `17.11`은 Debian bullseye(glibc 2.31)다. `-standard-bookworm`은 2.36, `-standard-trixie`는 2.41이다. `minimal-*` 이미지에는 `en_US.utf8` locale이 없어서 그 locale을 쓰는 원본은 crash loop에 빠진다.

### 플랫폼, 엔진, infra (origin/main 2026-11-07)

- GitHub App `hopcharlie-platform`(installation 178050051)의 권한은 `contents:write`, `metadata:read`뿐이고 저장소 선택은 selected다.
- push 토큰 API(ADR 0023)는 `hopcharlie-imported` 토픽이 있는 레포에만, `contents:write`로만 토큰을 준다. hopai `prepare`는 CLI 전용이고 `.github/workflows` 수정을 막는다. LLM은 명령을 실행하지 못하고, push는 hopai 실행기 코드가 한다.
- ai-sandbox의 샌드박스는 CNPG만 만들 수 있고 Pod, Service, Job은 만들지 못한다.
- 게이트웨이는 백엔드 이름 `^[a-z0-9-]+$`와 http(s) URL만 검사하고, 요청의 Host를 백엔드 URL의 호스트로 바꾼다(`internal/gateway/gateway.go` `SetURL`). 앱은 `<app>.hopcharlie.dev` Host로만 고르고, ALB 인증서와 규칙도 `*.hopcharlie.dev`뿐이다. 배포(`gatewaySpec`)는 앱 환경이 아닌 백엔드도 지우지 않고 쓰기 백엔드도 바꾸지 않지만, 백엔드는 이름과 URL 쌍(`map[string]string`)만 옮긴다. `register()`는 응답하지 않는 환경을 백엔드에 더하지 않는다. 쓰기 보류 `MaxHold` 기본값은 55초다.
- 엔진의 소스와 대상은 모두 CNPG Endpoint다(`pkg/plan`이 namespace와 cluster를 요구한다). `engine.Options.SourceCluster`가 있지만 platform `runs`는 넘기지 않는다. 사전 확인의 `checkTopology`, 강등 토큰 대기, 롤백의 `waitRecovery`, cold 모드의 `rollbackPromoted`는 원본이 CNPG라고 가정한다.
- 엔진의 cold 모드(엔진 ADR 0012)는 대상 Deployment를 kube API로 0으로 줄였다가 승격 뒤에 다시 늘린다. raw manifest로만 시험했고 Argo 아래에서는 시험하지 않았다. platform Plan 정책은 `target.app`이 있는 Plan을 아직 받지 않는다(`internal/plans/policy.go`).
- 배포 renderer는 replicas를 `max(replicas, 1)`로 쓰고(`internal/deploy/gitops.go`), 차트는 `default 1`로 0을 1로 바꾼다. ApplicationSet은 `selfHeal: true`이고 Deployment `/spec/replicas`를 무시하지 않는다.
- 엔진 fence gate의 해시(`pkg/verify`)는 PK나 `t::text` 순서로 정렬하고 COLLATE를 지정하지 않는다. 전환 뒤 검증은 옛 primary가 새 primary를 따라간다고 가정한다.
- platform의 Plan 예약(`plans.Reserve`)은 레지던시, 버전, 용량, 동등성 게이트를 돌리고, run 시작은 retention을 위해 원본 CNPG Cluster를 읽는다.
- 플랫폼 역할 `hopcharlie-platform-pod`에는 EC2 인스턴스 조회, SSM 명령 실행, 인스턴스 중지 권한이 없다. GCP SA에는 compute 조회 권한이 없다.
- 관리 클러스터의 API와 게이트웨이 Pod는 TCP 80, 443, 5432로만 밖에 나간다. 워크로드 앱 네임스페이스는 TCP 443과 같은 네임스페이스만 허용하고, CNPG Pod는 Tailscale egress 프록시의 5432로만 상대 DB에 닿는다.
- tailnet 정책은 사람이 손으로 고친다. 플랫폼이 쓸 Tailscale 자격 증명은 없다. 지금 grant는 `tag:hop-db-client-*`에서 `tag:hop-db-*`로 가는 tcp:5432뿐이다.
- 관리 클러스터 route policy(infra `management-app-routes.yaml`)는 이름 `^db-(eks|gke)-`, tailnet FQDN `hop-` + 이름, hostname `hop-db-management-to-(eks|gke)-`만 받는다.
- `*.hopcharlie.dev`는 와일드카드로 게이트웨이를 가리키고, 개별 레코드가 있으면 그 레코드가 이긴다. infra ADR 0036은 플랫폼에 Cloudflare 토큰을 주지 않기로 했다.
- docker는 IPAM 서브넷을 직접 정하지 않은 네트워크(compose 기본 네트워크)에서 `--ip`를 거부한다.
- 차트는 DB 이름과 owner를 `app`으로 고정해서 그린다.
- 앱 차트는 `imageName`을 쓰지 않아서 워크로드 operator 기본값(EKS `postgresql:18.6-minimal-trixie`)을 쓴다. DB 토폴로지는 두 앱 환경 사이의 CNPG만 그리고, 인증은 플랫폼 CA 인증서(`verify-full`)만 쓴다. DB PVC는 `topologyStorageSize`(기본 1Gi)다. ApplicationSet은 Cluster의 `replica`, `externalClusters`, `certificates`를 무시하므로 이 필드는 만들 때만 적용된다.
- 워크로드 admission policy는 플랫폼이 `spec.replica.primary`와 `promotionToken`만 바꾸게 한다.
- moimroom의 compose는 파일을 compose 안의 MinIO(`http://minio:9000`)에 두고 앱을 8080으로 연다.

## 결정

### 범위

- 대상은 VM 한 대에서 docker compose로 도는 앱이다. DB는 compose 안의 PostgreSQL 컨테이너이고, 파일은 HTTPS로 닿는 외부 S3 호환 저장소(AWS S3, GCS의 S3 API 등)에 둔다. 앱은 VM에서 HTTPS(443)로 응답한다.
- 앱의 공개 주소는 `<label>.hopcharlie.dev`이고, 앱 이름은 그 label이다. 게이트웨이와 ALB가 이 형태만 받기 때문이다.
- 이번 범위가 아닌 것은 리포트에 차단 사유로 남기고 가져오지 않는다.
  - RDS와 Cloud SQL. 물리 복제를 허용하지 않는다. 논리 복제는 후속 ADR로 다룬다.
  - compose 안의 MinIO처럼 VM 안에만 있는 파일 저장소. tailnet은 5432만 열려 있고 API Pod는 443으로만 나간다. 후속 ADR로 다룬다.
  - DB와 파일 저장소가 아닌 이름 있는 볼륨에 데이터가 있는 앱(업로드 디렉터리 등).
  - HTTPS로 응답하지 않는 앱, `*.hopcharlie.dev` 밖의 도메인.
- 클라우드는 AWS EC2부터 끝까지 만든다. GCP GCE는 위치 조사(클라우드 API)까지만 같은 단계에 만들고, 리포트에 "GCP VM 내부 조사는 다음 단계"로 남긴다. VM 내부 조사와 연결은 IAP와 OS Login 경로로 다음 단계에 붙이며, 같은 고정 스크립트를 실행 수단만 바꿔 쓴다.
- 시연용 기존 앱은 moimroom 사본이다. 파일은 AWS S3 버킷에 두고, compose에 TLS 리버스 프록시를 더해 `moimroom-legacy.hopcharlie.dev`(VM을 가리키는 개별 A 레코드)로 연다. DB 이미지는 moimroom 그대로 `postgres:17.11-alpine`이라 REINDEX 경로를 탄다.

### 흐름과 상태

`POST /v1/onboardings`(operator)가 온보딩 하나를 만들고, 플랫폼 job이 아래 상태를 차례로 진행한다. 상태와 결과는 `onboardings` 테이블에 남기고, 서버가 재시작하면 마지막 상태부터 이어서 진행한다. 원본 VM과 원본 DB를 바꾸는 일은 operator가 승인한 뒤(`connecting`)에만 한다.

| 상태 | 하는 일 | 다음으로 가는 조건 |
| --- | --- | --- |
| `discovering` | GitHub, DNS, 클라우드, VM, DB를 읽어 Facts를 만든다. 읽기만 한다 | 차단 사유가 없다 |
| `patching` | hopai가 계약 브랜치를 만들고, 플랫폼이 PR을 연다. 레포에 이미 맞는 `platform.yaml`이 있으면 건너뛴다 | PR이 열린다 |
| `checking` | PR 브랜치를 임시 앱으로 배포해 시험하고 앱 시작 시간을 잰다. 실패하면 hopai가 다시 고친다(최대 2회) | 임시 배포가 healthy이고 보류 예산(5번) 안이다 |
| `awaiting_approval` | 콘솔에서 리포트, PR diff, 시험 결과, 원본에 더할 변경 목록, 원본 배포가 멈춘다는 안내를 보여 준다 | operator가 PR head 커밋으로 승인한다 |
| `connecting` | 원본 배포 workflow 끄기, 원본 DB 준비, tailnet 연결 | 관리 클러스터에서 원본 DB에 replication 연결이 된다 |
| `registering` | PR 머지, 앱 등록(원본의 replica DB, 앱 0 replica), 게이트웨이 백엔드 등록 | Cluster가 streaming이고 Argo가 synced다 |
| `syncing` | DB streaming과 파일 복사가 따라오는지 본다. 원래 주소의 DNS가 게이트웨이로 바뀌었는지 본다 | 복제 지연, 파일 차이, DNS가 기준 안이다 |
| `ready` | 콘솔의 "전환" 버튼을 켠다 | operator가 전환을 시작한다 |
| `cutting_over` | 엔진 run(외부 원본 모드, cold) | run이 성공한다 |
| `decommissioning` | legacy 백엔드를 빼고 원본을 멈춘다 | operator가 중지를 확인한다 |
| `done` | 앱 상세의 "환경 추가"(platform #111)로 이어진다 | |

`connecting` 뒤에 실패하면 원본 배포 workflow는 꺼진 채로 남고, `resume`이나 `DELETE`가 처리한다. 승인 화면과 상태 화면은 원본 배포가 멈춰 있다는 것을 계속 보여 준다.

API는 다음과 같다. 모두 OpenAPI 계약에 넣고, 콘솔과 `charlie onboard`가 같은 API를 부른다.

| API | 역할 | 하는 일 |
| --- | --- | --- |
| `POST /v1/onboardings` | operator | 시작. 입력은 레포(`owner/name`)와 서비스 주소(`https://<label>.hopcharlie.dev`), 대상 환경(선택) |
| `GET /v1/onboardings`, `GET /v1/onboardings/{id}` | viewer | 목록, 상태, Facts 요약, 리포트, PR, 시험 결과, 원본에 더한 것, 사람이 할 일(App 설치 링크, 지울 DNS 레코드) |
| `GET /v1/onboardings/{id}/events` | viewer | 단계 기록(SSE) |
| `POST /v1/onboardings/{id}/approve` | operator | 본문 `{headSha}`. PR head가 다르면 409 |
| `POST /v1/onboardings/{id}/cutover` | operator | `ready`에서만. 엔진 run을 시작하고 run id를 남긴다 |
| `POST /v1/onboardings/{id}/decommission` | operator | 원본 인스턴스 중지를 확인한다 |
| `POST /v1/onboardings/{id}/resume` | operator | `failed`나 사람을 기다리는 상태(App 설치, DNS)에서 다시 확인하고 이어서 한다 |
| `DELETE /v1/onboardings/{id}` | operator | 아래 |

`DELETE`는 상태에 따라 다르다. 실행 중인 hopai Job과 임시 앱은 항상 먼저 멈추고 지운다.

- `awaiting_approval`까지: PR을 닫고 행을 지운다.
- `connecting`부터 `ready`까지: 원본에 더한 것을 행에 남긴 순서의 반대로 되돌리고(Tailscale 컨테이너, 상태 볼륨, 장치, hba 줄, `wal_keep_size` 줄, 복제 계정, 새로 만든 `postgres` role, 인스턴스 태그, 관리 클러스터의 `db-legacy-<app>`, workflow 다시 켜기), 게이트웨이 앱과 등록한 앱을 지운다. 머지한 PR은 되돌리지 않는다. 더한 파일은 원본 배포에 영향이 없고, 다시 온보딩하면 `patching`이 그 파일을 확인하고 건너뛴다.
- `cutting_over`부터: 지울 수 없다.

대상 환경을 고르지 않으면 원본 VM과 같은 클라우드의 환경(AWS는 `eks`, GCP는 `gke`)을 쓴다.

### 1. 조사 (`discovering`)

조사는 플랫폼 코드가 하고, 결과는 Facts JSON 하나로 남긴다. 비밀값은 Facts에 넣지 않는다.

| 대상 | 방법 | 얻는 것 |
| --- | --- | --- |
| GitHub | App 설치 토큰. 레포에 App이 설치되어 있지 않으면 설치 링크를 보여 주고 기다린다(`resume`으로 다시 확인) | 기본 브랜치와 HEAD, `compose*.y*ml`, Dockerfile, `.github/workflows/*`, 최근 workflow 실행(마지막 성공 커밋), VM에 배포하는 workflow 목록 |
| DNS와 HTTPS | 서비스 주소 해석, TLS 접속 | 공인 IP, 인증서 이름, TTL, 헬스 체크 후보 |
| AWS | `DescribeAddresses`, `DescribeNetworkInterfaces`, `DescribeInstances` | 인스턴스, 리전, 태그, ASG와 EKS 소속, SSM 관리 여부 |
| GCP | `instances.aggregatedList`의 natIP | 인스턴스, zone |
| VM 내부 | SSM Run Command로 문서 `HopCharlie-Inspect`만 실행한다 | compose 프로젝트와 네트워크와 서브넷, 컨테이너, 이미지와 revision 라벨, 포트, 볼륨과 크기, 여유 디스크, 환경 변수 이름 |
| DB | 같은 문서가 DB 컨테이너 안에서 `psql`로 읽기 쿼리를 돌린다 | 아래 표 |

환경 변수 값은 평문으로 SSM 출력에 싣지 않는다. 플랫폼이 조사마다 일회용 RSA 키 쌍을 만들고 공개키를 문서 매개변수로 넘긴다. 문서는 VM 안에서 digest를 고정한 openssl 컨테이너로 값을 암호화하고(무작위 AES-256 키를 RSA-OAEP로 감싼다), 암호문만 stdout에 쓴다. 24,000자를 넘으면 여러 번 나눠 받는다. VM에 따로 설치할 도구나 IAM은 없다. 플랫폼은 값을 풀어 ADR 0019 방식으로 다시 암호화해서 온보딩 행에 두고, 개인키는 버린다. Facts와 AI에는 이름, 출처 컨테이너, 비밀값 여부만 넘긴다.

DB에서 읽는 것과 판정은 다음과 같다.

| 읽는 것 | 판정 |
| --- | --- |
| `server_version_num` | CNPG 이미지 major를 같게 고른다(ADR 0020) |
| `version()`의 `linux-musl`, `pgdg11/12/13` | 원본 libc를 판별한다 |
| `pg_database`의 `datlocprovider`, `datcollate`, `datctype`, `datcollversion` | 이미지 선택과 REINDEX 대상을 정한다(아래) |
| `lc_monetary`, `lc_numeric`, `lc_time`, `lc_messages` | C 계열이 아니면 `minimal-*`을 쓰지 않는다 |
| `pg_settings`에서 `sourcefile`이 `postgresql.auto.conf`인 값 | 하나라도 있으면 차단한다. bootstrap 때 대상으로 복사되어 되돌릴 수 없다 |
| `pg_db_role_setting`의 `default_transaction_read_only` | 있으면 차단한다. fence를 덮어쓴다 |
| `pg_settings`의 `wal_keep_size` 출처 | `command line`이면(compose의 `-c`) 차단한다. `postgresql.conf`로 바꿀 수 없다 |
| superuser인 role, `postgres` role 존재 | fence 계정을 정한다. `postgres` role이 없으면 `connecting`에서 만든다 |
| `POSTGRES_DB`, `POSTGRES_USER`와 실제 DB, owner | 차트에 DB 이름과 owner로 넘긴다 |
| `pg_database_size`, WAL 생성량, 연결 수 | 대상 PVC 크기, 동기화 시간 추정, 리포트 |

이미지는 다음 규칙으로 고르고, 고른 이미지를 digest까지 `app_databases` 행에 둔다.

- glibc 원본은 glibc 릴리스가 같은 `-standard-<debian>` 이미지를 쓴다(bullseye는 기본 태그, bookworm, trixie).
- musl 원본이고 locale이 `C`, `POSIX`이거나 builtin provider이면 `-standard-bookworm`을 쓴다.
- musl 원본이고 그 밖의 libc locale이면 `-standard-bookworm`을 쓰고, REINDEX 대상을 정한다. `pg_index.indcollation`이 C 계열이 아닌 인덱스가 대상이다. collation oid 100(`default`)은 DB의 collation으로 본다. text 컬럼의 range 파티션 키나 CHECK 제약이 C가 아닌 collation에 기대면 REINDEX로 고칠 수 없으므로 차단한다.
- ICU provider는 ICU 버전이 같은 이미지가 없으면 차단한다.

대상 PVC 크기는 `pg_database_size`의 두 배와 1Gi 중 큰 값으로 정해 gitops 값에 쓴다. 원본 VM의 여유 디스크가 2GiB보다 작으면 `wal_keep_size`를 둘 수 없으므로 차단한다.

드리프트도 본다. 실행 중인 이미지의 revision 라벨이나 마지막으로 성공한 배포 커밋이 기본 브랜치 HEAD와 다르면, 리포트 맨 위에 경고를 둔다. 계약 파일은 git 기준으로 만들기 때문이다. 경고는 진행을 막지 않고 operator가 승인 화면에서 확인한다.

차단 사유를 한곳에 모으면 다음과 같다. DB가 PostgreSQL 컨테이너가 아니다. VM이 SSM 관리 대상이 아니다. VM이 ASG나 EKS 노드다. 범위 밖 저장소, 볼륨, 도메인을 쓴다. 앱이 HTTPS로 응답하지 않는다. 이미지를 고를 수 없다. 위 표의 설정 조건에 걸린다. superuser를 찾지 못했다. 디스크가 모자란다. 쓰기 보류 예산을 넘는다(5번).

### 2. 계약 PR (`patching`, `checking`)

- 플랫폼이 hopai Job을 새 종류 `onboard`로 띄운다(`internal/airun`). hopai는 Facts와 기본 브랜치 클론을 받아 브랜치 `hopcharlie/onboard`에 커밋하고 push한다. push 토큰은 ADR 0023과 같은 `contents:write` 하나이고, 온보딩 행이 `patching`이나 `checking`인 레포에만 준다. 토픽 검사는 가져온 사본용으로 그대로 두고, 온보딩은 행으로 허가한다. hopai 실행기는 그 브랜치 말고는 push하지 않는다.
- PR은 플랫폼이 App 토큰으로 연다. PR 본문에는 리포트 요약을 넣는다. hopai에는 PR 권한을 주지 않는다.
- PR은 원본 배포에 영향을 주지 않는 새 파일만 더한다(`platform.yaml`, `hopcharlie.Dockerfile` 등). compose 파일, compose가 쓰는 Dockerfile, `.github/` 아래, 그 밖의 기존 파일은 고치지 않는다. 앱 코드를 고쳐야만 계약을 맞출 수 있으면 PR을 만들지 않고 차단 사유로 남긴다.
- `platform.yaml`은 compose에서 옮긴다. 공개 서비스와 포트, 헬스 체크, `postgres`와 `s3` 리소스, 비밀값 이름(`secrets`), 일반 값(`env`), 메모리(ADR 0024)를 쓴다. DB와 저장소 연결 값은 플랫폼 이름(`DATABASE_URL`, `PG*`, `S3_*`)을 쓰고, 앱이 다른 이름을 읽으면 `env`에서 `$(...)`로 넘긴다.
- 시험은 PR 브랜치를 임시 앱 `<app>-onb-<짧은 id>`로 대상 환경 하나에 등록해서 배포한다. 이 앱은 DB 짝 행을 만들지 않고(독립 primary), 게이트웨이에 등록하지 않으며(배포 옵션 `gateway: false`), 비밀값에는 시험용 값을 넣는다. healthy는 Argo의 sync와 health로 본다. Deployment를 만든 때부터 Ready가 될 때까지의 시간을 앱 시작 시간으로 잰다. 끝나면 앱을 지운다. 실패하면 배포 로그를 hopai에 넘겨 다시 고치게 하고, 두 번 실패하면 `failed`로 멈춘다.

### 3. 승인과 원본 연결 (`awaiting_approval`, `connecting`)

승인은 콘솔에서 operator가 한다. Free 플랜이라 브랜치 보호를 쓸 수 없기 때문이다. 승인 화면에는 Facts 요약, 차단 사유와 경고, PR diff, 시험 결과, 원본에 더할 변경 목록, 원본 배포가 정리까지 멈춘다는 안내를 보여 준다. 승인은 화면에 보이는 PR head 커밋으로 보낸다. 그 사이에 PR이 바뀌었거나 기본 브랜치가 움직여 PR이 최신이 아니면, 브랜치를 다시 만들고 시험부터 다시 한다.

승인하면 `connecting`에서 아래 일을 이 순서로 한다. 한 일은 하나씩 온보딩 행에 남겨 `DELETE`가 되돌릴 수 있게 한다.

1. VM에 배포하는 workflow를 모두 GitHub API로 disable하고, 진행 중인 실행이 있으면 끝날 때까지 기다린다(10분이 넘으면 취소한다). PR을 머지해도 원본이 다시 배포되지 않게 하고, 원본 workflow의 `docker compose down`이 아래 컨테이너 때문에 실패하지 않게 하려는 것이다.
2. 인스턴스에 태그 `hopcharlie-onboarding=<onboarding id>`를 붙인다. 조사에서 찾은 그 인스턴스에만 붙인다.
3. 플랫폼이 Tailscale OAuth 클라이언트로 인증 키를 발급한다. 일회용, preauthorized, 태그 `tag:hop-legacy`, 만료 1시간이다. 이 키는 문서 매개변수로 넘기므로 SSM 명령 기록에 남는다. 일회용이고 1시간 뒤 만료되므로 받아들인다.
4. SSM 문서 `HopCharlie-Connect`를 실행한다. 문서는 리뷰한 고정 스크립트이고, 매개변수는 검증된 값(컨테이너 이름, compose 네트워크, 인증 키, 일회용 공개키)만 받는다.
   - DB 컨테이너 안에서 복제 계정 `hopcharlie_replicator`(LOGIN REPLICATION)를 VM에서 만든 무작위 비밀번호로 만든다. 비밀번호는 일회용 공개키로 암호화해 stdout으로 돌려준다(1번 조사와 같은 방식). `postgres` role이 없으면 `SUPERUSER LOGIN`(비밀번호 없음)으로 만든다. 비밀번호가 없으므로 scram 연결로는 로그인할 수 없다.
   - compose 네트워크에 Tailscale 컨테이너 `hopcharlie-ts-<app>`을 띄운다. 이미지 digest는 고정하고, userspace 모드, `--restart unless-stopped`, 상태 디렉터리(`TS_STATE_DIR`)는 이름 있는 볼륨 `hopcharlie-ts-<app>-state`다. 호스트 이름은 `hop-db-legacy-<app>`, 태그는 `tag:hop-legacy`다. `TS_SERVE_CONFIG`의 `TCPForward`로 tailnet의 TCP 5432를 DB 컨테이너 서비스 이름의 5432로 넘긴다. compose 프로젝트 라벨이 없으므로 원본의 `docker compose up`이 지우지 않는다.
   - PGDATA 안 hba 파일 끝에 `host replication hopcharlie_replicator <compose 네트워크 서브넷> scram-sha-256`을 더한다. 서브넷은 `docker network inspect`로 읽는다. 전용 계정과 scram이 접속을 지킨다. loopback 주소는 쓰지 않는다. 공식 이미지의 `host all all 127.0.0.1/32 trust`에 걸려 비밀번호 없이 들어오기 때문이다.
   - `postgresql.conf` 끝에 `wal_keep_size = '1GB'`를 더한다. hba와 함께 `pg_reload_conf()`로 적용하고 재시작하지 않는다. `postgresql.conf`는 CNPG가 다시 만들므로 대상에 남지 않는다.
5. 관리 클러스터에 egress Service `db-legacy-<app>`(tailnet FQDN `hop-db-legacy-<app>`)을 만든다(`internal/dbroute`, infra ADR 0064와 같은 방식).
6. 이 경로로 superuser 접속과 replication 연결(`IDENTIFY_SYSTEM`)이 되는지 확인한다.

fence는 조사에서 찾은 superuser(공식 이미지에서는 `POSTGRES_USER`)가 한다. 복제 계정은 `ALTER SYSTEM`과 superuser 연결 종료를 할 수 없기 때문이다. 이 계정의 비밀번호는 온보딩 행에만 두고, run을 시작할 때 엔진 프로세스 환경으로만 넘긴다.

### 4. 등록과 동기화 (`registering`, `syncing`)

- 플랫폼이 PR을 squash 머지한다.
- 앱 등록은 온보딩 job이 store를 직접 호출해서 한 트랜잭션으로 한다. 앱 행, `app_databases` 행, 비밀값이 같이 생기고, 첫 배포는 그 뒤에 큐에 들어간다. 게이트웨이 앱 등록은 트랜잭션 밖에서 바로 이어서 하며, 다시 해도 결과가 같다.
- `app_databases`에 아래 열을 더한다. 앱 계정 비밀번호는 원본 값이다. CA는 ADR 0030처럼 새로 만들고, 나중에 환경을 추가할 때 이 CA로 짝을 만든다.
  - `source_kind`: `peer`(지금과 같다) 또는 `external`
  - `image`: 고른 CNPG 이미지(digest 포함). 짝 환경을 포함한 이 앱의 모든 DB에 같은 이미지를 그린다. 비어 있으면 지금처럼 operator 기본값이다.
  - `database`, `owner`: DB 이름과 owner. 비어 있으면 `app`이다.
  - 외부 원본 정보: tailnet 호스트, 복제 계정 비밀번호 암호문, REINDEX 대상 목록
- 비밀값은 조사에서 얻은 값 가운데 다음을 빼고 ADR 0019로 봉인해 첫 배포부터 넣는다. 빼는 것은 플랫폼이 넣는 이름(`DATABASE_URL`, `PG*`, `S3_*`, ADR 0024의 예약 이름)과 값에 compose 서비스 이름이 들어 있는 것이다. 원본 저장소 키는 앱 비밀값에 넣지 않고 아래 rclone 원격에만 쓴다.
- 대상 앱은 전환 전까지 0 replica로 둔다. 일반 앱은 read-only replica DB 위에서 healthy가 될 수 없기 때문이다. 이를 위해 다음을 바꾼다. 첫 배포의 성공 조건은 Cluster streaming과 Argo synced다.
  - 배포 renderer와 차트가 replicas 0을 그대로 쓴다(`max(..., 1)`과 `default 1`을 고친다).
  - 온보딩 중인 앱의 Deployment에는 라벨 `hopcharlie.dev/onboarding-cold: "true"`를 그리고, ApplicationSet은 이 라벨이 있는 Deployment의 `/spec/replicas`를 무시한다. 엔진 cold 모드가 kube API로 늘린 값을 Argo가 되돌리지 않게 하려는 것이다.
  - run이 성공하면 플랫폼이 gitops 값에 원래 replicas를 쓰고 라벨을 뺀다.
  - Plan 정책은 온보딩 Plan에 한해 `target.app`(cold)을 받는다.
- 게이트웨이 앱 백엔드는 `legacy`와 대상 환경 두 개이고, 쓰기와 읽기는 모두 `legacy`로 보낸다. 대상은 0 replica라 `register()`가 더하지 않으므로, 온보딩 job이 두 백엔드를 직접 넣는다.
  - `legacy` 백엔드는 원래 이름(`https://<label>.hopcharlie.dev`, TLS SNI와 Host)으로 요청하되 VM의 공인 IP로 접속한다. 이를 위해 게이트웨이 앱 spec에 백엔드별 접속 주소 맵(`dial`)을 더한다. `dial`은 AppSpec과 AppInfo, 배포의 `gatewaySpec`, 상태 파일, 백엔드 재사용 비교에 모두 들어가고, 프록시와 헬스 체크 둘 다 백엔드별 `DialContext`로 그 주소에 접속한다. 그래서 원래 주소의 DNS가 게이트웨이로 바뀐 뒤에도 게이트웨이가 자기 자신으로 돌아오지 않는다.
- 차트(infra 쪽 ADR)는 `dbTopology.external`이 있으면 대상 Cluster를 다음과 같이 그린다. kind에서 동작한 manifest와 같다. 여기서 `<cluster>`는 `<app>-<postgres 리소스 이름>`이다.
  - `imageName`은 `app_databases.image`, PVC 크기는 1번에서 정한 값, DB 이름과 owner는 `app_databases` 값이다.
  - `bootstrap.pg_basebackup: {source: legacy, database, owner, secret: <cluster>-app}`
  - `replica: {primary: legacy, self: <cluster>-<env>, source: legacy}`
  - `externalClusters`는 세 개다. `legacy`(원본 egress Service, `hopcharlie_replicator`, 비밀번호 Secret, `sslmode: prefer`), 자기 자신, 짝 환경이다. 짝 환경 항목은 지금 차트의 일반 짝 항목과 같이 이름 `<cluster>-<peer>`, 호스트 `<cluster>-to-<peer>`, `streaming_replica`, `verify-full`, 플랫폼 CA 인증서로 태어날 때부터 그린다. ApplicationSet이 이 필드를 나중에 다시 적용하지 않기 때문이다. 짝으로 가는 tailnet 객체는 platform #111의 `peerPresent: false`처럼 그리지 않는다.
  - `certificates`는 ADR 0030과 같이 플랫폼 CA를 쓴다.
  - 원본으로 가는 egress Service와 replication NetworkPolicy를 그린다.
  - ApplicationSet은 Cluster의 `/spec/bootstrap`도 무시한다. `replica`처럼 만들 때만 의미가 있는 필드이고, 전환 뒤 차트가 그리는 값이 바뀌기 때문이다(5번).
- 원본 DB 호스트(`db-legacy-<app>`)는 온보딩 행에서 그 앱의 `AppDBHosts`에 더한다. Plan 정책이 DSN 호스트로 앱 소유와 게이트웨이 범위를 정하기 때문이다.
- 파일은 조사에서 얻은 앱의 저장소 주소와 키로 rclone 원격 `legacy`를 만든다. 이 원격은 그 온보딩의 job과 run 프로세스 환경에만 넣는다. `syncing` 동안 `legacy:<bucket>`에서 앱 버킷(ADR 0032)으로 반복 복사한다. 허용 버킷 목록에는 온보딩 행의 버킷을 더한다.
- `syncing`은 다음을 모두 만족하면 `ready`로 넘어간다.
  - 대상이 streaming이고 복제 지연이 2초 안이다.
  - 마지막 파일 복사에서 남은 차이가 100개 이하다. 나머지는 쓰기 보류 안의 마지막 복사가 맡는다.
  - 원래 주소가 게이트웨이를 가리킨다. 원래 주소에는 VM을 가리키는 개별 레코드가 있으므로, 도메인 소유자가 그 레코드를 지워야 와일드카드가 게이트웨이로 보낸다. 콘솔은 지울 레코드를 보여 주고 DNS 조회로 확인한다. 플랫폼은 DNS 자격 증명을 갖지 않는다(infra ADR 0036). 확인 뒤 옛 TTL만큼 더 기다린다. 사용자가 VM으로 직접 들어오는 동안 전환하면 그 요청이 read-only DB를 만나기 때문에 이 조건을 둔다.

### 5. 전환 (`cutting_over`)

플랫폼은 온보딩 전용 Plan을 만든다. 소스는 `kind: external`, 이름 `legacy`, DSN은 관리 클러스터의 `db-legacy-<app>`과 superuser 계정이다. 대상 앱은 cold 모드다. 게이트웨이 `sourceBackend`는 `legacy`다.

쓰기 보류 예산은 게이트웨이 `MaxHold`(55초)에서 여유 15초를 뺀 40초다. 보류 안에서 하는 일(파일 마지막 복사, replay 대기, fence gate, 승격, REINDEX, 앱 시작)의 예상 시간 합이 이 예산을 넘으면 `checking`에서 차단한다. 앱 시작은 `checking`에서 잰 값이고, REINDEX와 fence gate는 인덱스와 테이블 크기를 보수적인 처리 속도로 나눠 추정한다.

platform의 게이트는 외부 원본일 때 다음과 같이 다룬다.

| 게이트 | 처리 |
| --- | --- |
| 버전(ADR 0020) | Facts의 `server_version_num`과 `app_databases.image`의 major를 비교한다 |
| 용량 | 대상 PVC 크기와 `pg_database_size`를 비교한다 |
| 레지던시 | 온보딩 행의 원본 버킷을 허용 목록에 더한 뒤 그대로 돌린다 |
| 동등성 | 원본 환경이 없으므로 돌리지 않고, 결과에 "외부 원본이라 생략"으로 남긴다 |
| retention | 원본 CNPG가 없으므로 돌리지 않는다 |

엔진(migration-engine 쪽 ADR)은 `source.kind: external`일 때 별도 switchover 경로를 쓴다. 기존 CNPG 경로는 바꾸지 않는다.

1. 사전 확인: 원본이 recovery가 아니고 `default_transaction_read_only`가 off다. 대상이 `replica.primary: legacy`이고 streaming 중이다. 복제 지연이 기준 안이다. `checkTopology`와 강등 토큰 대기는 이 경로에서 쓰지 않는다.
2. 게이트웨이가 쓰기를 보류한다(기존과 같다).
3. 원본 fence: `ALTER SYSTEM SET default_transaction_read_only = on`, `pg_reload_conf()`, 200ms 대기, 다른 client backend 종료, `pg_current_wal_flush_lsn()` 기록. role은 바꾸지 않는다.
4. 파일 마지막 복사.
5. 대상의 replay LSN이 기록한 LSN 이상이 될 때까지 기다린다.
6. fence gate를 돌린다. 해시 쿼리는 collation이 있는 컬럼만 `COLLATE "C"`로 정렬해서, 원본과 대상의 정렬 규칙 차이와 대상의 깨진 text 인덱스에 영향을 받지 않게 한다.
7. 대상의 `replica.primary`를 자기 이름으로 바꾼다. promotionToken은 쓰지 않는다. 대상이 쓰기를 받을 때까지 기다린다.
8. REINDEX 대상 목록이 있으면 대상에서 `REINDEX INDEX`를 차례로 실행한다.
9. 대상 앱을 띄우고(cold), 새 primary가 안정될 때까지 기다린 뒤(엔진 ADR 0009, 승격 직후 재시작 포함), 게이트웨이 쓰기와 읽기를 대상으로 보내고 보류를 푼다.
10. 검증은 6번의 fence gate를 기준으로 삼는다. 원본은 승격 뒤 따라오지 않으므로 전환 뒤 DB 비교는 하지 않고, 파일만 원본에서 대상 방향으로 비교한다.

6번까지 실패하면 원본 fence를 되돌리고(`ALTER SYSTEM RESET default_transaction_read_only`, reload) 보류한 쓰기를 원본으로 보낸다. 7번 이후에 실패하면 원본으로 되돌리지 않는다. 원본은 read-only로 남고, run은 엔진 ADR 0013의 halt로 멈춘다. 그동안 보류는 유지되므로 쓰기는 `MaxHold`가 지나면 503으로 실패하고 데이터는 잃지 않는다. operator가 콘솔에서 resume하면 멈춘 단계부터 대상에서 이어서 한다. cold 모드의 `rollbackPromoted`(대상 재강등)는 이 경로에서 쓰지 않고, 콘솔은 온보딩 run의 롤백 버튼을 숨긴다.

run이 성공하면 플랫폼이 아래 일을 한다.

- 대상의 `replica.source`를 짝 환경 이름(`<cluster>-<peer>`)으로 바꾼다. admission policy는 값이 `legacy`일 때 짝 이름으로 바꾸는 것만 허용한다.
- `app_databases.source_kind`를 `peer`로 바꾸고, 대상 환경을 primary로 하는 짝 없는 상태(`peerPresent: false`)와 원래 replicas로 다시 배포한다. 이때 차트는 원본 egress Service와 replication NetworkPolicy를 더 그리지 않는다. 차트가 그리는 `bootstrap`은 `initdb`로 바뀌지만 ApplicationSet이 무시하므로 Cluster는 그대로다. `legacy` 비밀번호 Secret은 `externalClusters`의 `legacy` 항목이 계속 가리키므로, 외부 원본 정보가 행에 있는 동안 차트가 계속 그린다.
- 새 primary에서 `hopcharlie_replicator`를 지운다. 앱 계정이 superuser인 것은 리포트에 권고로만 남긴다. 앱이 마이그레이션에 쓸 수 있기 때문이다.

### 6. 정리 (`decommissioning`)

1. 4번의 DNS 확인과 TTL 대기가 이미 끝났으므로, 게이트웨이에서 `legacy` 백엔드를 뺀다.
2. Tailscale 장치를 지우고 관리 클러스터의 `db-legacy-<app>`을 지운다.
3. operator가 콘솔에서 확인하면, 태그가 붙은 인스턴스를 중지한다. 지우지는 않는다. 디스크와 원본 버킷은 operator가 확인한 뒤 클라우드에서 지운다.
4. 원본 workflow는 disable한 채로 둔다. 리포트에 정리한 것과 남은 것을 적는다.

### 권한

| 주체 | 받는 것 | 받지 않는 것 |
| --- | --- | --- |
| 플랫폼(AWS) | `ec2:DescribeInstances`, `DescribeAddresses`, `DescribeNetworkInterfaces`. `ssm:SendCommand`는 문서 리소스(`HopCharlie-Inspect`, `HopCharlie-Connect`)와 인스턴스 리소스를 따로 둔 문이고, Connect용 인스턴스 문은 `ssm:resourceTag/hopcharlie-onboarding`이 있을 때만이다. `ssm:GetCommandInvocation`. 키 `hopcharlie-onboarding`에 한정한 `ec2:CreateTags`. 온보딩 태그가 있는 인스턴스의 `ec2:StopInstances` | 임의 SSM 문서, `AWS-RunShellScript`, 인스턴스 종료와 삭제 |
| 같은 역할의 거부 | `eks:cluster-name`이나 `aws:autoscaling:groupName` 태그가 있는 인스턴스(`Null` 조건)에 대한 `CreateTags`, `SendCommand`, `StopInstances` | |
| 플랫폼(GCP) | 프로젝트의 인스턴스 조회 | VM 접속 |
| 플랫폼(GitHub App) | `contents:write`, `pull_requests:write`, `actions:write` | `workflows`, `secrets`, `administration` |
| 플랫폼(Tailscale) | `tag:hop-legacy`로만 키를 만들고 장치를 지울 수 있는 OAuth 클라이언트 | 정책 수정 |
| hopai | 온보딩 레포 하나의 1시간 `contents:write` 토큰, Facts | PR 권한, 비밀값, 클라우드 자격 증명, VM 접근 |
| 엔진 run | 그 run의 프로세스 환경으로 받은 원본 superuser DSN과 rclone 원격 | |

태그는 플랫폼이 스스로 붙이므로, 태그 조건은 "조사에서 고른 인스턴스에만 Connect와 Stop을 한다"는 플랫폼 코드의 약속을 IAM으로 한 번 더 묶는 정도다. 실제 경계는 위의 거부 조건과 고정 문서다. SSM 문서는 infra Terraform이 소유하고, 문서 안의 명령은 리뷰한 고정 스크립트다. 플랫폼이 명령 문자열을 만들어 넘기지 않는다.

### 한 번 하는 설정

| 할 일 | 맡는 곳 |
| --- | --- |
| GitHub App 권한에 `pull_requests:write`, `actions:write`를 더하고 설치에서 승인한다 | org owner |
| tailnet 정책에 `tag:hop-legacy`의 tagOwners를 두고, `tag:hop-db-client-{management,eks,gke}`에서 `tag:hop-legacy`로 가는 tcp:5432 grant를 더한다. OAuth 클라이언트를 만들어 SSM `/hopcharlie/tailscale-onboard-oauth`에 넣는다 | tailnet 관리자 |
| 플랫폼 역할과 GCP SA 권한, SSM 문서 두 개 | infra Terraform, apply는 사람 |
| 차트: `dbTopology.external`, `image`, DB 이름과 owner, replicas 0 허용, cold 라벨. ApplicationSet: cold 라벨 Deployment의 `/spec/replicas`와 Cluster `/spec/bootstrap` 무시. 관리 route policy: 이름 `^db-legacy-`, FQDN `hop-db-legacy-`, hostname `hop-db-management-to-legacy-` 허용. 워크로드 admission policy: `replica.source`를 `legacy`에서 짝 이름으로 바꾸는 것만 허용 | infra |
| 게이트웨이 `dial`, renderer의 replicas 0, Plan 정책의 온보딩 cold | platform |
| 외부 원본 switchover 경로, fence gate의 `COLLATE "C"` | migration-engine |

앱마다 사람이 하는 일은 다음 둘이다. 둘 다 그 앱 소유자의 권한이라 플랫폼이 대신할 수 없다. 콘솔이 할 일과 링크를 보여 주고, 끝났는지 플랫폼이 확인한다.

- GitHub App을 그 레포에 설치한다(레포 접근 허락).
- 원래 주소의 개별 DNS 레코드를 지운다(infra ADR 0036 때문에 플랫폼은 DNS를 바꾸지 않는다).

VM이 SSM 관리 대상(SSM agent와 인스턴스 프로필)이어야 한다는 것은 대상 VM의 전제 조건이다. 아니면 조사가 차단 사유와 설정 방법을 보여 준다.

### 구현 전에 확인할 것

- 차트를 고친 뒤 실제 렌더 결과로 같은 시나리오를 다시 돌린다. 짝 붙이기와 양방향 전환은 손으로 쓴 차트 형태 manifest로 확인했다(상황).
- replica 상태의 Cluster에서 백업(ScheduledBackup `immediate`)이 실패하는지 본다. 실패하면 첫 백업을 승격 뒤로 미룬다.
- 실제 VM에서 Tailscale `TCPForward`가 compose 네트워크의 다른 컨테이너 이름으로 동작하는지 본다. 안 되면 다른 방식을 정할 때까지 이 단계에서 멈춘다(loopback 전달은 쓰지 않는다).
- ArgoCD ignoreDifferences를 라벨이 붙은 Deployment에만 거는 방식(jqPathExpressions 등)이 지금 ApplicationSet에서 동작하는지 본다.

## 검토한 대안

- **원본 레포의 compose와 workflow를 PR로 고쳐서 Tailscale 사이드카와 hba를 넣기**: 원본 배포가 PR로 바뀌고, App에 `workflows`와 `secrets` 권한이 필요하다. DB 준비가 SQL과 reload로 재시작 없이 끝나는 것을 확인했으므로 고르지 않았다.
- **원본 DB 포트를 인터넷에 열고 TLS로 복제**: 공식 이미지에는 TLS 설정이 없어서 원본을 고쳐야 하고, 워크로드 앱 네임스페이스의 egress도 넓혀야 한다. 팀의 클라우드 간 DB 경로(infra ADR 0048)와도 다르다.
- **논리 복제**: RDS도 다룰 수 있지만 스키마 복사, 시퀀스 맞추기, PK 없는 테이블 처리, `wal_level` 변경(재시작)이 필요하다. 물리 복제는 엔진의 기존 전환 순서를 거의 그대로 쓴다.
- **pg_dump와 복원**: 전환 동안 쓰기를 오래 막아야 해서 무중단이 아니다.
- **원본 fence를 role `NOLOGIN`으로**: role 변경이 대상에 복제되어 승격 뒤 앱이 로그인하지 못한다.
- **복제 계정으로 fence**: `ALTER SYSTEM`과 superuser 연결 종료를 할 수 없다.
- **대상 앱을 전환 전에 띄워 두기(warm)**: 일반 앱은 read-only DB에서 시작하지 못한다. moimroom만 replica에서 마이그레이션을 건너뛴다.
- **조사 결과를 S3 버킷이나 SSM SecureString으로 주고받기**: VM 인스턴스 역할에 버킷 권한이 따로 필요하고, `{{ssm-secure:}}`는 VM 역할로 우리 SSM을 읽게 한다. VM 안에서 암호화해 stdout으로 받으면 둘 다 필요 없다.
- **Tailscale 컨테이너에 고정 IP**: compose 기본 네트워크는 `--ip`를 받지 않는다.
- **원래 도메인을 그대로 게이트웨이로 받기**: 게이트웨이와 ALB가 `*.hopcharlie.dev`만 받는다. 다른 도메인은 인증서와 Host 라우팅을 더해야 한다.
- **Cloudflare DNS를 플랫폼이 바꾸기**: infra ADR 0036이 거절했다.
- **ai-sandbox 샌드박스에서 앱 시험**: 샌드박스는 Pod를 만들지 못한다. 실제 배포 경로로 임시 앱을 띄우는 쪽이 빌드와 차트까지 같이 시험한다.
- **hopai가 PR을 직접 열기**: hopai에 `pull_requests:write`가 필요하다. 플랫폼이 열면 hopai 토큰은 ADR 0023 그대로다.
- **SSM 명령 출력으로 비밀값 받기**: SSM 읽기 권한이 있는 누구나 평문으로 볼 수 있고, 출력은 24,000자에서 잘린다.

## 남은 위험

- 원본에 replication slot이 없다. `wal_keep_size` 1GB를 넘는 WAL이 bootstrap이나 지연 중에 생기면 대상이 따라가지 못한다. `syncing`에서 streaming이 끊기면 대상 Cluster와 PVC를 지우고 다시 bootstrap한다.
- pg_basebackup은 원본의 spread checkpoint를 기다린다. 큰 DB는 `checkpoint_timeout`만큼 걸릴 수 있다.
- 승격 직후 CNPG가 postgres를 한 번 재시작한다(약 0.4초). 보류를 푸는 시점은 엔진 ADR 0009의 안정 대기 뒤다.
- glibc 릴리스가 다른 이미지밖에 없으면 collation version 경고가 남는다. 차단하지 않고, 전환 뒤 `ALTER DATABASE ... REFRESH COLLATION VERSION`과 REINDEX를 리포트에 권고로 남긴다.
- 게이트웨이를 거치지 않고 DB에 직접 쓰는 작업(cron, 워커)은 HTTP 보류가 붙잡지 못한다. 엔진 ADR 0012와 같은 한계다. fence 뒤의 쓰기는 read-only 오류로 실패하고 데이터는 잃지 않는다.
- 머지 결과가 아니라 PR head를 시험한다. 기본 브랜치가 움직이면 승인 단계에서 다시 만들고 시험한다.
- 원본 배포는 `connecting`부터 정리까지 멈춘다. 그동안 원본 레포에 들어온 변경은 대상 앱의 배포로만 나간다.
- REINDEX와 앱 시작 시간 추정이 틀리면 보류가 `MaxHold`를 넘어 일부 쓰기가 503을 받는다. 데이터는 잃지 않는다.
- fence 뒤에 새로 연결한 세션이 스스로 `SET default_transaction_read_only = off`를 하면 원본에 쓸 수 있다. 앱이 이렇게 하지 않는다고 본다.

## 추가: 계약 산출물 (2026-11-07)

다른 작업(플랫폼 구현, infra 차트, migration-engine, hopai, 콘솔, CLI)이 함께 기준으로 삼는 계약을 먼저 머지한다. 이 커밋은 런타임 동작을 바꾸지 않는다. API는 `PLATFORM_ONBOARDING=1`일 때만 경로가 생기고 모두 501 `not_implemented`로 답한다.

| 계약 | 파일 | 내용 |
| --- | --- | --- |
| API | `api/openapi.yaml`, `pkg/apiv1` | `/v1/onboardings` 아래 9개 경로(`startOnboarding`, `listOnboardings`, `getOnboarding`, `deleteOnboarding`, `streamOnboardingEvents`, `approveOnboarding`, `cutOverOnboarding`, `decommissionOnboarding`, `resumeOnboarding`). 스키마 `Onboarding`, `OnboardingState`(위 표의 상태와 `failed`), `OnboardingHumanAction`, `OnboardingFactsSummary`, `OnboardingReport`, `OnboardingPR`, `OnboardingCheck`, `OnboardingLegacyChange`, `OnboardingEvent`. 오류 코드 `onboarding_not_found`, `onboarding_exists`, `onboarding_state_conflict`, `pr_head_mismatch`, `not_implemented` |
| Facts | `pkg/onboard/facts` | 조사 결과의 Go 타입. ai-sandbox가 이미 platform 모듈을 import하므로 JSON Schema 대신 `pkg/` 아래 표준 라이브러리만 쓰는 패키지로 둔다. 차단 사유 `Block*`, 경고 `Warn*` 코드, 보류 예산 `HoldBudgetSeconds = 40`. 비밀값은 없고 환경 변수는 `{name, secret, excluded}`만 있다 |
| 이름과 hopai 입력 | `internal/onboard` | `legacy`, `hopcharlie_replicator`, `LEGACY_PGPASSWORD`, 브랜치 `hopcharlie/onboard`, 태그 `hopcharlie-onboarding`, 라벨 `hopcharlie.dev/onboarding-cold`, `db-legacy-<app>`, `hop-db-legacy-<app>`, 임시 앱 `<app>-onb-<id 끝 6자>`. hopai Job 환경 변수(`KIND=onboard`, `ONBOARDING`, `REPO`, `BRANCH`, `FACTS`, `CHECK_LOG`)와 엔진 Plan 모양을 패키지 주석에 적는다 |
| store | `internal/store/migrations/0016_onboardings.sql`, `internal/store/onboarding.go` | 테이블 `onboardings`(상태, `doc`은 `apiv1.Onboarding` JSON, `facts`, 암호문 `secrets_*`, `superuser_password_*`, `replication_password_*`)와 `onboarding_events`. 앱마다 done이 아닌 온보딩은 하나다. `app_databases`에 `source_kind`(`peer` 기본, `external`), `image`, `database`, `owner`, `external_host`, `replication_password_nonce`, `replication_password_ciphertext`, `reindex text[]`를 더한다. 암호문의 additional data는 `("onboardings", id, field, "")`이고 field는 `secrets`, `superuser.password`, `replication.password`다. `app_databases`의 복제 비밀번호는 `("app_databases", app, "replication.password", "")`다(`internal/secrets` 상수) |
| gitops 값 | `internal/deploy/gitops.go` | `values.postgres {image, database, owner, storageSize}`, `values.dbTopology.external {name, host, user, passwordSecret, sslmode}`, `values.onboardingCold`(서비스 replicas 0과 cold 라벨), `values.sealedSecrets.pgLegacy`(SealedSecret `<cluster>-legacy`). 정확한 YAML 모양과 차트가 그릴 것은 `PostgresValues` 주석에 있다. `Render`는 구현 전까지 이 값들을 거부하므로 기존 앱의 파일은 바이트 단위로 그대로다 |
| 게이트웨이 | `internal/gateway/apps.go` | `AppSpec.Dial`, `AppInfo.Dial`(`dial`, 백엔드 이름에서 `host:port`). 구현 전까지 비어 있지 않은 `dial`은 400 `dial_not_supported`다. 게이트웨이 API는 OpenAPI 밖이라 `pkg/apiv1`에는 없다 |
| 엔진 Plan | `internal/onboard` 주석 | `source.kind: external`, `source.name: legacy`, `source.env: legacy`, `source.dsn`(`${LEGACY_PGPASSWORD}`), `target.reindex`, `target.app {mode: cold, kind: Deployment, name, replicas}`, `gateway.sourceBackend: legacy`. migration-engine `feat/external-source`의 `pkg/plan` 이름과 같다 |

계약을 정하면서 이 ADR에서 정한 것은 다음과 같다.

- 실패한 온보딩도 그 앱을 잡고 있다. 같은 앱을 다시 온보딩하려면 `resume`하거나 `DELETE`한다.
- `DELETE`는 202로 바로 답하고 `deleting`을 true로 둔다. 되돌리기가 끝나면 행이 없어진다.
- 전환 run이 halt로 멈추면 온보딩은 `cutting_over`에 남고, run의 resume(`POST /v1/migrations/{runId}/resume`)으로 이어 간다. 온보딩의 `resume`은 `failed`와 사람을 기다리는 상태만 받는다.
- cold는 앱의 모든 서비스를 0으로 둔다. 엔진은 공개 서비스만 띄우고, 다른 서비스는 run이 성공한 뒤 플랫폼이 원래 replicas를 쓸 때 뜬다.
- 승격 뒤 `legacy` 비밀번호 Secret은 `sealedSecrets.pgLegacy`가 있는 동안 `dbTopology.external`과 상관없이 그린다.
