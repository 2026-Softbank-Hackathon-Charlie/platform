# 0024. platform.yaml 서비스에 포트 없는 워커와 메모리, CPU를 두고, DB 연결 값을 변수마다 나눠 넣는다

- 날짜: 2026-11-05
- 상태: 제안
- 관련 ADR: [0011](0011-github-app-deploy.md)(`platform.yaml` 규칙 중 `port` 필수를 바꾼다), [0015](0015-branch-instances.md), [0016](0016-app-object-storage.md), [0019](0019-app-secrets-and-redeploy.md), infra ADR 0050, infra ADR 0056(차트 쪽 구현)

## 배경과 제약

오픈소스 앱을 플랫폼에 올리면서 2026-11-05 기준으로 세 가지가 막혔다.

- DB 연결: 앱은 `DATABASE_URL` 하나만 받는다. 호스트, 사용자, 비밀번호를 따로 읽는 앱(Spring의 `SPRING_DATASOURCE_*`, Keycloak의 `KC_DB_*`, Gitea)은 시작 스크립트에서 URL을 쪼개야 했다.
- 워커: `port`가 필수이고 `healthCheck`가 없으면 차트가 TCP readiness probe를 단다. 포트를 열지 않는 큐 워커는 준비되지 않고, 배포가 300초 뒤 이유 없이 실패한다.
- 메모리: 모든 컨테이너가 환경 기본값(request cpu 50m, memory 64Mi, limit memory 256Mi)을 받는다. JVM 앱은 뜨지 않는다.

확인한 사실(2026-11-05):

- 운영 CNPG app Secret `moimroom/moimroom-db-app`의 키는 `dbname`, `host`, `jdbc-uri`, `password`, `port`, `uri`, `user`, `username`(그 밖에 `fqdn-*`, `pgpass`)이다. 키 이름만 조회했다.
- infra `clusters/aws-dev-management/values/hopcharlie-apps.yaml`의 `eks`, `gke` 프로필은 네임스페이스마다 ResourceQuota requests.cpu 2, requests.memory 4Gi, limits.memory 8Gi, pods 20과 LimitRange 기본값(request cpu 50m, memory 64Mi, limit memory 256Mi)을 둔다. 인스턴스(ADR 0015)마다 네임스페이스가 따로이므로 quota도 인스턴스마다 따로다.
- 앱 차트의 Deployment는 `maxSurge: 1`, `maxUnavailable: 0`이다. Argo CD가 모든 Deployment를 한 번에 바꾸므로 배포 중에는 서비스마다 `replicas + 1`개의 파드가 동시에 quota를 쓴다.
- 데이터베이스 파드는 지금 LimitRange 기본값을 받는다. infra ADR 0056로 `postgres.resources`가 request memory 256Mi, limit memory 512Mi가 되고 CPU request는 기본값 50m이다.
- 배포 실행기(`internal/deploy`)는 공개 서비스만 게이트웨이에 등록하고(`register`, `ServiceBackends`, `TemplateBackends`), 나머지는 Argo CD Application의 sync와 health만 기다린다. Argo CD는 Deployment를 갱신된 파드가 모두 available이면 Healthy로 본다. Service나 Endpoints를 기다리는 곳은 공개 서비스의 주소 찾기뿐이다.

## 검토한 대안

- `PG`로 시작하는 이름을 모두 예약: 앱이 `PGSSLMODE`, `PGAPPNAME`, `PGOPTIONS`를 직접 정해야 하는 경우가 있다. 정확히 여섯 이름만 예약한다.
- `AWS_ACCESS_KEY_ID` 같은 `AWS_*`를 s3 리소스에서 자동으로 넣기: AWS SDK의 기본 자격 증명 순서는 환경 변수가 먼저다. 파드 자격 증명으로 Bedrock이나 다른 AWS API를 부르는 앱이 버킷 전용 키를 집어 들어 실패한다. GCS(ADR 0016의 gke)에서는 이름부터 맞지 않는다. 필요한 앱은 `AWS_ACCESS_KEY_ID: "$(S3_ACCESS_KEY)"`로 직접 넘긴다.
- 서비스마다 limit만 받고 합계는 보지 않기: 서비스 5개가 각각 1.5Gi를 쓰면 quota를 넘는다. 넘친 파드는 ReplicaSet이 만들지 못하고(FailedCreate) 배포는 `progressDeadlineSeconds` 뒤에야 실패하며, 이유가 배포 화면에 나오지 않는다. 등록과 배포 전 검사에서 바로 알려 주려고 합계를 검사한다.
- 처음 안(memory 64Mi에서 2Gi, cpu 50m에서 2, 합계 3Gi, 롤링 업데이트를 세지 않음): 2Gi 서비스 하나(replicas 1)도 롤링 업데이트 중에는 파드가 둘이라 4Gi이고, DB 256Mi를 더하면 requests.memory 4Gi를 넘는다. cpu 2는 서비스 하나로 quota 전체다. 64Mi 서비스로는 파드 수가 pods 20을 넘을 수 있다(아래). 그래서 숫자를 다시 정했다.
- memory request와 limit을 따로 받기: 값이 둘이면 사용자가 합계를 맞추기 어렵고, request가 limit보다 작으면 노드에서 메모리가 모자랄 때 먼저 죽는다. 하나만 받아 둘 다 같게 둔다.
- CPU limit 두기: JVM 시작 시간이 throttle로 길어진다. quota에도 limits.cpu가 없다. request만 둔다.

## 결정과 이유

### 서비스 필드

| 필드 | 규칙 | 차트가 그리는 것 |
| --- | --- | --- |
| `port` | 선택. 쓰면 1에서 65535 | 있으면 `containerPort`, Service, `PORT`, probe. 없으면 워커 |
| `public` | `true`이면 `port`가 있어야 한다 | 게이트웨이가 보내는 Service |
| `healthCheck` | 쓰면 `/`로 시작하고 `port`가 있어야 한다 | HTTP probe |
| `memory` | 선택. Kubernetes quantity, 256Mi에서 1536Mi | memory request와 limit 모두 이 값 |
| `cpu` | 선택. Kubernetes quantity, 50m에서 500m | CPU request. CPU limit 없음 |

- 워커(`port` 없음)는 Service, `containerPort`, probe, `PORT`가 없다. 컨테이너가 실행되면 준비된 것으로 본다. Deployment에 `minReadySeconds: 10`을 두어, 10초 동안 재시작 없이 돌아야 available로 센다. probe가 없으면 시작하자마자 죽는 워커도 잠깐 Ready가 되어 롤링 업데이트가 옛 파드를 내리고 Argo CD가 Healthy로 볼 수 있기 때문이다.
- 워커만 있는 앱도 된다. 공개 서비스가 없으므로 게이트웨이에 등록하지 않고 URL이 없으며, 배포는 모든 환경이 Synced, Healthy이면 끝난다. 지금의 공개 서비스 없는 앱과 같은 경로다.
- `memory`와 `cpu`는 문자열로 받는다. YAML이 숫자로 읽는 `cpu: 0.5`도 문자열 `"0.5"`로 들어온다. gitops 파일에는 Kubernetes가 되돌려 쓰는 정규형(`1.5Gi`는 `1536Mi`, `0.5`는 `500m`)으로 쓴다. 그래서 Argo CD가 보는 값과 클러스터의 값이 같다.

### 합계와 숫자

모든 서비스에 대해 다음 두 합을 검사한다. `memory`가 없는 서비스는 256Mi(기본 limit), `cpu`가 없는 서비스는 50m(기본 request)로 센다.

- (replicas + 1) x memory의 합은 3Gi 이하
- (replicas + 1) x cpu의 합은 1500m 이하

`+ 1`은 롤링 업데이트 중 서비스마다 하나 더 뜨는 파드다. 이 두 합이 quota 안에 드는 이유:

| quota 항목 | 서비스 몫(최대) | DB 파드 | 합 | quota |
| --- | --- | --- | --- | --- |
| requests.memory | 3Gi. 파드마다 request는 센 값 이하(설정하면 같고, 기본값은 64Mi) | 256Mi | 3.25Gi | 4Gi |
| limits.memory | 3Gi. 파드마다 limit은 센 값과 같다 | 512Mi | 3.5Gi | 8Gi |
| requests.cpu | 1500m | 50m | 1550m | 2 |
| pods | 파드마다 256Mi 이상으로 세므로 3Gi / 256Mi = 12개 | 1 | 13 | 20 |

- 남는 requests.memory 768Mi와 requests.cpu 450m은 DB 파드에 붙을 sidecar(예: 백업 plugin)와 DB 메모리를 1Gi까지 올릴 여유다.
- memory 하한을 256Mi로 둔 것은 pods 때문이다. 하한이 64Mi이면 3Gi로 48개를 셀 수 있어 pods 20을 넘는다. 256Mi보다 작게 주면 기본값보다 limit만 낮아지므로 쓸 이유도 적다. 그런 서비스는 `memory`를 쓰지 않으면 된다.
- 상한 1536Mi는 replicas 1인 서비스 하나가 롤링 업데이트에서 합계 3Gi를 다 쓰는 값이다. cpu 상한 500m은 합계 1500m 안에서 replicas 2까지 되는 값이고, 2026-11-05 23시 워크로드 노드 두 대(각 allocatable 1930m)에 남은 CPU request 여유가 각각 약 600m여서 한 파드가 노드 하나에 들어가는 크기다.
- 지금 운영 앱은 둘 다 서비스 하나, replicas 1, `memory`와 `cpu` 없음이라 memory 512Mi, cpu 100m로 센다.
- 남는 한계: quota는 종료 중인 파드도 grace period가 끝날 때까지 센다. Deployment 컨트롤러는 옛 파드가 종료되는 동안(preStop 5초와 앱 종료 시간) 다음 새 파드를 만들 수 있고, 그때 잠깐 quota가 모자라면 ReplicaSet이 다시 시도한다. 배포가 늦어질 수는 있지만 `progressDeadlineSeconds`(eks 300초) 안에 끝난다. 노드 용량은 quota와 별개라 이 규칙이 보장하지 않는다.

### 검사 메시지

`pkg/appspec`의 `Validate`가 다음을 돌려준다(경로: 메시지).

- `services[i].port`: `must be between 1 and 65535`
- `services[i].public`: `needs a port: the traffic gateway sends requests to it`
- `services[i].healthCheck`: `needs a port: the probes request the path on it`
- `services[i].memory`: `must be a Kubernetes quantity such as 512Mi or 1Gi`, `must be between 256Mi and 1536Mi`
- `services[i].cpu`: `must be a Kubernetes quantity such as 250m or 0.5`, `must be between 50m and 500m`
- `services`: `need at most 3Gi of memory during a rolling update: the sum of (replicas + 1) x memory, counting an unset memory as 256Mi, is <합>`
- `services`: `need at most 1500m of CPU during a rolling update: the sum of (replicas + 1) x cpu, counting an unset cpu as 50m, is <합>`
- `services[i].env.<NAME>`: `is set by the platform (PORT, DATABASE_URL, PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD, JDBC_DATABASE_URL and S3_* are reserved)`

어느 서비스의 `memory`나 `cpu`가 틀리면 그 합계는 검사하지 않는다(같은 원인을 두 번 알리지 않는다).

### DB 연결 변수

`postgres` 리소스가 있으면 모든 서비스가 `DATABASE_URL`과 같은 Secret `<app>-<resource>-app`에서 다음을 받는다. 모든 환경에서 차트가 그 환경의 CNPG Cluster를 그리므로 Secret도 같다.

| 변수 | Secret 키 |
| --- | --- |
| `PGHOST` | `host` |
| `PGPORT` | `port` |
| `PGDATABASE` | `dbname` |
| `PGUSER` | `username` |
| `PGPASSWORD` | `password` |
| `JDBC_DATABASE_URL` | `jdbc-uri` |

- 여섯 이름은 `Reserved`에 더해 서비스 `env`와 앱 시크릿(ADR 0019) 이름으로 쓸 수 없다. `PGSSLMODE` 같은 나머지 PG 변수는 앱이 정한다.
- 차트는 플랫폼 변수를 서비스 `env`보다 앞에 둔다. Kubernetes는 `$(NAME)`을 앞에 정한 변수로 바꾸므로 앱이 읽는 이름으로 넘길 수 있다: `KC_DB_URL_HOST: "$(PGHOST)"`, `SPRING_DATASOURCE_URL: "$(JDBC_DATABASE_URL)"`, `AWS_ACCESS_KEY_ID: "$(S3_ACCESS_KEY)"`. 앱 시크릿(`envFrom`)도 `env`보다 먼저 정해지므로 같은 방법이 된다.

### 적용 범위

- 등록, 인스턴스 추가(ADR 0015), 배포 전 검사가 모두 `deploy.CheckSpec`(`appspec.Check`)을 거치므로 브랜치 인스턴스도 같은 규칙과 같은 gitops 파일 형식을 쓴다. 인스턴스마다 네임스페이스와 quota가 따로라 보장도 인스턴스마다 성립한다.
- 재배포(ADR 0019)는 저장한 spec을 다시 검사하지 않고 그린다. 이 변경 전의 spec은 `memory`, `cpu`가 없고 `port`가 있으므로 gitops 파일이 바이트까지 같다.
- `memory`, `cpu`, 포트 없는 서비스를 쓴 gitops 파일은 infra ADR 0056의 차트만 그릴 수 있다. infra 변경이 먼저 merge되어야 한다.

## 영향과 후속 작업

- 기대 효과: JVM 앱, Keycloak, 큐 워커를 시작 스크립트 없이 올린다. 합계를 넘는 `platform.yaml`은 등록과 배포 전에 위치와 이유가 담긴 오류로 거절된다.
- 단점과 위험:
  - 이전에 통과하던 spec 중 롤링 업데이트를 세어 파드가 12개를 넘는 것(예: 서비스 5개, replicas 2)은 이제 거절된다. 운영 앱 moimroom과 docker-doom은 파드 2개로 센다.
  - CLI(`charlie`)와 `hopai`는 예전 `pkg/appspec`을 고정해 쓴다. 의존성을 올리기 전에는 로컬 검사에서 `memory`, `cpu`를 모르는 키로 거절하고, 포트 없는 서비스를 `port: is required`로 거절한다. 서버 검사는 이 ADR을 따른다.
  - 콘솔의 API 타입에서 `port`가 선택이 된다.
  - quota와 LimitRange, DB 파드 resources는 infra 프로필에 있고 이 숫자는 코드 상수다. 한쪽만 바꾸면 보장이 깨진다.
- 필요한 후속 작업 및 검증:
  1. infra 차트 PR(ADR 0056)을 먼저 merge하고, 운영 앱이 Synced, Healthy인지 본 뒤 이 변경을 merge한다.
  2. CLI와 ai-sandbox의 `github.com/2026-Softbank-Hackathon-Charlie/platform` 의존성을 올린다. CLI의 서비스 표는 포트 없는 서비스의 PORT를 0으로 보이므로 함께 고친다.
  3. infra 프로필의 quota, LimitRange, `postgres.resources`를 바꾸면 `pkg/appspec`의 `MaxTotalMemory`, `MaxTotalCPU`, 범위를 다시 계산한다.
  4. 워커 하나와 `memory`를 쓴 앱을 eks에 배포해 Deployment가 Service 없이 Healthy가 되고, quota 사용량이 위 표 안에 드는지 확인한다.

## 관련 자료

- [pkg/appspec/appspec.go](../../pkg/appspec/appspec.go): 규칙, 상수, 메시지
- [internal/deploy/gitops.go](../../internal/deploy/gitops.go): gitops 파일의 `port`, `memory`, `cpu`
- [api/openapi.yaml](../../api/openapi.yaml): `AppSpec`, `AppSpecService`, `AppSpecResource`
- [deploy/charts/hopcharlie-app](../../deploy/charts/hopcharlie-app): infra 차트의 테스트용 사본
- infra `helm/charts/hopcharlie-app`, `clusters/aws-dev-management/values/hopcharlie-apps.yaml`, `docs/adr/0056-render-app-service-contract-in-chart.md`
- Kubernetes: [Dependent environment variables](https://kubernetes.io/docs/tasks/inject-data-application/define-interdependent-environment-variables/), [Resource quotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/), [minReadySeconds](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#min-ready-seconds)
- CloudNativePG: [Connecting from an application](https://cloudnative-pg.io/documentation/current/applications/)
