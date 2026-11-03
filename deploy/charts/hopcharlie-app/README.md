# hopcharlie-app

HopCharlie 플랫폼이 GitHub에서 배포하는 앱 하나를 그리는 차트다(ADR 0011). Argo CD의 ApplicationSet(`deploy/argocd`)이 gitops 파일 `apps/<instance>/<env>.yaml`마다 이 차트로 Application `<namespace>-<env>`를 만든다.

## 값

- **gitops 파일 그대로**: Argo CD가 파일을 values 파일로 넘긴다. 차트는 `values.app`, `values.services[]`, `values.resources[]`를 그리고, `commit`과 `deployment`는 Deployment annotation에만 쓴다. `values.schema.json`이 `pkg/appspec`과 같은 규칙과 모르는 키를 거부한다. 이미지는 digest로 고정해야 한다.
- **`profile`**: 환경 프로필. ApplicationSet이 환경마다 `valuesObject`로 더한다. 항목과 기본값은 `values.yaml`에 있다.

## 그리는 것

| 객체 | 이름 | 조건 |
| --- | --- | --- |
| Namespace | 릴리스 네임스페이스(파일의 `namespace`) | 항상. Pod Security 라벨(`profile.podSecurity`), `hopcharlie.dev/app`, `hopcharlie.dev/instance`, sync-wave -1 |
| Deployment, Service | `<app>-<service>` | 서비스마다. 공개 서비스만 `profile.publicService`의 type과 annotation을 씀 |
| CNPG Cluster | `<app>-<resource>` | `postgres` 리소스. `DATABASE_URL`은 secret `<app>-<resource>-app`의 `uri` |
| (없음) | | `s3` 리소스는 `S3_*` 변수만 넣는다. 버킷 `<bucketPrefix><namespace>-<resource>`와 키 Secret `profile.s3.credentialsSecret`은 미리 있어야 한다 |
| ResourceQuota, LimitRange, NetworkPolicy | `hopcharlie-app` | `profile.quota`, `limitRange`, `networkPolicy`의 `enabled` |

새 버전이 준비되지 않아도 옛 파드가 계속 받는다. Deployment는 `maxUnavailable: 0`, `maxSurge: 1`이고, `progressDeadlineSeconds`가 지나면 Argo CD가 `Degraded`로 보여 준다.

## 앱을 지우면 네임스페이스째 지워진다

네임스페이스가 Application의 객체이므로, gitops 파일이 지워져 Application이 지워지면 Argo CD가 네임스페이스와 그 안의 모든 것을 지운다. CNPG의 데이터(PVC)도, 누가 넣어 둔 Secret(예: S3 키)도 함께 사라지고 되돌릴 수 없다. ADR 0011의 "앱을 지우면 DB의 데이터도 함께 사라진다"와 같다. 이미 있던 네임스페이스에 배포하면 Argo CD가 그 네임스페이스를 가져가므로, 지울 때 그 안의 것도 함께 지워진다. AppProject가 시스템과 플랫폼 네임스페이스를 막으므로 앱이 `kube-system`, `argocd` 같은 네임스페이스를 가져갈 수는 없다.

## 테스트

`go test ./test/charts`(helm lint, helm template)와 `test/e2e/deploy/run.sh`(kind의 Argo CD).
