# 0019. 앱 시크릿은 플랫폼이 암호화해 보관하고 배포할 때 Sealed Secrets로 넣으며, 재배포는 빌드 없이 한다

- 상태: 채택
- 날짜: 2026-11-04

## 상황

- moimroom의 `HOST_CODE`, `ADMIN_TOKEN`처럼 비밀 환경 변수가 필요한 앱이 있다. `platform.yaml`은 앱 레포(moimroom은 공개 레포)에 있으므로 여기에 넣을 수 없다. ADR 0011은 시크릿 주입을 나중으로 미뤘다.
- 운영자는 콘솔에서 시크릿을 넣고, 고치고, 눈 버튼으로 다시 볼 수 있어야 한다. 바꾼 값은 빌드를 다시 하지 않고 재배포로 반영해야 한다.
- ADR 0016으로 플랫폼이 만든 S3 키를 Sealed Secrets로 gitops 파일에 넣는 경로가 생긴다. 시크릿도 같은 경로를 쓴다.

## 결정

### 범위

- 시크릿은 앱 단위로 둔다. 기본 인스턴스와 브랜치 인스턴스(ADR 0015)가 모두 받는다.
- 인스턴스마다 덮어쓸 수 있다. 인스턴스가 받는 값은 앱 단위 시크릿 위에 그 인스턴스의 덮어쓰기를 얹은 것이다. 덮어쓰기를 지우면 앱 단위 값으로 돌아간다.
- 이름은 `^[A-Z_][A-Z0-9_]*$`, 64자까지이고 `appspec.Reserved`(PORT, DATABASE_URL, S3_*)는 쓸 수 없다. 값은 4096바이트까지, 앱 단위와 인스턴스마다 50개까지다.
- `platform.yaml`의 env와 이름이 같으면 시크릿이 이긴다. 플랫폼이 gitops 파일을 쓸 때 그 서비스의 env에서 같은 이름을 뺀다.

### API

- `GET /v1/apps/{name}/secrets`(viewer): 이름, 바꾼 시각, 바꾼 사람만 돌려준다.
- `PUT /v1/apps/{name}/secrets`(operator): `instance`가 없으면 앱 단위, 있으면 그 인스턴스의 덮어쓰기를 `set`, `delete`로 한 번에 바꾼다. 감사 로그에는 이름만 남는다.
- `GET /v1/apps/{name}/secrets/{secret}?instance=`(operator): 값 하나를 돌려준다. 콘솔의 눈 버튼이 부른다. GET이지만 부를 때마다 감사 로그에 남기고 `Cache-Control: no-store`를 붙인다.
- agent나 viewer 역할의 토큰(예: Rin에게 준 CLI 토큰)은 이름만 볼 수 있고 값은 볼 수 없다.

### 보관

- 플랫폼 DB에 앱, 인스턴스(앱 단위는 빈 문자열), 이름마다 한 행을 두고, 값은 AES-256-GCM 암호문과 nonce로 저장한다. AAD는 `<app>/<instance>/<name>`이라서 다른 행으로 옮겨 붙이면 복호화되지 않는다. 키 버전 컬럼을 두어 나중에 키를 바꿀 수 있게 한다.
- 키는 32바이트이고 SSM `/hopcharlie/app-secrets-key`(Terraform이 만든다)에서 API의 `PLATFORM_SECRETS_KEY`(base64)로 받는다. 키가 없으면 시크릿 API는 503 `secrets_not_configured`이고, 시크릿이 없는 앱의 배포는 영향을 받지 않는다.
- ADR 0016과 다른 점: 플랫폼이 만든 S3 키는 다시 만들 수 있으므로 봉인한 암호문만 둔다. 사람이 넣은 시크릿은 다시 보여 주고 새 인스턴스와 새 환경에도 넣어야 하므로, 플랫폼이 평문을 되살릴 수 있게 보관한다.

### 전달

- 배포할 때 인스턴스와 환경마다 받는 시크릿을 그 환경 클러스터의 Sealed Secrets 공개 인증서로 봉인한다. 범위는 strict(네임스페이스 = 인스턴스, 이름 = `app-secrets`)다.
- 앱 차트가 SealedSecret `app-secrets`를 만들고, 모든 서비스 컨테이너에 `envFrom`으로 넣는다.
- 봉인은 할 때마다 결과가 달라진다. 그래서 파드가 바뀐 값을 받도록, 받는 시크릿 전체(이름과 값)의 HMAC-SHA256을 `appChecksum`으로 넣고 차트가 파드 템플릿 annotation에 쓴다. HMAC 키는 시크릿 키에서 파생한다. 값이 같으면 checksum도 같아서 파드가 다시 뜨지 않는다.
- gitops 파일의 `values`에 다음을 더한다. ADR 0016의 S3 키도 여기에 들어간다.

```yaml
values:
  sealedSecrets:            # 넣을 것이 없으면 키 자체가 없다
    s3:                     # s3 리소스가 있을 때(ADR 0016). SealedSecret app-s3
      accessKey: <암호문>
      secretKey: <암호문>
    app:                    # 받는 시크릿이 있을 때. SealedSecret app-secrets
      HOST_CODE: <암호문>
    appChecksum: <hex>      # app이 있을 때
```

### 환경 설정

ADR 0016의 저장소 드라이버와 Sealed Secrets 인증서는 `PLATFORM_ENVS_FILE`의 환경마다 둔다.

```yaml
environments:
  - name: eks
    # ...기존 필드
    sealedSecretsCert: /config/sealed-secrets/eks.pem   # 이 클러스터 컨트롤러의 공개 인증서
    storage:
      driver: aws                    # aws | minio
      bucketPrefix: hopcharlie-eks-  # 15자까지
      region: ap-northeast-2
      iamPath: /hopcharlie-apps/
      permissionsBoundary: arn:aws:iam::<계정>:policy/hopcharlie-app-boundary
    # minio 드라이버: endpoint, region, adminAccessKeyEnv, adminSecretKeyEnv(관리 키를 담은 환경 변수 이름)
```

- `sealedSecretsCert`가 없는 환경에는 시크릿이나 s3 리소스가 있는 앱을 배포할 수 없다. `storage`가 없는 환경에는 s3 리소스가 있는 앱을 등록할 수 없다(503 `storage_not_configured`).

### 재배포

- `POST /v1/apps/{name}/deployments`에 `redeploy: true`를 주면 그 인스턴스의 마지막 healthy 배포를 같은 커밋과 같은 이미지로 다시 배포한다. 빌드와 GitHub 읽기는 하지 않고, 지금의 시크릿으로 gitops 파일만 다시 쓴다. trigger는 `redeploy`다.
- healthy 배포가 없으면 409 `nothing_to_redeploy`다. `ref`와 함께 쓸 수 없다.
- 콘솔은 시크릿을 저장한 뒤 "재배포" 버튼으로 이 요청을 보낸다. CLI는 `charlie app secret list|get|set|rm`과 `charlie deploy APP --redeploy`를 쓴다.

## 검토한 대안

- 저장할 때 봉인만 하고 암호문만 두기(ADR 0016의 S3 키 방식): 값을 다시 보여 줄 수 없고, 새 브랜치 인스턴스나 새 환경에 넣으려면 다시 입력해야 한다.
- AWS Secrets Manager나 SSM에 두고 External Secrets로 받기: GKE와 온프렘이 AWS를 읽을 자격 증명을 가져야 한다(ADR 0016에서 제외한 이유와 같다).
- KMS로 키를 감싸기: 더 안전하지만 AWS 전용이고 플랫폼 pod 역할에 권한이 더 필요하다. 키 버전 컬럼이 있으므로 나중에 키 공급원만 바꿀 수 있다.
- 재배포할 때 브랜치 head를 다시 빌드하기: 구현은 쉽지만 시크릿 하나를 바꿀 때마다 CodeBuild로 1~3분이 걸린다.

## 결과

- infra는 SSM `/hopcharlie/app-secrets-key`를 만들고 API의 init 컨테이너가 `PLATFORM_SECRETS_KEY`로 넣는다. 차트의 SealedSecret 템플릿과 AppProject의 SealedSecret 허용은 ADR 0016과 같이 바꾼다.
- 시크릿 키를 잃으면 저장된 시크릿을 되살릴 수 없다. 키는 SSM과 Terraform state에만 있다.
- 값을 볼 수 있는 사람은 operator(콘솔 관리자와 operator CLI 토큰)뿐이고, 볼 때마다 감사 로그에 남는다.
