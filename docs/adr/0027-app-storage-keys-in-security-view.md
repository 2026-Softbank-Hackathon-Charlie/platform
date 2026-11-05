# 0027. 앱 저장소 키는 모든 환경에서 정적 키로 두고, 보안 화면에 키마다 만든 시각과 허용 범위를 보여 준다

- 상태: 채택
- 날짜: 2026-11-05

## 상황

ADR 0016에 따라 플랫폼은 s3 리소스가 있는 앱 인스턴스마다 버킷 하나와 그 버킷만 쓰는 정적 키를 만든다. EKS에서는 IAM 사용자와 오래 사는 액세스 키다. 수명이 긴 키는 새면 지울 때까지 유효하므로, EKS에서는 EKS Pod Identity(또는 IRSA)로 바꾸자는 의견이 나왔다.

그런데 보안 화면(콘솔의 보안 페이지, `GET /v1/security`)은 이 키들을 따로 보여 주지 않았다. AWS 계정 수집기가 90일 넘은 액세스 키를 `aws.access-key-age`로 알려 주는 것이 전부였고, 어느 앱의 키인지, 언제 만들었는지, 무엇을 할 수 있는지는 화면에서 알 수 없었다. GKE의 HMAC 키와 온프렘의 MinIO 키는 아예 보이지 않았다.

## 결정

### 키는 지금처럼 둔다

EKS도 Pod Identity로 바꾸지 않고 인스턴스마다 정적 키를 쓴다.

- 세 환경 모두에서 같은 방법으로 동작하는 것은 정적 키뿐이다. GKE에서 앱은 GCS의 S3 호환 API를 쓰는데, 이 API는 HMAC 키로만 인증한다. 온프렘은 MinIO의 서비스 계정 키를 쓴다. 둘 다 Pod Identity에 해당하는 것이 없다.
- EKS만 바꾸면 앱 설정이 환경마다 달라진다. EKS에서는 `S3_ACCESS_KEY`, `S3_SECRET_KEY`와 Secret `app-s3` 없이 SDK의 기본 자격 증명 체인을 쓰고, GKE와 온프렘에서는 키를 쓰게 된다. 앱을 EKS에서 GKE로 이전하면 이전과 함께 인증 방식과 차트 값이 바뀌고, 앱 코드도 두 방식을 다 지원해야 한다. ADR 0016이 앱 계약(`S3_*` 환경 변수와 정적 키)을 하나로 둔 이유와 같다.

### 이미 있는 완화

코드와 실환경에서 확인했다.

- 키 하나는 버킷 하나만 쓴다. `aws` 드라이버는 사용자마다 인라인 정책 `hopcharlie-bucket`을 두고, 그 버킷에 `s3:ListBucket`, `s3:GetBucketLocation`, `s3:ListBucketMultipartUploads`, 그 버킷의 객체에 `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, `s3:AbortMultipartUpload`, `s3:ListMultipartUploadParts`만 허용한다(`internal/storage` `bucketPolicy`). 배포할 때마다 `PutUserPolicy`로 다시 쓴다. `minio` 드라이버는 같은 문서를 canned policy로 쓰고, `gcs` 드라이버는 서비스 계정에 그 버킷의 `roles/storage.objectAdmin`과 `roles/storage.legacyBucketReader`만 준다.
- permissions boundary가 정책을 덮는다. infra의 `hopcharlie-app-boundary`는 이 계정의 `hopcharlie-eks-*` 버킷 목록 작업과 객체 작업만 허용하고, 버킷 설정(정책, ACL, 공개 차단, 삭제)은 허용하지 않는다. 플랫폼 pod 역할은 이 boundary를 붙일 때만 `/hopcharlie-apps/` 사용자를 만들고 정책을 쓸 수 있다(`iam:PermissionsBoundary` 조건). 그래서 인라인 정책이 잘못 써져도 앱 키는 `hopcharlie-eks-*`가 아닌 버킷, 다른 계정의 버킷, 버킷 설정에 닿지 않는다. 다른 앱의 버킷을 막는 것은 인라인 정책이다.
- 새 키를 만들고 옛 키를 지우는 경로가 있다(ADR 0016). sealed-secrets 인증서가 바뀌면 새 키를 만들고 옛 키를 `old_key_id`에 적었다가 healthy 배포 뒤에 지운다. 키가 밖에서 지워졌으면 다음 배포가 새 키를 만든다. 새 키를 만들기 전에 기록에 없는 키를 지우고, 사용자당 키는 둘까지다. 인스턴스나 앱을 지우면 사용자, 키, 버킷을 지우고 실패하면 5분마다 다시 한다.
- 실환경(2026-11-05, 읽기 전용 IAM 조회): `/hopcharlie-apps/` 사용자는 `hopcharlie-app-moimroom` 하나다. boundary는 `hopcharlie-app-boundary`, 인라인 정책은 `hopcharlie-bucket` 하나이고 `hopcharlie-eks-moimroom`과 그 객체만 허용한다. 활성 액세스 키가 하나이고 2026-11-04 08:21:52 UTC에 만들어졌다.

### 보안 화면에 키 목록을 더한다

- `GET /v1/security`의 응답에 `storageKeys`를 더한다. 키마다 앱, 인스턴스, 환경, 드라이버, 버킷, 키 id의 끝 4자, 쓰임(`current` 지금 쓰는 키, `old` 교체 뒤 파드가 아직 쓸 수 있는 옛 키, `unrecorded` 플랫폼 기록에 없는 키), 상태(`ok`, `missing`, `unread`), 만든 시각(`createdAt`), 허용 범위(`scope`의 `actions`와 `boundary`), 안내 문구를 담는다. 키 id는 배포 이벤트처럼 끝 4자만 보인다.
- 어떤 키가 있어야 하는지는 플랫폼 DB(`instance_storage`의 `key_id`, `old_key_id`)에서 읽는다.
- 만든 시각은 제공자에서 읽는다. `aws`는 `ListAccessKeys`의 `CreateDate`, `gcs`는 HMAC 키 목록의 `timeCreated`다. `minio`는 만든 시각을 기록하지 않아서 비워 둔다. DB의 `created_at`은 행을 처음 만든 시각이고 키를 바꿔도 그대로이므로 키의 나이로 쓰지 않는다. 제공자를 읽으면 기록한 키가 실제로 있는지(`missing`)와 기록에 없는 키(`unrecorded`)도 함께 알 수 있다.
- 허용 범위는 드라이버가 배포마다 쓰는 것을 보여 준다(`aws`와 `minio`는 위 S3 작업, `gcs`는 두 역할, `aws`는 boundary ARN도). 같은 목록을 정책을 만드는 코드와 함께 쓴다. 제공자에서 정책을 다시 읽지는 않는다. 손으로 바꾼 정책은 다음 배포가 덮어쓰고, 그 사이에도 boundary를 넘지 못한다.
- 저장소 드라이버가 있는 환경마다 출처 `storage-keys`를 하나 둔다. 다른 출처처럼 60초 캐시와 25초 제한 시간을 따르고, 읽기만 하며, finding은 만들지 않는다(90일 넘은 키는 `aws-iam`이 이미 알려 준다). `environment` 필터는 키에도 적용되고 `severity` 필터는 적용되지 않는다.
- 읽지 못한 경우를 숨기지 않는다. 키는 상태 `unread`와 이유로 나오고 그 환경의 출처는 `error`가 된다. 드라이버를 만들지 못한 환경, 행을 만든 드라이버와 지금 드라이버가 다른 경우, 설정에서 빠진 환경의 행(그 환경 이름으로 `error` 출처를 따로 만든다), 제공자가 거부한 경우(AWS 오류 코드만 보이고 메시지는 내보내지 않는다)가 그렇다. DB를 읽지 못하면 모든 `storage-keys` 출처가 `error`다.
- 권한은 더하지 않는다. 플랫폼 pod 역할에는 `iam:ListAccessKeys`가 이미 있다(앱 사용자에 대한 `app-storage` 정책과 보안 수집기의 읽기 정책). GCP 플랫폼 서비스 계정에는 `storage.hmacKeys.list`가 이미 있다. infra 변경은 없다.
- 콘솔 보안 페이지에 "앱 저장소 키" 표를 더한다. 응답에 `storageKeys`가 없으면(이 결정 전의 플랫폼) 플랫폼이 아직 키를 보고하지 않는다고 안내하고, 비어 있으면 키가 없다고 보여 준다.

## 검토한 대안

- EKS Pod Identity 또는 IRSA(네임스페이스 세션 태그로 버킷 하나만 허용): EKS에서는 정적 키가 없어지지만 위의 이유로 환경마다 앱 설정이 달라진다. ADR 0016에서도 같은 이유로 고르지 않았다.
- 키를 정해진 주기로 자동 교체하기: 새 키를 만들고 옛 키를 지우는 경로는 있지만, 앱 시크릿이 없는 앱은 키가 바뀌어도 파드가 다시 뜨지 않아 옛 키를 지울 수 없다(ADR 0016의 알려진 한계). 이 한계를 먼저 풀어야 하므로 이번에는 나이를 보이는 데서 멈춘다.
- DB에 키를 만든 시각 열을 더하기: 마이그레이션이 필요하고, 이미 있는 행은 교체가 있었는지 몰라 정확한 값을 채울 수 없다. 제공자의 값이 정답이고 키가 실제로 있는지도 함께 확인된다.
- 제공자에서 정책을 다시 읽어 범위로 보이기: `aws`는 `iam:GetUserPolicy` 권한을 infra에 더해야 하고, 드라이버마다 정책을 해석하는 코드가 필요하다. 배포마다 덮어쓰는 정책과 boundary로 충분하다고 보고 나중으로 미룬다.
- 키를 finding으로 보이기: 키는 문제가 아니라 목록이다. 문제(오래된 키)는 `aws-iam` 수집기가 이미 finding으로 낸다.

## 결과

- 보안 화면에서 앱 키마다 어느 버킷에 무엇을 할 수 있는지, 언제 만들었는지, 기록과 실제가 맞는지 볼 수 있다. EKS, GKE, 온프렘이 같은 표에 나온다.
- 수집할 때마다 `instance_storage` 행마다 제공자 목록 호출이 한 번 늘어난다(60초 캐시).
- GKE의 `timeCreated`는 fake 서버 테스트로만 확인했고 실환경 GCS에서는 아직 확인하지 않았다. MinIO 키는 만든 시각 없이 나온다.
- 병합 순서는 platform 먼저, 그다음 콘솔이다. 콘솔이 먼저 나가도 `storageKeys`가 없다는 안내만 보인다.
