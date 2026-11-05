# hopcharlie-app

HopCharlie 플랫폼이 GitHub에서 배포하는 앱 하나를 그리는 차트다(platform ADR 0011). Argo CD의 ApplicationSet(infra의 `helm/charts/hopcharlie-apps`, 이 저장소의 테스트용 사본 `deploy/argocd`)이 gitops 파일 `apps/<instance>/<env>.yaml`마다 이 차트로 Application `<namespace>-<env>`를 만든다.

## 값

- **gitops 파일 그대로**: Argo CD가 파일을 values 파일로 넘긴다. 차트는 `values.app`, `values.services[]`, `values.resources[]`, `values.sealedSecrets`를 그리고, `commit`과 `deployment`는 Deployment annotation에만 쓴다. `values.schema.json`이 platform 저장소의 `pkg/appspec`과 같은 규칙과 모르는 키를 거부한다. 이미지는 digest로 고정해야 한다.
- **`profile`**: 환경 프로필. ApplicationSet이 환경마다 `valuesObject`로 더한다. 항목과 기본값은 `values.yaml`에 있다.

## 그리는 것

| 객체 | 이름 | 조건 |
| --- | --- | --- |
| Namespace | 릴리스 네임스페이스(파일의 `namespace`) | 항상. Pod Security 라벨(`profile.podSecurity`), `hopcharlie.dev/app`, `hopcharlie.dev/instance`, sync-wave -1 |
| Deployment | `<app>-<service>` | 서비스마다. `values.sealedSecrets.app`이 있으면 모든 컨테이너가 `envFrom`으로 Secret `app-secrets`를 받고, 파드 annotation `hopcharlie.dev/secrets-checksum`에 `appChecksum`을 쓴다 |
| Service | `<app>-<service>` | `port`가 있는 서비스마다. 공개 서비스만 `profile.publicService`의 type, port(0이면 서비스의 포트), annotation을 씀 |
| CNPG Cluster | `<app>-<resource>` | `postgres` 리소스. 파드의 requests와 limits는 `profile.postgres.resources`. 서비스의 DB 변수는 아래 표 |
| (없음) | | `s3` 리소스는 `S3_*` 변수를 넣는다. 버킷은 `<bucketPrefix><namespace>`이고 플랫폼이 만든다(platform ADR 0016). 키는 Secret `profile.s3.credentialsSecret`에서 읽는다 |
| SealedSecret | `app-s3` | `values.sealedSecrets.s3`. 버킷 키(`accessKey`, `secretKey`). 이때 `profile.s3`는 Secret `app-s3`와 기본 키 이름을 읽어야 한다 |
| SealedSecret | `app-secrets` | `values.sealedSecrets.app`. 앱 시크릿(platform ADR 0019) |
| SealedSecret | `app-pgbackup` | `values.sealedSecrets.pgBackup`. 백업 버킷 키(`accessKey`, `secretKey`, platform ADR 0025) |
| ObjectStore, ScheduledBackup | `<app>-<resource>` | `postgres` 리소스, `values.backup`, `profile.postgres.backup.enabled`가 모두 있을 때. infra의 같은 차트 README 백업 절 |
| ResourceQuota, LimitRange, NetworkPolicy | `hopcharlie-app` | `profile.quota`, `limitRange`, `networkPolicy`의 `enabled` |

## 서비스 계약

플랫폼의 `pkg/appspec`이 `platform.yaml`을 검사한 뒤 gitops 파일을 쓴다. 규칙과 숫자의 근거는 platform ADR 0024에 있다. 이 차트는 그 결과를 그대로 그리고, `values.schema.json`은 모양(포트 범위, quantity 형식, 예약된 이름, 포트 없는 공개 서비스와 `healthCheck`)만 다시 거부한다. 범위와 합계는 플랫폼만 검사한다.

| 서비스 값 | 차트가 그리는 것 |
| --- | --- |
| `port` 있음 | `containerPort` `http`, Service, `PORT`. `healthCheck`가 있으면 HTTP startup, readiness, liveness probe, 없으면 TCP readiness probe |
| `port` 없음(워커) | Service, `containerPort`, probe, `PORT`가 없다. 컨테이너가 실행되면 준비된 것으로 보고, `minReadySeconds: 10`이라 10초 동안 재시작 없이 돌아야 새 버전으로 친다. 시작하자마자 죽는 워커는 옛 파드를 내리지 않는다. `public: true`와 `healthCheck`는 schema가 거부한다 |
| `memory` | memory request와 limit을 모두 이 값으로 쓴다 |
| `cpu` | CPU request를 이 값으로 쓴다. CPU limit은 없다 |
| 둘 다 없음 | `profile.resources`(기본 request cpu 50m, memory 64Mi, limit memory 256Mi) |

`postgres` 리소스가 있으면 모든 컨테이너가 Secret `<app>-<resource>-app`(CNPG가 만드는 app Secret)에서 다음 변수를 받는다. 모든 환경에서 `DATABASE_URL`과 같은 Secret이다.

| 변수 | Secret 키 |
| --- | --- |
| `DATABASE_URL` | `uri` |
| `PGHOST` | `host` |
| `PGPORT` | `port` |
| `PGDATABASE` | `dbname` |
| `PGUSER` | `username` |
| `PGPASSWORD` | `password` |
| `JDBC_DATABASE_URL` | `jdbc-uri` |

플랫폼 변수는 컨테이너 `env`에서 서비스의 `env`보다 앞에 온다. 그래서 서비스의 `env` 값에 `$(PGHOST)`처럼 쓰면 Kubernetes가 플랫폼 값으로 바꾼다. 앱이 읽는 이름이 다르면 `KC_DB_URL_HOST: "$(PGHOST)"`, `SPRING_DATASOURCE_URL: "$(JDBC_DATABASE_URL)"`, `AWS_ACCESS_KEY_ID: "$(S3_ACCESS_KEY)"`처럼 넘긴다. `envFrom`의 앱 시크릿도 `env`보다 먼저 정해지므로 같은 방법으로 넘길 수 있다. 차트는 `AWS_*`를 넣지 않는다. 파드 자격 증명으로 다른 AWS API를 부르는 앱의 SDK가 버킷 전용 키를 먼저 집어 들기 때문이다. `PGSSLMODE` 같은 다른 PG 변수는 앱이 정한다.

SealedSecret은 대상 클러스터의 Sealed Secrets 공개 인증서로 strict 범위로 봉인한 값이다. 이름과 네임스페이스가 다르면 열리지 않는다. sync-wave -1이라 Argo CD는 컨트롤러가 Secret을 만든 뒤에 Deployment를 바꾼다. Secret은 컨트롤러가 만들고 SealedSecret이 소유하므로 Argo CD가 지우지 않는다. 같은 이름이면 `env`가 `envFrom`보다 이긴다. 플랫폼이 시크릿과 같은 이름을 `env`에서 빼고 gitops 파일을 쓰며, 남아 있으면 차트가 렌더링을 실패시킨다.

새 버전이 준비되지 않아도 옛 파드가 계속 받는다. Deployment는 `maxUnavailable: 0`, `maxSurge: 1`이고, `progressDeadlineSeconds`가 지나면 Argo CD가 `Degraded`로 보여 준다.

## 앱을 지우면 네임스페이스째 지워진다

네임스페이스가 Application의 객체이므로, gitops 파일이 지워져 Application이 지워지면 Argo CD가 네임스페이스와 그 안의 모든 것을 지운다. CNPG의 데이터(PVC)도, 누가 넣어 둔 Secret(예: S3 키)도 함께 사라지고 되돌릴 수 없다. platform ADR 0011의 "앱을 지우면 DB의 데이터도 함께 사라진다"와 같다. 이미 있던 네임스페이스에 배포하면 Argo CD가 그 네임스페이스를 가져가므로, 지울 때 그 안의 것도 함께 지워진다. AppProject가 시스템과 플랫폼 네임스페이스를 막으므로 앱이 `kube-system`, `argocd` 같은 네임스페이스를 가져갈 수는 없다.

## 테스트

infra 저장소의 `helm/charts/hopcharlie-app`이 원본이고, 이 디렉터리는 kind E2E와 `go test ./test/charts`가 같은 차트를 쓰도록 둔 사본이다. 바꿀 때는 infra를 먼저 고치고 그대로 옮긴다. infra의 `scripts/check-helm.py`가 lint, 렌더링, AppProject 허용 여부를 확인하고, 이 저장소의 `go test ./test/charts`(helm lint, helm template)와 `test/e2e/deploy/run.sh`(kind의 Argo CD)가 같은 차트를 확인한다.
