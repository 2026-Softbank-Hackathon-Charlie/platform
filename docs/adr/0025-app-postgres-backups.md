# 0025. 앱 Postgres는 Barman Cloud plugin으로 앱 전용 백업 버킷에 백업한다

- 상태: 제안
- 날짜: 2026-11-05
- 관련 ADR: [0011](0011-github-app-deploy.md), [0015](0015-branch-instances.md), [0016](0016-app-object-storage.md), [0019](0019-app-secrets-and-redeploy.md), infra ADR 0006(다른 인증 방식, 아래 참고), 0050, 0057, migration-engine ADR 0001, 0008
- 관련 PR: 이 PR(버킷과 키), platform #75(환경 화면의 백업 상태), infra #75(차트), #76(ObjectStore 읽기 권한), #77(eks DB 디스크 4Gi), #80(GKE moimroom DB 백업, ADR 0057, 이전 runbook), frontend #36(콘솔)

## 상황

앱의 `postgres` 리소스는 인스턴스 하나짜리 CNPG Cluster이고 백업이 없다. 볼륨이나 단일 AZ의 EBS를 잃으면 되돌릴 수 없다. 심사 기준은 백업을 포함해 원래 환경과 같은 수준의 운영 구성을 요구한다.

확인한 사실은 다음과 같다.

- 워크로드 EKS와 GKE 모두 CNPG 1.30.1과 Barman Cloud plugin v0.15.1이 떠 있다. infra의 `charlie-db/postgres-poc`가 두 클러스터에서 이 plugin으로 매일 백업하고 있다(EKS는 Pod Identity로 S3, GKE는 Workload Identity로 GCS).
- 클러스터 내장 Barman(`spec.backup.barmanObjectStore`)은 CNPG에서 지원 중단 예정이고 infra 검사도 막는다. VolumeSnapshot은 워크로드 EKS에 CRD가 없다.
- plugin을 쓰면 마지막 백업 시각과 복구 가능 시작 시점은 Cluster status가 아니라 ObjectStore `status.serverRecoveryWindow`에 있다.
- 이전 엔진의 파일 이전은 버킷 전체를 `rclone sync`한다. 앱 파일 버킷에 백업을 같이 두면 원본의 백업이 대상으로 복사되고, 대상 버킷에 있던 대상 DB의 백업은 지워진다.
- GCS를 S3 호환 API와 HMAC 키로 쓰는 경로(`gcs` 드라이버)는 Barman으로 검증한 적이 없다. plugin 문서는 boto3의 checksum 기본값이 S3가 아닌 저장소에서 실패할 수 있다고 적는다.

## 결정

### 백업 방식

- Barman Cloud plugin을 WAL 보관자로 쓴다. Barman의 base backup은 보관된 WAL이 있어야 일관된 상태로 복원되므로 WAL 보관을 반드시 켠다.
- 매일 한국 시각 03:00(UTC 18:00)에 base backup을 하고, 켜자마자 한 번 한다. 보존은 7일이고 plugin이 지운다.
- 차트(infra `helm/charts/hopcharlie-app`)가 Cluster의 `plugins`, ObjectStore, ScheduledBackup을 그린다. 환경 프로필의 `profile.postgres.backup.enabled`가 켜져 있고 플랫폼이 아래 값을 보냈을 때만 그린다.

### 버킷과 키(eks, `aws` 드라이버)

- 앱의 기본 인스턴스(앱 이름과 같은 인스턴스)에 `postgres` 리소스가 있으면, 환경 설정의 저장소 드라이버가 `aws`일 때 배포 실행기가 앱 전용 백업 버킷과 그 버킷만 쓰는 키를 만든다. 브랜치 인스턴스는 미리보기라 만들지 않는다.
- 드라이버를 만들 수 있었는지가 아니라 설정으로 정한다. 설정은 `aws`인데 드라이버를 만들지 못했으면(예: 자격 증명 오류, platform #66 이후 API는 그래도 뜬다) 배포를 실패시킨다. 백업 값 없이 배포하면 차트가 plugin, ObjectStore, ScheduledBackup을 빼서 돌고 있는 DB의 백업이 꺼지기 때문이다.
- ADR 0016의 드라이버를 그대로 쓴다. 드라이버가 만드는 이름의 인스턴스 자리에 `<app>---pg`를 넘긴다. 버킷은 `<bucketPrefix><app>---pg`(예: `hopcharlie-eks-moimroom---pg`), IAM 사용자는 `hopcharlie-app-<app>---pg`다. 앱 이름에는 `--`가 없고 브랜치 슬러그에는 `-`가 연달아 오지 않으므로 어떤 인스턴스 이름에도 `---`가 없다. 그래서 다른 인스턴스의 버킷과 겹치지 않는다. 앱 이름은 32자까지라 버킷과 사용자 이름이 길이 제한 안에 든다.
- 키는 ADR 0016처럼 봉인한다. strict 범위, 네임스페이스는 인스턴스, Secret 이름은 `app-pgbackup`이다. gitops 파일에는 `values.sealedSecrets.pgBackup`(키)과 `values.backup`(`bucket`, `path`)이 들어간다.
- 인증서가 바뀌면 새 키를 만들고, 옛 키는 그 뒤 healthy가 된 배포가 지운다. plugin은 WAL을 올릴 때마다 Secret을 다시 읽으므로 파드가 다시 뜰 필요가 없다.
- 버킷이나 키를 만들지 못해도 s3 리소스와 같이 배포를 실패시킨다. 같은 이유다.
- GKE(`gcs` 드라이버)에는 만들지 않는다. HMAC 경로를 검증하지 않았기 때문이다. 지금 GKE의 moimroom DB는 플랫폼 배포가 아니라 손으로 만든 Cluster이므로, infra ADR 0057(infra #80)이 Workload Identity로 따로 백업한다.

### 인증 방식: infra ADR 0006과 달리 장기 키를 쓴다

- infra ADR 0006(채택)은 `charlie-db/postgres-poc` 백업에 전용 버킷과 Pod Identity를 쓰고 장기 access key를 만들지 않기로 했다. 그 ADR은 범위를 PoC Cluster로 한정했지만, 같은 클러스터에서 같은 plugin으로 하는 백업이므로 그 원칙과 다르게 가는 이유를 여기 적는다.
- 이 결정에서는 플랫폼이 배포할 때 앱마다 백업 저장소를 만든다. Pod Identity를 쓰려면 플랫폼이 앱마다 IAM 역할을 만들고 그 앱 DB의 ServiceAccount에 EKS Pod Identity association을 걸어야 한다. 지금 플랫폼 역할에는 IAM 사용자를 만드는 권한(`iam:CreateUser`)만 있고 역할과 association을 만드는 권한이 없다(infra `terraform/platform/aws/app-storage.tf`). 앱을 지울 때 역할과 association을 정리하는 경로도 새로 필요하다. 제출 전까지 이 경로를 만들고 검증할 수 없어서, 이미 운영 중인 ADR 0016의 드라이버를 그대로 썼다.
- ADR 0016이 앱 저장소에 정적 키를 고른 이유는 세 환경에서 앱 설정을 같게 하는 것이다. 백업 키는 앱이 쓰지 않으므로 이 이유는 해당하지 않는다. GKE 백업(infra ADR 0057)도 키 없는 Workload Identity를 쓴다. 따라서 장기 키는 원칙으로 정한 것이 아니라 일정 때문에 고른 임시 방식이다.
- 키의 범위는 ADR 0016과 같다. 버킷 하나만 허용하는 인라인 정책, permissions boundary `hopcharlie-app-boundary`, 앱 네임스페이스의 봉인된 Secret으로 제한하고, 인증서가 바뀌면 새 키를 만들고 옛 키를 지운다.

### 행은 따로 두고 지우지 않는다

- 백업 버킷과 키의 행은 `instance_storage`가 아니라 새 테이블 `backup_storage`(앱, 환경)에 둔다. 지운 인스턴스의 버킷을 지우는 정리(ADR 0016)는 `instance_storage`만 본다. 이 결정 이전의 플랫폼 이미지로 되돌려도 그 이미지는 이 테이블을 모르므로 백업 버킷을 지우지 않는다.
- 앱을 지워도 백업 버킷, 사용자, 키, 행은 남는다. 백업은 앱 데이터를 잃었을 때 쓰는 것이라 앱과 같이 사라지면 안 된다. 필요 없어진 백업은 운영자가 지운다(아래 절차).

### 등록마다 새 경로

- 버킷 안의 경로는 앱을 등록한 시각(UTC, `20060102T150405Z`)이다. ObjectStore의 `destinationPath`는 `s3://<bucket>/<path>/`, Barman의 서버 이름은 Cluster 이름이다.
- 같은 이름으로 지웠다가 다시 등록한 앱은 새 경로를 받는다. Barman은 새 DB가 처음 WAL을 보관할 때 그 자리가 비어 있는지 검사하고, 비어 있지 않으면 보관을 멈춘다. 새 경로라 이 검사를 통과한다.
- 다시 등록한 앱은 같은 버킷과 키를 이어 쓰므로 예전 등록의 백업도 읽을 수 있다. 키를 등록 경로로 좁히려면 드라이버 정책을 경로마다 바꿔야 해서 이번에는 받아들인다. 키는 앱 네임스페이스의 Secret에만 있어 그 클러스터에 접근할 수 있는 사람만 쓸 수 있다. 다른 사람에게 같은 이름을 줄 때는 운영자가 먼저 백업 버킷을 지운다.

### 상태 표시(platform #75, infra #76, frontend #36)

- 환경 화면의 `DatabaseCluster`에 `backup`을 더한다(`objectStore`, `walArchiving`, `lastSuccessfulBackup`, `lastFailedBackup`, `firstRecoverabilityPoint`). WAL 보관자 plugin이 켜진 Cluster에만 있다. CNPG는 보관자가 없어도 `ContinuousArchiving`을 True로 두므로 그 조건만으로 보관 중이라고 말하지 않는다.
- 플랫폼 계정은 워크로드에서 `objectstores`를 `list`할 수 있어야 한다(infra `hopcharlie-platform-access.yaml`). 권한이 없으면 시각 없이 보여 주고 `EnvironmentStatus.error`에 이유를 적는다.

## 결과

- 백업을 켜거나 끄면 CNPG가 plugin 컨테이너를 넣거나 빼려고 Postgres 파드를 한 번 다시 만든다. 인스턴스가 하나라 20~40초 동안 DB가 내려간다.
- WAL 보관이 실패하면 PostgreSQL이 WAL을 지우지 못해 디스크가 찬다. CNPG 기본값(`wal_keep_size` 512MB, `max_wal_size` 1GB)만으로 1Gi를 넘으므로 eks 프로필의 디스크를 4Gi로 늘리고, 콘솔이 WAL 보관 실패를 경고한다.
- **이전과의 관계.** CNPG 1.30.1은 원본을 강등할 때 shutdown checkpoint가 든 WAL을 보관해야 demotion token을 낸다(`replicaclusterswitch/shutdown_wal.go`). 원본의 WAL 보관이 실패하면 토큰이 나오지 않는다. 그러면 엔진은 migration-engine ADR 0008대로 원본이 펜싱된 채 쓰기를 게이트웨이에 붙잡고 `halted`로 멈춘다. 자동 원복은 없고, 게이트웨이의 최대 보류 시간이 지나면 쓰기는 503이 된다. 그래서 이전을 시작하기 전에 운영자가 원본의 WAL 보관을 확인한다(infra `docs/runbooks/aws-gcp-db-migration.md` 1절 6번, infra #80). 엔진은 바꾸지 않는다.
- 전환 뒤 옛 원본은 새 primary를 따르는 replica로 남고, 받은 WAL을 계속 자기 저장소에 보관하며 매일 백업한다. 다른 클라우드에 사본이 하나 더 생긴다.
- **replica의 백업은 primary를 기다린다.** replica(`archive_mode: always`)에서 하는 base backup은 `pg_backup_stop`에서 백업 끝의 WAL이 보관되기를 기다리는데, replica는 세그먼트를 스스로 바꾸지 못한다. primary에 쓰기가 없으면 다음 쓰기 뒤 `archive_timeout`(5분)이 지날 때까지 Backup이 `running`으로 남는다. 지금 EKS의 `moimroom-db`가 GKE primary를 따르는 replica이므로, GKE 쪽 백업을 10분 뒤(03:10 KST, infra #80)에 두어 그 백업이 세그먼트를 바꿔 주게 했다. 오래 걸리면 primary에서 `SELECT pg_switch_wal()`을 한 번 실행한다.
- 같은 등록 안에서 Cluster를 같은 이름으로 다시 만들면(예: 그 환경의 gitops 파일을 지웠다 다시 배포) 경로가 같아 빈 보관소 검사에 걸린다. 운영자가 그 경로의 `<Cluster 이름>/`을 지우거나 옮긴 뒤 다시 만든다.
- **이미지를 되돌리면.** 이 결정 이전의 플랫폼 이미지로 되돌리면 버킷과 키, 객체는 남는다. 그러나 그 이미지의 다음 배포가 백업 값 없는 gitops 파일을 쓰므로 차트가 plugin, ObjectStore, ScheduledBackup을 빼고(Postgres 파드가 한 번 다시 만들어진다) ScheduledBackup이 소유한 Backup 기록도 지워진다. S3의 base backup과 WAL은 남아 복원할 수 있다.
- 비용: 버킷 저장량(7일치 base backup과 WAL)과 요청 비용이 든다.

## 후속 작업

- 이전 직전 원본의 수동 백업(Backup)과 그 이름을 이전 기록의 원복 지점으로 남기는 일은 이전 엔진 담당과 따로 정한다.
- GKE의 `gcs` 드라이버로 같은 백업을 하려면 Barman과 GCS HMAC 조합을 먼저 검증한다.
- EKS 백업을 infra ADR 0006처럼 Pod Identity로 바꾼다. 플랫폼 역할에 앱 백업용 IAM 역할 생성(경계 적용)과 `eks:CreatePodIdentityAssociation`, `eks:DeletePodIdentityAssociation`을 주고, 배포 실행기가 앱 DB의 ServiceAccount에 association을 걸며, 앱을 지우면 함께 지운다. 바꾼 뒤에는 장기 키와 `app-pgbackup` Secret을 지운다.

## 운영자 정리 절차

앱을 지운 뒤 백업이 필요 없으면 다음 순서로 지운다.

1. IAM 사용자 `hopcharlie-app-<app>---pg`의 액세스 키, 인라인 정책 `hopcharlie-bucket`, 사용자를 지운다.
2. 버킷 `<bucketPrefix><app>---pg`의 모든 객체 버전을 지우고 버킷을 지운다.
3. 플랫폼 DB에서 `DELETE FROM backup_storage WHERE app = '<app>'`를 실행한다.

infra `terraform/platform/aws`를 destroy하기 전에도 이 절차로 모든 백업 사용자를 지운다. 플랫폼이 지우지 않으므로 permissions boundary를 단 사용자가 남아 destroy가 실패한다.

## 관련 자료

- [internal/deploy/sealed.go](../../internal/deploy/sealed.go), [internal/store/migrations/0010_backup_storage.sql](../../internal/store/migrations/0010_backup_storage.sql)
- infra `helm/charts/hopcharlie-app/templates/postgres.yaml`
- [Barman Cloud plugin](https://cloudnative-pg.io/plugin-barman-cloud/docs/), [CNPG replica clusters](https://cloudnative-pg.io/docs/1.30/replica_cluster)
