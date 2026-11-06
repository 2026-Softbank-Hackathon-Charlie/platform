# 0030. 두 환경에 등록한 앱의 DB는 처음부터 이전 가능한 짝으로 만든다

- 상태: 제안
- 날짜: 2026-11-06
- 관련 ADR: [0011](0011-github-app-deploy.md), [0015](0015-branch-instances.md), [0016](0016-app-object-storage.md), [0019](0019-app-secrets-and-redeploy.md), [0025](0025-app-postgres-backups.md), infra ADR 0048, 0060, 0063(차트, 이 ADR의 짝), migration-engine ADR 0001, 0006
- 관련 PR: 이 PR(플랫폼), infra `feat/app-db-topology`(차트), infra #99(관리 EKS에서 앱 DB로 가는 길)

## 상황

콘솔의 이전 버튼(`POST /v1/migrations`)은 엔진 plan을 실행하고, 엔진 precheck는 CNPG distributed topology를 요구한다. 원본은 `replica.primary == replica.self`인 primary이고, 대상은 원본을 따르는 replica여야 한다. primary는 나중에 replica로 바꿀 수 없다(system id가 다르다). 그래서 두 번째 환경의 DB는 처음부터 replica로 태어나야 한다.

지금은 두 환경에 등록한 앱(예: `moimroom-mig`)이 환경마다 독립 primary를 가진다. 앱 차트가 `bootstrap`, `replica`, `externalClusters`, `certificates`를 쓰지 않기 때문이다. 운영 moimroom은 상대 클러스터의 CA와 `-replication` Secret을 손으로 복사해 짝을 만들었다.

확인한 사실:

- 등록한 환경 목록은 바뀌지 않는다. `store.App.Environments`는 `PLATFORM_ENVS_FILE` 순서이고 바꾸는 API가 없다.
- 새 앱의 쓰기 백엔드는 응답한 환경 중 `app.Environments`의 첫 번째이고, 배포는 그 뒤로 쓰기 백엔드를 바꾸지 않는다(`internal/deploy/job.go` `gatewaySpec`).
- ApplicationSet은 CNPG Cluster의 `.spec.replica`, `.spec.externalClusters`, `.spec.certificates`를 무시한다. 이 필드는 Cluster를 만들 때만 적용되고, 엔진이 바꾼 `replica`를 배포가 되돌리지 않는다.
- CNPG는 replica 클러스터에 app Secret을 만들지 않는다. 차트의 `DATABASE_URL` 등은 `<cluster>-app`을 읽으므로 replica 쪽에도 그 Secret이 있어야 한다.
- CNPG는 사용자 CA Secret에 `ca.key`가 있으면 서버 인증서와 `streaming_replica` 인증서를 그 CA로 만들고 90일마다 갱신한다. CA 키는 PEM `EC PRIVATE KEY`(SEC1)여야 한다. 사용자 CA 자체는 갱신하지 않고 만료 경고만 낸다.
- 엔진은 plan의 `${VAR}`을 API 프로세스 환경에서만 펼친다(`plan.Parse`).

## 결정

### 대상

- `PLATFORM_APP_DB_TOPOLOGY=1`이고 `PLATFORM_SECRETS_KEY`가 있는 서버에서, 환경이 정확히 두 개이고 두 환경 모두 `PLATFORM_APP_DB_HOST_TEMPLATES`에 있으며 platform.yaml에 `postgres` 리소스가 있는 앱을 등록하면 DB 짝을 만든다. 하나라도 아니면 짝 없이 예전처럼 등록한다. 끄면 새 앱도 예전과 같다.
- 기본 인스턴스(앱 이름과 같은 인스턴스)의 `postgres` 리소스만 대상이다. 브랜치 인스턴스는 지금처럼 환경마다 독립 DB다.
- 세 환경 이상은 대상이 아니다. 원본의 `replica.source`는 상대 하나만 가리킨다.
- 이미 등록된 앱은 행이 없어서 gitops 파일이 바이트 단위로 그대로다. 짝이 필요하면 삭제하고 다시 등록한다.

### 저장: `app_databases`

- 마이그레이션 `0011_app_databases.sql`이 앱마다 한 행을 둔다. 열은 primary 환경(`app.Environments[0]`), 다른 환경, app 계정 비밀번호, CA 인증서와 키다.
- 비밀번호와 CA 키는 `PLATFORM_SECRETS_KEY`로 AES-256-GCM 암호화해서만 저장한다(ADR 0019와 같은 키, key version, key check). additional data는 `app_databases`, 앱, 필드 이름이라 다른 앱의 행이나 앱 시크릿으로 옮기면 풀리지 않는다. CA 인증서는 공개 값이라 평문으로 둔다.
- `POST /v1/apps`는 앱 행과 짝 행을 한 트랜잭션에서 만든다(`CreateAppWithDatabase`). 하나라도 실패하면 등록 전체가 실패하고 첫 배포도 큐에 들어가지 않는다.
- `DeleteApp`이 같은 트랜잭션에서 행을 지운다. 다시 등록하면 새 비밀번호와 새 CA를 쓴다. 네임스페이스와 DB도 새로 생기므로 맞다.

### 생성

- 비밀번호는 `crypto/rand`로 뽑은 영숫자 32자다. URL에 그대로 넣어도 된다.
- CA는 ECDSA P-256 자체 서명, `IsCA`, `KeyUsageCertSign|CRLSign`, 유효기간 10년이다. 키는 `x509.MarshalECPrivateKey`로 SEC1 PEM `EC PRIVATE KEY`를 만든다. CA 갱신 절차는 이번 범위가 아니다. CNPG가 만료 전에 경고한다.

### gitops 값 (infra 차트와의 계약)

배포할 때마다 두 환경 각각의 sealed-secrets 인증서로 같은 평문을 다시 봉인한다(strict scope, 네임스페이스는 인스턴스). 값은 `resources` 다음, `sealedSecrets` 앞에 쓴다. `pgApp`, `pgCA`는 `sealedSecrets` 안에서 `app`, `appChecksum` 다음, `pgBackup` 앞에 쓴다.

```yaml
values:
  dbTopology:
    primary: "gke"   # 태어날 때 primary인 환경
    peer: "eks"      # 태어날 때 replica인 환경. 두 환경의 파일에 같은 값을 쓴다
  sealedSecrets:
    pgApp:           # SealedSecret <app>-<pg>-app, type kubernetes.io/basic-auth
      "dbname": ...
      "host": ...
      "jdbc-uri": ...
      "password": ...
      "port": ...
      "uri": ...
      "username": ...
    pgCA:            # SealedSecret app-pg-ca
      "ca.crt": ...
      "ca.key": ...
```

- `pgApp` 평문은 CNPG가 만드는 형식과 같다. `username`, `dbname`은 `app`, `host`는 `<cluster>-rw`, `port`는 `5432`, `uri`는 `postgresql://app:<pw>@<cluster>-rw.<ns>:5432/app`, `jdbc-uri`는 `jdbc:postgresql://<cluster>-rw.<ns>:5432/app?password=<pw>&user=app`이다.
- `primary`와 `peer`는 서로 다르고 하나는 파일의 `env`와 같다. 차트는 파일의 env가 아닌 쪽을 상대로 본다. `Render`도 이 규칙과 postgres 리소스, `pgApp`, `pgCA`가 함께 있는지 검사한다.
- 짝 행이 있는데 서버에 `PLATFORM_SECRETS_KEY`가 없으면 배포가 실패한다(앱 시크릿과 같은 규칙).

### 이전 초안과 정책

- `PLATFORM_APP_DB_HOST_TEMPLATES`(예 `eks=db-eks-{instance}.hopcharlie-db-network.svc.cluster.local,gke=db-gke-{instance}.hopcharlie-db-network.svc.cluster.local`)가 있으면, 정적 카탈로그(`PLATFORM_APPS_FILE`)에 없는 이름은 짝 행이 있는 등록 앱으로 초안을 만든다. 이름이 겹치면 정적 카탈로그가 이긴다(운영 moimroom 보호). 파일이 없는 서버도 이 설정만으로 초안을 만든다.
  - namespace는 인스턴스, cluster는 `<app>-<pg>`, database와 user는 `app`, schemas는 비운다(엔진 기본 `public`).
  - DSN은 `postgres://app:${<ENV>_<APP>_PGPASSWORD}@<템플릿 호스트>:5432/app?sslmode=require`다. 관리 경로는 `sslmode=require`를 그대로 쓴다.
  - 파일 이전은 넣지 않는다. 앱 버킷 키는 평문이 없어서 서버 rclone 원격으로만 옮길 수 있고, 그 자격 증명이 앱 버킷에 닿는지 확인하지 않았다.
- 같은 앱은 `GET /v1/apps`, `GET /v1/apps/{name}`에 정적 카탈로그 앱과 같은 모양(namespace, database, 환경별 cluster와 dbHost)으로 보인다. 그래서 콘솔의 이전 화면, `GET /v1/traffic?app=`, `POST /v1/ai-runs`가 그대로 이 앱을 고른다. 비밀번호와 CA는 보이지 않는다.
- 정책의 비밀번호 변수 허용 호스트와 앱별 게이트웨이 범위(ADR 0012, #79)는 시작할 때 정한 정적 값에 더해, plan을 parse할 때마다 짝 행에서 다시 읽는다. 그래서 재시작 없이 새 앱을 이전할 수 있다. 정적 값과 겹치는 변수나 앱은 정적 값이 이긴다. 등록 앱의 DB 호스트만 쓰는 plan은 그 앱의 범위(`<gateway>/apps/<app>`)만 받는다.
- 비밀번호는 서버 시작 때 넣지 않는다. 초안을 만들 때와 plan을 parse할 때(제출, 시작, 재개, 되돌리기) 정책이 그 변수를 허용한 뒤에 행을 읽어 복호화하고 `os.Setenv`로 넣는다. plan과 DB에는 `${VAR}`만 남는다(엔진 ADR 0006).
  - 한 번 넣은 값은 프로세스 환경에 남는다. SSM `db-passwords.env`의 비밀번호와 같은 노출 범위다. API가 띄우는 rclone 같은 자식 프로세스도 이 환경을 물려받는다.

## 결과

- 짝으로 태어난 앱은 콘솔의 이전 버튼으로 primary 쪽에서 replica 쪽으로 옮길 수 있다. 이전한 뒤 배포해도 차트는 같은 `bootstrap`과 같은 born 역할을 그리고, `replica`, `externalClusters`, `certificates`는 Argo가 무시하므로 엔진이 바꾼 역할이 유지된다.
- 이전한 뒤 Cluster를 지우고 다시 만들면 등록 때의 역할로 다시 태어나 게이트웨이의 쓰기 환경과 어긋난다. 운영자 절차로 남긴다.
- CA 키가 두 클러스터의 인스턴스 네임스페이스 Secret에 있다. 범위는 앱 하나다.
- replica로 태어난 환경의 앱 파드는 읽기 전용 DB 위에서 떠야 한다. 시작할 때 스키마를 바꾸는 앱은 그 환경에서 Healthy가 되지 않는다.
- 초안의 원본 측정과 precheck는 관리 EKS에서 `db-<env>-<instance>.hopcharlie-db-network.svc.cluster.local`로 접속한다. 이 경로는 infra #99 영역이고, 앱마다 management 프로필에 `egress.peers` 항목 두 개(`eks-<instance>`, `gke-<instance>`)를 더하는 일이 남는다. 그 전에는 짝과 복제는 완성되지만 이전 버튼은 precheck의 원본 DB 접속에서 멈춘다.

## 적용 순서

1. infra 차트 PR(infra ADR 0063)을 병합한다. 그 전에 이 설정을 켜면 차트 스키마가 `dbTopology`를 거부해 sync가 실패한다.
2. 운영 platform 값(`clusters/aws-dev-management/values/hopcharlie-platform.yaml`)에 `PLATFORM_APP_DB_TOPOLOGY=1`, `PLATFORM_APP_DB_HOST_TEMPLATES`를 넣고, `PLATFORM_DSN_HOST_SUFFIXES`에 `.hopcharlie-db-network.svc.cluster.local`을 더한다.
3. moimroom-mig를 삭제하고 eks, gke로 다시 등록한다. gke가 primary, eks가 replica로 태어난다.
4. 플랫폼 저장소의 차트 사본 `deploy/charts/hopcharlie-app`에 infra 차트 변경을 옮겨 kind E2E가 같은 차트를 쓰게 한다.

## 검토한 대안

- 지금 있는 primary를 나중에 replica로 바꾼다: system id가 달라 CNPG가 따라가지 못한다.
- 엔진의 `target.manifest`로 이전할 때 대상을 만든다: 차트가 같은 이름의 Cluster를 이미 만들어 겹치고, 엔진 계정에 `create`와 Secret 권한이 필요하다.
- CNPG가 만든 CA와 Secret을 클러스터 사이에 복사한다(운영 moimroom 방식): 복사하는 주체에 양쪽 Secret 권한이 필요하고 순서를 Argo 밖에서 맞춰야 한다.
- 서버 인증서와 replication 인증서를 플랫폼이 직접 발급한다: 90일 갱신을 플랫폼이 맡아야 한다. CA만 주면 CNPG가 발급과 갱신을 한다.
- 서버 시작 때 모든 짝의 비밀번호를 환경에 넣는다: 새 앱마다 재시작이 필요하고, 이전하지 않는 앱의 비밀번호까지 환경에 둔다.
- 엔진에 비밀번호 조회 함수를 넣는다: 엔진 저장소까지 바꿔야 한다. 같은 효과를 parse 직전의 `os.Setenv`로 낸다.

## 추가: 관리 쪽 DB 경로를 플랫폼이 만든다 (2026-11-06)

"결과"에 남긴 일, 앱마다 management 프로필에 `egress.peers`를 더하는 일을 없앤다. 플랫폼 API가 관리 EKS에 직접 egress Service를 만들고 지운다(패키지 `internal/dbroute`).

- 켜는 법: `PLATFORM_APP_DB_ROUTE_TAILNET`(예 `tail567fe8.ts.net`), `PLATFORM_APP_DB_ROUTE_PROXY_CLASS`(`hopcharlie-app-db-egress-management`), `PLATFORM_APP_DB_ROUTE_TAG`(`tag:hop-db-client-management`)를 모두 넣는다. 셋 다 없으면 꺼져 있고 지금과 같다. 일부만 있거나 `PLATFORM_APP_DB_HOST_TEMPLATES`가 없으면 서버가 시작하지 않는다.
- 이름: 호스트 템플릿 `<service>.<namespace>.svc.cluster.local`에서 읽는다. `db-eks-{instance}.hopcharlie-db-network.svc.cluster.local`이면 namespace `hopcharlie-db-network`의 Service `db-eks-<app>`이다. 이 모양이 아니면 서버가 시작하지 않는다.
- Service: `type: ExternalName`, `externalName: placeholder`, 포트 `postgres` 5432. 라벨은 `app.kubernetes.io/managed-by: hopcharlie-platform`, `hopcharlie.dev/app: <app>`, `hopcharlie.dev/db-route: <env>`다. annotation은 `tailscale.com/tailnet-fqdn: hop-db-<env>-<app>.<tailnet>`(앱 차트의 ingress 이름), `tailscale.com/hostname: hop-db-management-to-<env>-<app>`, `tailscale.com/tags`, `tailscale.com/proxy-class`다.
- 만들 때: 짝이 있는 앱을 등록한 직후 백그라운드로 만들고, 실패는 로그만 남긴다. API 시작 때와 2분마다 짝 목록을 읽어 없는 Service를 만든다.
- 지울 때: 앱 삭제가 성공하면 바로 지운다. 주기 실행은 짝 목록에 없는 앱의 Service를 그 앱의 짝 행을 다시 읽어 없을 때(`ErrNotFound`)만 지운다. 짝 목록을 읽지 못했거나 목록이 비어 있으면 아무것도 지우지 않는다. 지울 때는 읽은 객체의 UID를 전제 조건으로 건다.
- 고치지 않는다: operator가 `externalName`과 finalizer를 바꾸므로 update와 patch는 하지 않는다. 설정이 바뀌어 annotation이 다르면 경고만 남긴다. 바꾸려면 Service를 지우고 다음 주기에 다시 만들게 한다.
- 남의 Service: 같은 이름이라도 라벨 셋과 이름이 맞지 않으면 만들지도 지우지도 않고 경고(`conflict`)를 남긴다. infra #99의 정적 `db-eks`, `db-gke`는 이름이 달라 겹치지 않는다.
- 관리 클러스터 권한(infra가 별도 매니페스트로 적용): `hopcharlie-db-network`의 Role에 `services`의 `get, list, watch, create, delete`, `charlie-platform/hopcharlie-platform`에 RoleBinding. update와 patch는 주지 않는다. VAP로 CREATE는 라벨과 이름, `placeholder`, annotation 값을, DELETE는 `oldObject`의 managed-by 라벨을 검사한다.
- 초안의 원본 측정이 실패하면(`sourceStatsReason: not_measured`) 서버 로그와 API 설명에 "등록 직후라면 관리 경로를 만드는 중일 수 있으니 잠시 뒤 다시 초안을 만든다"를 남긴다. 경로 준비 상태를 API로 보여 주는 일(`routes[]`, 409 `db_route_pending`)은 하지 않는다. operator의 준비 조건이 실제 연결을 뜻하지 않기 때문이다.
- 등록 앱마다 관리 EKS에 프록시 Pod가 환경마다 하나씩 생긴다. 앱 수가 늘면 ProxyGroup egress를 다시 검토한다.
