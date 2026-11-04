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
- 이름은 `^[A-Z_][A-Z0-9_]*$`, 64자까지이고 `appspec.Reserved`(PORT, DATABASE_URL, S3_*)는 쓸 수 없다. 값은 1바이트에서 4096바이트까지이고(빈 값은 넣을 수 없다), 앱 단위와 인스턴스마다 50개까지다.
- `platform.yaml`의 env와 이름이 같으면 시크릿이 이긴다. 플랫폼이 gitops 파일을 쓸 때 그 서비스의 env에서 같은 이름을 뺀다.

### API

- `GET /v1/apps/{name}/secrets`(viewer): 이름, 바꾼 시각, 바꾼 사람만 돌려준다.
- `PUT /v1/apps/{name}/secrets`(operator): `instance`가 없으면 앱 단위, 있으면 그 인스턴스의 덮어쓰기를 `set`, `delete`로 한 번에 바꾼다. 감사 로그에는 이름만 남는다.
- `GET /v1/apps/{name}/secrets/{secret}?instance=`(operator): 값 하나를 돌려준다. 콘솔의 눈 버튼이 부른다. GET이지만 부를 때마다 감사 로그에 남기고 `Cache-Control: no-store`를 붙인다. 감사 로그를 값보다 먼저 쓰고, 쓰지 못하면 값을 주지 않고 503 `audit_unavailable`로 답한다.
- agent나 viewer 역할의 토큰(예: Rin에게 준 CLI 토큰)은 이름만 볼 수 있고 값은 볼 수 없다.

### 보관

- 플랫폼 DB에 앱, 인스턴스(앱 단위는 빈 문자열), 이름마다 한 행을 두고, 값은 AES-256-GCM 암호문과 nonce로 저장한다. AAD는 앱, 인스턴스, 이름을 각각 길이와 함께 이어 붙인 값이라서 다른 행으로 옮겨 붙이면 복호화되지 않는다. 키 버전 컬럼을 두어 나중에 키를 바꿀 수 있게 하고, 키에서 HKDF로 만든 짧은 키 확인값을 행마다 저장한다. 다른 키로 띄운 서버는 시작할 때 그런 행이 있다고 로그에 남기고, 그 값을 읽으려 하면 키가 다르다고 분명히 실패한다.
- 키는 32바이트이고 SSM `/hopcharlie/app-secrets-key`(Terraform이 만든다)에서 API의 `PLATFORM_SECRETS_KEY`(base64)로 받는다. 키가 없으면 시크릿 API는 503 `secrets_not_configured`이고, 시크릿이 없는 앱의 배포는 영향을 받지 않는다.
- ADR 0016과 다른 점: 플랫폼이 만든 S3 키는 다시 만들 수 있으므로 봉인한 암호문만 둔다. 사람이 넣은 시크릿은 다시 보여 주고 새 인스턴스와 새 환경에도 넣어야 하므로, 플랫폼이 평문을 되살릴 수 있게 보관한다.

### 전달

- 배포할 때 인스턴스와 환경마다 받는 시크릿을 그 환경 클러스터의 Sealed Secrets 공개 인증서로 봉인한다. 범위는 strict(네임스페이스 = 인스턴스, 이름 = `app-secrets`)다.
- 앱 차트가 SealedSecret `app-secrets`를 만들고, 모든 서비스 컨테이너에 `envFrom`으로 넣는다.
- 봉인은 할 때마다 결과가 달라진다. 그래서 파드가 바뀐 값을 받도록, 앱, 인스턴스, 받는 시크릿 전체(이름과 값), 버킷 키 id의 HMAC-SHA256을 `appChecksum`으로 넣고 차트가 파드 템플릿 annotation에 쓴다. HMAC 키는 시크릿 키에서 파생한다. 값이 같으면 checksum도 같아서 파드가 다시 뜨지 않는다. 버킷 키 id가 들어가므로 키가 바뀌면 파드도 다시 뜬다(ADR 0016).
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
- 재배포는 그 healthy 배포가 간 환경에만 간다. 다른 환경은 다른 버전을 돌리고 있을 수 있기 때문이다. `environments`를 생략하면 그 배포의 환경 중 앱이 아직 배포하는 환경 모두에 가고, 그런 환경이 없으면 409 `nothing_to_redeploy`다. `environments`에 그 배포가 가지 않은 환경이 있으면 409 `nothing_to_redeploy`이고, 그 환경에는 브랜치를 배포한다.
- 그 인스턴스에 끝나지 않은 배포(queued, building, committing, syncing)가 있으면 409 `deployment_in_progress`다. 진행 중인 배포는 gitops 커밋 때의 시크릿을 이미 쓰므로, 끝난 뒤에 다시 재배포한다.
- 이 확인과 저장 사이에 다른 배포가 들어올 수 있으므로 실행기도 재배포를 다른 배포와 함께 돌리지 않는다. 재배포가 아닌 배포는 끝나지 않은 재배포를 만든 시각과 환경에 관계없이 모두 superseded로 끝낸다. 재배포는 큐에 넣을 때(서버 재시작 때 이어 받을 때도) 재배포가 아닌 끝나지 않은 배포가 있으면 스스로 superseded로 끝나고, 그 배포가 지금의 시크릿을 쓴다는 메시지를 남긴다. 두 배포 모두 저장한 뒤에 큐에 넣으므로 동시에 들어와도 적어도 한쪽은 다른 쪽을 본다. 그 배포가 가지 않는 환경은 그 배포가 끝난 뒤 다시 재배포한다.
- 배포는 읽은 `platform.yaml`을 기록하고, 재배포는 그 기록과 이미지를 복사한다. 그래서 GitHub을 읽지 않는다.
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
