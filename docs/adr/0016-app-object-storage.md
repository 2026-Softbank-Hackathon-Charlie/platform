# 0016. 앱 인스턴스마다 플랫폼이 버킷과 그 버킷 전용 키를 만들고, 키는 Sealed Secrets로 gitops에 넣는다

- 상태: 채택, 구현됨(AWS·MinIO 2026-11-04, GCS 코드 2026-11-05; GCS 실환경 검증은 별도)
- 날짜: 2026-11-03

## 상황

`platform.yaml`에 `type: s3` 리소스가 있는 앱(예: moimroom의 `photos`)은 `S3_ENDPOINT`, `S3_BUCKET`, `S3_REGION`, `S3_PATH_STYLE`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`를 받는다. 앱 차트는 버킷 이름을 `<bucketPrefix><namespace>-<resource>`로 정하고, 키는 네임스페이스의 Secret(`profile.s3.credentialsSecret`, eks 프로필은 `app-s3`)에서 읽는다. 그런데 ADR 0011부터 버킷과 그 Secret을 누가 만들지 정하지 않아서, s3 리소스가 있는 앱은 실제 클러스터에서 뜨지 않는다. ADR 0015로 브랜치마다 인스턴스가 생기므로 인스턴스마다 버킷과 키가 필요하다.

플랫폼은 AWS(EKS), GCP(GKE), 온프렘에 앱을 배포하고 그 사이에서 이전한다. 환경마다 다른 방법을 쓰면 이전할 때마다 앱과 차트의 설정이 바뀐다. 그래서 모든 환경에서 하나의 방법을 쓴다.

## 결정

### 앱 계약은 그대로 둔다

- 앱은 모든 환경에서 위의 `S3_*` 환경 변수와 정적 키로 S3 호환 API를 쓴다. AWS S3, GCS의 S3 호환 API(HMAC 키), MinIO가 모두 이 계약으로 동작한다. moimroom은 바꾸지 않는다.

### 버킷

- 버킷은 인스턴스마다 하나이고 이름은 `<bucketPrefix><instance>`다. `platform.yaml`은 s3 리소스를 앱당 하나만 허용하므로 리소스 이름을 붙이지 않는다. 차트의 `S3_BUCKET`도 이 이름으로 바꾼다.
- `bucketPrefix`는 15자까지다(eks는 `hopcharlie-eks-`). 버킷 이름 상한이 63자이므로 인스턴스 이름 상한(`appspec.MaxInstanceName`)을 53자에서 48자로 줄인다.
- 플랫폼이 인스턴스의 첫 gitops 커밋 전에 버킷을 만든다. 공개 접근을 막고, 앱과 인스턴스를 태그로 붙인다.

### 키

- 플랫폼이 인스턴스마다 그 버킷 하나에만 쓸 수 있는 키를 만든다. 키를 만드는 방법만 환경마다 다르고, 환경 설정(`PLATFORM_ENVS_FILE`)의 저장소 드라이버가 정한다.

| 드라이버 | 환경 | 키를 만드는 방법 |
| --- | --- | --- |
| `aws` | EKS | IAM 사용자 `hopcharlie-app-<instance>`(경로 `/hopcharlie-apps/`, permissions boundary 적용), 그 버킷만 허용하는 인라인 정책, 액세스 키 |
| `gcs` | GKE | 서비스 계정(이름은 인스턴스 이름의 해시), 그 버킷에만 `roles/storage.objectAdmin`·`roles/storage.legacyBucketReader`, HMAC 키 |
| `minio` | 온프렘, kind E2E | MinIO 관리 API로 사용자와 그 버킷만 허용하는 정책 |

- 드라이버의 관리 자격 증명은 플랫폼만 가진다. AWS는 플랫폼 pod 역할, GCP는 ADC(Application Default Credentials), MinIO는 SSM에서 전달한 환경 변수를 쓴다. GCP 운영 설정은 `GOOGLE_APPLICATION_CREDENTIALS`가 가리키는 공개 WIF 설정과 projected pod token으로 인증하며 서비스 계정 개인키를 발급하지 않는다.
- `minio` 드라이버가 쓰는 MinIO 관리 클라이언트(madmin-go)는 AGPL-3.0이다. 그래서 이 드라이버는 빌드 태그 `minio`로 빌드한 API에만 들어간다(kind E2E, 온프렘). 기본 빌드와 게시하는 플랫폼 이미지에는 없고, 그 빌드는 `minio` 저장소 설정을 시작할 때 거절한다.
- 소유는 태그 `hopcharlie:app`으로 가린다. 다른 앱의 태그가 붙은 버킷과 사용자는 쓰지도 지우지도 않는다. IAM 사용자는 만드는 호출에서 태그가 붙으므로 태그가 없으면 플랫폼의 것이 아니다. 태그가 없는 버킷은 같은 계정이 방금 만든 경우에만 이어서 쓴다.
- 앱 이름으로 소유를 가리므로, 지운 앱이나 인스턴스의 버킷과 키를 지우는 중이면 같은 이름의 앱 등록과 인스턴스 추가를 409 `storage_cleanup_pending`으로 거절한다.
- 사용자 이름은 인스턴스 이름만으로 정한다. 그래서 두 환경이 같은 AWS 계정이나 같은 MinIO 서버에 사용자를 만들면 이름이 같은 인스턴스가 사용자를 나눠 쓰게 된다. `aws` 드라이버는 한 환경만 쓸 수 있고 `minio` 드라이버는 환경마다 엔드포인트가 달라야 하며, 어기면 서버가 환경 설정을 읽을 때 거절한다.

### 키 전달: Sealed Secrets

- 플랫폼은 만든 키를 대상 클러스터의 Sealed Secrets 공개 인증서로 암호화한다. 범위는 strict(네임스페이스 = 인스턴스, 이름 = `app-s3`)라서 다른 네임스페이스에서는 복호화되지 않는다.
- 암호문은 gitops 파일의 `values`에 들어가고, 앱 차트가 SealedSecret `app-s3`로 렌더링한다. 각 클러스터의 sealed-secrets 컨트롤러가 그 네임스페이스에 Secret을 만든다.
- 평문 키는 Git, 플랫폼 DB, 로그에 남기지 않는다. 플랫폼은 암호문과 키 식별자(삭제용)만 인스턴스와 환경별로 저장하고, 배포할 때마다 같은 암호문을 gitops 파일에 다시 쓴다.
- 플랫폼은 어느 클러스터에도 쓰기 권한이 필요 없다. 클러스터마다 공개 인증서만 알면 된다.

### 수명

- 인스턴스나 앱을 지우면 gitops 삭제 커밋으로 네임스페이스와 Secret이 사라진다. 그 뒤 플랫폼이 드라이버로 키(와 사용자 또는 서비스 계정)를 지우고, 버킷을 비운 뒤 삭제한다. DB가 네임스페이스와 함께 지워지는 것과 같은 수명이다.
- 앱을 다른 환경으로 이전하면 플랫폼이 대상 환경의 드라이버로 버킷과 키를 만들고, 대상 클러스터의 인증서로 암호화한다. 버킷 데이터의 복사는 지금처럼 이전 엔진이 맡는다.

### 구현

- 드라이버는 `internal/storage`의 `aws`(S3, IAM 사용자, 인라인 정책 `hopcharlie-bucket`, 액세스 키), `minio`(버킷, 사용자, 같은 이름의 canned policy, 서비스 계정), `gcs`(GCS 버킷, 서비스 계정, HMAC 키)다. `minio`는 빌드 태그 `minio`가 있어야 들어간다.
- `gcs` 설정은 `bucketPrefix`, 단일 리전 `region`(예: `asia-northeast3`), 프로젝트 ID `project`를 요구한다. 기존 `golang.org/x/oauth2/google` ADC와 공식 Storage·IAM REST API를 사용한다. 서비스 계정 이름 충돌을 막기 위해 한 GCP 프로젝트는 한 환경에서만 쓴다. 버킷은 생성할 때 소유 라벨(`hopcharlie-app`, `hopcharlie-instance`), 균일 버킷 수준 접근, 공개 접근 방지를 함께 설정한다. 기존 버킷은 프로젝트·소유 라벨·리전을 확인하고, 라벨이 없는 기존 버킷은 인수하지 않는다. 버킷 IAM 변경은 기존 바인딩과 조건·etag를 보존한다.
- 앱 계정의 `objectAdmin`은 객체 접근용이고, 같은 버킷의 `legacyBucketReader`는 S3 호환 클라이언트의 버킷 위치·정보 조회용이다. 플랫폼 관리 계정에는 대상 프로젝트의 버킷 목록·생성·조회·수정·IAM·객체 삭제, 서비스 계정 생성·조회·삭제, HMAC 키 관리 권한이 필요하다. 이 권한 연결과 projected token·WIF 설정은 infra가 담당한다.
- GCS HMAC은 access ID만 저장하고 비밀값은 생성 응답에서만 받아 기존 봉인 경로로 전달한다. 키 삭제는 INACTIVE 전환 뒤 수행한다. 생성 키 사용까지 최대 60초, 비활성화 전파는 최대 3분 걸릴 수 있어 실패는 재실행할 수 있게 남긴다. `iam.disableServiceAccountKeyCreation`·`storage.restrictAuthTypes` 정책이 막으면 오류로 끝내며 정책을 자동 변경하지 않는다. GCS soft delete·보존 정책은 기본값 또는 기존 값을 유지하므로 앱 삭제 후에도 보존 비용이 남을 수 있다.
- 배포 실행기가 커밋하기 전에 인스턴스 잠금 안에서 환경마다 버킷과 키를 확인한다. 저장한 키가 없으면 새로 만들어 봉인하고, 있으면 그 키가 아직 있는지 확인해 저장한 암호문을 다시 쓴다(사라졌으면 새 키). 저장한 행을 만든 드라이버가 그 환경의 지금 드라이버와 다르면 새 키를 만들지 않고 배포를 실패시킨다.
- 인증서 지문이 바뀌었으면 새 키를 만들어 다시 봉인하고, 바뀌기 전 키를 행의 `old_key_id`에 적는다. 돌고 있는 파드가 옛 키를 쓸 수 있으므로 커밋할 때는 지우지 않는다. 시크릿이 있는 앱은 `appChecksum`에 키 id가 들어가 키가 바뀌면 파드가 다시 뜬다. 그래서 그 뒤 healthy가 된 배포가 무엇이든 옛 키를 지우고 `old_key_id`를 비운다.
- 사용자당 키는 둘까지다(AWS 한도이고, `minio`도 같게 둔다). `old_key_id`가 남아 있을 때 인증서가 또 바뀌면 지금 키와 옛 키를 모두 지켜야 하므로, 새 키를 만들지 않고 배포를 실패시킨다.
- 배포 이벤트에는 키 id의 마지막 4자만 보인다.
- 인스턴스와 앱을 지운 뒤 실행기가 드라이버로 사용자, 키, 버킷을 지우고 행을 지운다. 실패하면 행을 남기고 서버 시작 때와 5분마다 다시 한다.
- 환경이 설정에서 빠졌거나 드라이버가 바뀐 행은 플랫폼이 지울 수 없다. 실행기는 시도할 때마다 error 로그 `cannot delete the bucket and key of a deleted instance`를 instance, env, driver, bucket과 함께 남긴다. 운영자는 옛 드라이버의 도구로 버킷, 사용자, 키를 지운 뒤(AWS는 `aws s3 rb s3://<bucket> --force`, `aws iam delete-access-key`, `aws iam delete-user-policy --policy-name hopcharlie-bucket`, `aws iam delete-user`, MinIO는 `mc rb --force <alias>/<bucket>`, `mc admin user rm <alias> hopcharlie-app-<instance>`, `mc admin policy rm <alias> hopcharlie-app-<instance>`) 행을 지운다: `DELETE FROM instance_storage WHERE instance = '<instance>' AND env = '<env>';`. 살아 있는 인스턴스의 환경 드라이버를 바꾼 경우도 같은 방법으로 옛 버킷과 행을 지우면(데이터가 필요하면 먼저 복사한다) 다음 배포가 새 드라이버로 버킷과 키를 만든다.
- 버킷 이름은 차트가 `profile.s3.bucketPrefix`로 만들므로 각 환경의 프로필과 `storage.bucketPrefix`를 같게 둔다. kind E2E는 sealed-secrets 컨트롤러(chart 2.20.0)와 `minio` 드라이버로 버킷, 키 격리, 삭제까지 확인한다.

## 검토한 대안

- EKS Pod Identity(네임스페이스 세션 태그로 버킷 하나만 허용): 정적 키가 없어지지만 AWS에서만 동작한다. GKE는 Workload Identity로 방법이 다르고, GCS의 S3 호환 API는 결국 HMAC 키가 필요하다.
- 플랫폼이 각 클러스터에 Secret을 직접 쓰기: 모든 클러스터에 쓰기 권한이 필요하다. GKE의 Argo CD 권한은 `charlie-apps` 네임스페이스로 제한되어 있고, 네임스페이스가 생기기 전에는 Secret을 넣을 수 없다.
- External Secrets로 중앙 저장소(SSM)에서 받기: 전달 경로는 같지만 GKE와 온프렘이 AWS SSM을 읽을 자격 증명을 따로 가져야 한다.
- 모든 클러스터에 MinIO를 띄워 저장소 자체를 통일하기: AWS에서도 S3 대신 EBS 위의 MinIO를 쓰게 되고, 노드 2대에 부담이 크다.
- 공용 버킷 하나에 인스턴스별 접두사: 버킷 생성은 없어지지만 모든 앱이 접두사(`S3_PREFIX`)를 알아야 해서 앱 계약이 바뀐다.
- Terraform으로 앱마다 버킷 만들기: 앱이나 브랜치를 추가할 때 infra PR이 필요하다.

## 결과

- infra는 한 번만 바꾼다.
  - 각 클러스터의 sealed-secrets 컨트롤러 동기화(chart 2.20.0, `kube-system`)와 공개 인증서를 플랫폼이 읽을 위치(SSM)
  - 플랫폼 pod 역할: `hopcharlie-eks-*` 버킷 관리, `/hopcharlie-apps/` 경로의 IAM 사용자와 액세스 키 관리, 그 사용자에게 붙일 permissions boundary
  - GCP 플랫폼 서비스 계정: 프로젝트 안의 버킷, 서비스 계정, HMAC 키 관리
  - 앱 차트의 SealedSecret 템플릿과 values 스키마, AppProject가 SealedSecret을 허용
- 이후 앱이나 브랜치를 더해도 infra 변경은 없다.
- 수명이 긴 키를 쓴다. 키마다 버킷 하나로 범위가 좁고 인스턴스와 함께 지워진다. 교체는 새 키를 만들어 다시 암호화해 커밋하고 이전 키를 지우는 방법으로 나중에 더한다.
- sealed-secrets 컨트롤러가 각 클러스터에 떠 있어야 한다. 컨트롤러 키를 잃으면 암호문을 다시 만들어야 하므로, 플랫폼은 키 식별자로 키를 새로 만들어 다시 커밋할 수 있어야 한다.
- 인스턴스 이름이 48자를 넘는 브랜치는 이름이 잘리고 해시가 붙는다(ADR 0015의 규칙과 같다).
- kind E2E는 sealed-secrets와 MinIO 드라이버로 온프렘 경로를 그대로 검증한다.
- 알려진 한계: 시크릿이 없는 앱은 키가 바뀌어도 파드가 다시 뜨지 않으므로 옛 키가 `old_key_id`에 남는다. 앱에 시크릿을 하나 넣고 다시 배포하거나 재배포하면 healthy가 된 뒤 지워진다. 그 전에 인증서가 또 바뀌면 배포가 실패하고, 운영자가 파드를 다시 띄운 뒤(`kubectl -n <instance> rollout restart deployment`) 옛 키를 지우면(`aws iam delete-access-key --user-name hopcharlie-app-<instance> --access-key-id <old_key_id>`, MinIO는 `mc admin user svcacct rm <alias> <old_key_id>`) 다음 배포가 이어서 교체한다. 컨트롤러의 `keyrenewperiod`가 0이라 인증서는 컨트롤러 키를 잃었을 때만 바뀌므로 드물다.
