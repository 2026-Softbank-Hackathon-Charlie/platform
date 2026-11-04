# 0016. 앱 인스턴스마다 플랫폼이 버킷과 그 버킷 전용 키를 만들고, 키는 Sealed Secrets로 gitops에 넣는다

- 상태: 채택
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
| `gcs` | GKE | 서비스 계정(이름은 인스턴스 이름의 해시), 그 버킷의 `roles/storage.objectAdmin`, HMAC 키 |
| `minio` | 온프렘, kind E2E | MinIO 관리 API로 사용자와 그 버킷만 허용하는 정책 |

- 드라이버의 관리 자격 증명은 플랫폼만 가진다. AWS는 플랫폼 pod 역할을 쓰고, GCP와 MinIO는 SSM에 둔 값을 쓴다.
- `minio` 드라이버가 쓰는 MinIO 관리 클라이언트(madmin-go)는 AGPL-3.0이다. 그래서 이 드라이버는 빌드 태그 `minio`로 빌드한 API에만 들어간다(kind E2E, 온프렘). 기본 빌드와 게시하는 플랫폼 이미지에는 없고, 그 빌드는 `minio` 저장소 설정을 시작할 때 거절한다.
- 소유는 태그 `hopcharlie:app`으로 가린다. 다른 앱의 태그가 붙은 버킷과 사용자는 쓰지도 지우지도 않는다. IAM 사용자는 만드는 호출에서 태그가 붙으므로 태그가 없으면 플랫폼의 것이 아니다. 태그가 없는 버킷은 같은 계정이 방금 만든 경우에만 이어서 쓴다.
- 앱 이름으로 소유를 가리므로, 지운 앱이나 인스턴스의 버킷과 키를 지우는 중이면 같은 이름의 앱 등록과 인스턴스 추가를 409 `storage_cleanup_pending`으로 거절한다.

### 키 전달: Sealed Secrets

- 플랫폼은 만든 키를 대상 클러스터의 Sealed Secrets 공개 인증서로 암호화한다. 범위는 strict(네임스페이스 = 인스턴스, 이름 = `app-s3`)라서 다른 네임스페이스에서는 복호화되지 않는다.
- 암호문은 gitops 파일의 `values`에 들어가고, 앱 차트가 SealedSecret `app-s3`로 렌더링한다. 각 클러스터의 sealed-secrets 컨트롤러가 그 네임스페이스에 Secret을 만든다.
- 평문 키는 Git, 플랫폼 DB, 로그에 남기지 않는다. 플랫폼은 암호문과 키 식별자(삭제용)만 인스턴스와 환경별로 저장하고, 배포할 때마다 같은 암호문을 gitops 파일에 다시 쓴다.
- 플랫폼은 어느 클러스터에도 쓰기 권한이 필요 없다. 클러스터마다 공개 인증서만 알면 된다.

### 수명

- 인스턴스나 앱을 지우면 gitops 삭제 커밋으로 네임스페이스와 Secret이 사라진다. 그 뒤 플랫폼이 드라이버로 키(와 사용자 또는 서비스 계정)를 지우고, 버킷을 비운 뒤 삭제한다. DB가 네임스페이스와 함께 지워지는 것과 같은 수명이다.
- 앱을 다른 환경으로 이전하면 플랫폼이 대상 환경의 드라이버로 버킷과 키를 만들고, 대상 클러스터의 인증서로 암호화한다. 버킷 데이터의 복사는 지금처럼 이전 엔진이 맡는다.

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
