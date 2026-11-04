# E2E: GitHub 저장소에서 Argo CD까지 앱 배포

GitHub 앱 배포(ADR 0011)와 앱 버킷, 키, 시크릿(ADR 0016, ADR 0019)을 kind 하나에서 처음부터 끝까지 돌린다. AWS와 GitHub은 쓰지 않는다. 앞부분은 플랫폼 없이 gitops 파일을 손으로 커밋해, 앱 차트 `deploy/charts/hopcharlie-app`과 AppProject, ApplicationSet `deploy/argocd`를 운영과 같은 Argo CD v3.5.3에서 확인한다. 뒷부분은 같은 일을 플랫폼 API로 한다. 플랫폼이 가짜 GitHub에서 `platform.yaml`을 읽고, docker로 빌드하고, gitops 저장소에 커밋하고, Argo CD를 기다린 뒤 게이트웨이에 앱을 등록한다. 마지막에 moimroom의 브랜치 `dev`를 인스턴스 `moimroom--dev`로 따로 띄운다(ADR 0015).

```bash
test/e2e/deploy/run.sh                  # 실행하고 끝나면 전부 지운다
KEEP=1 test/e2e/deploy/run.sh           # 끝난 뒤에도 그대로 둔다 (다시 실행하면 클러스터를 다시 씀)
ONLY=instances test/e2e/deploy/run.sh   # 손으로 쓰는 부분과 플랫폼의 demo를 건너뛰고, 플랫폼의 moimroom과 브랜치 인스턴스만 (14~19)
ONLY=storage test/e2e/deploy/run.sh     # 플랫폼의 버킷, 키, 시크릿, 재배포만 (S1~S6)
INSTANCES=0 test/e2e/deploy/run.sh      # 브랜치 인스턴스(15~18)만 건너뜀. 인스턴스 API가 없는 플랫폼용
test/e2e/deploy/down.sh                 # 남긴 것을 지운다
```

필요한 것은 docker, kubectl, helm, go, git, curl, tar다. 그리고 platform과 나란히 있는 moimroom 체크아웃이 있어야 하고, 그 체크아웃에 `platform.yaml`이 있어야 한다. 위치는 `MOIMROOM_DIR`로 바꿀 수 있다. kind는 `go install`로 작업 디렉터리(`.hopcharlie/e2e-deploy/bin`)에만 설치한다. Windows(Git Bash, Docker Desktop)와 Linux에서 같은 스크립트를 쓴다.

## 구성

포트는 모두 127.0.0.1에만 연다. 번호는 `lib.sh`에 모아 두었다.

| 무엇 | 어디 | 하는 일 |
| --- | --- | --- |
| kind 클러스터 `hop-deploy` | kubeconfig `.hopcharlie/e2e-deploy/kubeconfig` | Argo CD core(v3.5.3), CNPG 1.25.1, MinIO, sealed-secrets 컨트롤러(chart 2.20.0, infra와 같은 이미지), 앱 |
| 레지스트리 컨테이너 `hop-deploy-registry` | `localhost:18393` | kind의 local registry 방식. 파드도 같은 `localhost:18393/...` 이름으로 받음 |
| git 서버 `gitserver` (네임스페이스 `e2e-kit`) | `18390`, 클러스터 안 `http://gitserver.e2e-kit.svc.cluster.local` | `git http-backend`로 `gitops.git`과 `infra.git`을 인증 없이 제공. 차트는 `infra.git`의 `helm/charts/hopcharlie-app`에 둠. 호스트와 플랫폼이 push하고 Argo CD가 fetch |
| 프록시 `proxy` (같은 네임스페이스) | `18391` | 호스트의 게이트웨이(`HTTP_PROXY`)와 `curl -x`가 `*.svc.cluster.local`을 클러스터 안 이름 그대로 부름 |
| 게이트웨이 | `18300`, 관리 `19903` | `-app-domain e2e.test`. `Host: <instance>.e2e.test`를 인스턴스의 Service로 보냄 |
| 플랫폼 저장소 컨테이너 `hop-deploy-store` | `55852` | 플랫폼 DB |
| `fakegithub` | `18392` | 플랫폼의 GitHub App 클라이언트가 부르는 API(설치 토큰, ref, 파일, 저장소 목록)를 `.hopcharlie/e2e-deploy/github/<owner>/<name>`의 git 체크아웃으로 답함 |
| 플랫폼 API | `18380` | `-tags minio`로 빌드. `PLATFORM_DEPLOY_BUILDER=docker`, `PLATFORM_GITOPS_MODE=git`, `PLATFORM_ARGOCD_KUBECONTEXT=kind-hop-deploy`, `PLATFORM_APP_BACKEND_TEMPLATE=http://{app}-{service}.{instance}.svc.cluster.local:{port}`, `PLATFORM_SECRETS_KEY`. clone은 `PLATFORM_DOCKER_CLONE_URL`로 같은 체크아웃에서. `envs.yaml`의 환경 `kind`에 컨트롤러 인증서와 `minio` 드라이버(접두사 `e2e-`, MinIO root 키)가 있고, 드라이버는 `HTTP_PROXY`로 프록시를 거쳐 MinIO를 클러스터 안 이름으로 부름 |
| `probe`, `driver` | 호스트 | `probe`는 게이트웨이로 50ms마다 요청하고 실패 수와 받은 버전을 센다. `driver`는 `pkg/client`로 앱을 등록하고, 서명한 push webhook을 보내고, 배포 SSE를 따라가고, 인스턴스를 더하고 지우고, 앱을 지운다 |

테스트 앱 `demo/`는 그 자체로 앱 저장소다(`platform.yaml`, `Dockerfile`, Go 모듈 하나). `/`는 `version.txt`와 `DEMO_MESSAGE`를 답한다. `/healthz`는 200을 답하고, `DEMO_HEALTHY=false`이면 500을 답한다.

## 흐름

손으로 쓴 gitops 파일:

1. 이미지 네 개를 빌드해 레지스트리에 push한다: git 서버와 프록시, demo v1, demo v2, moimroom. gitops 파일에는 digest로 고정한 이름을 쓴다. `ONLY=instances`이면 git 서버와 프록시만 빌드한다.
2. kind, CNPG, git 서버와 프록시, Argo CD를 올린다. argocd-cm에 SealedSecret health 검사(infra와 같은 Lua)를 넣고, sealed-secrets 컨트롤러를 설치해 공개 인증서를 API 서버의 service proxy로 받는다(`.hopcharlie/e2e-deploy/sealed-secrets.pem`). `infra.git`에 차트를, `gitops.git`에 빈 커밋을 push하고, `helm template deploy/argocd -f argocd-values.yaml`을 적용한다. 게이트웨이를 띄우고, MinIO를 올려 손으로 쓰는 moimroom의 버킷 `e2e-moimroom`과 누구의 것도 아닌 버킷 `e2e-other`를 만든다.

3번부터 8번까지는 `ONLY=instances`이면 건너뛴다.
3. demo v1의 gitops 파일을 커밋하고, 플랫폼처럼 ApplicationSet과 Application에 refresh를 요청한다. 확인하는 것:
   - Application `demo-kind`가 gitops head(`status.sync.revisions`)에서 `Synced Healthy`가 된다.
   - 차트가 만든 네임스페이스 `demo`에 Pod Security `restricted`와 ResourceQuota, LimitRange, NetworkPolicy가 있다.
   - NetworkPolicy는 프록시의 네임스페이스는 들이고, 다른 네임스페이스의 파드는 막는다.
4. 게이트웨이 관리 API로 `demo`를 등록한다(플랫폼이 할 일).
5. 헬스 체크가 실패하는 버전(같은 이미지, `DEMO_HEALTHY=false`)을 커밋한다. 새 파드는 준비되지 않는다. `progressDeadlineSeconds`(kind 프로필 60초)가 지나면 Deployment가 `ProgressDeadlineExceeded`, Application이 `Degraded`가 된다. 그동안과 그 뒤 5초까지 게이트웨이로 보낸 요청이 모두 200이고, 모두 v1이 답해야 한다.
6. demo v2를 커밋한다. `Healthy`가 되고, 요청이 하나도 실패하지 않은 채 마지막 응답이 v2여야 한다.
7. moimroom의 gitops 파일(`postgres`, `s3` 리소스)을 커밋하고, 네임스페이스가 생기면 MinIO root 키로 Secret `app-s3`를 넣는다. 플랫폼은 버킷마다 키를 봉인해 넣는다(S1, 14). `Healthy`가 된 뒤 `/readyz`(DB 핑, 키로 버킷 확인)가 200이고, `/api/spaces`가 DB의 공간을 돌려줘야 한다.
8. `apps/`를 지우는 커밋을 한다. Application 둘과 네임스페이스 둘이 지워져야 한다. 네임스페이스는 차트의 객체라서, 그 안의 DB 볼륨과 테스트가 넣은 S3 Secret도 함께 사라져야 한다.

플랫폼:

9. 저장소, 가짜 GitHub(저장소 `e2e/demo` = `demo/`), 플랫폼 API를 띄운다. 게이트웨이의 `demo`는 지워서 플랫폼이 다시 등록하게 한다.

10번부터 13번까지는 `ONLY=instances`이면 건너뛴다.

10. `POST /v1/apps`로 `e2e/demo`를 등록하고 배포 SSE를 `healthy`까지 따라간다. gitops 파일에 그 배포 id, 앱 커밋, digest 이미지가 있어야 하고, `Host: demo.e2e.test`가 v1을 답해야 한다.
11. `DEMO_HEALTHY: "false"`로 바꾼 커밋을 push webhook으로 보낸다. 배포는 `failed`로 끝나야 한다. Deployment의 progress deadline(60초) 뒤 Application이 `Degraded`가 되고, 1분 이어지면 `PLATFORM_ARGOCD_TIMEOUT`(100초)을 기다리지 않고 끝난다. 그동안 게이트웨이 요청이 하나도 실패하지 않고, 모두 v1이 답해야 한다.
12. v2와 v3 커밋을 잇달아 push한다. v2 배포는 `superseded`, v3 배포는 `healthy`로 끝나야 하고, 요청 실패 없이 마지막 응답이 v3여야 한다.
13. `DELETE /v1/apps/demo`. Application, 네임스페이스, gitops 파일이 사라져야 한다.
S1부터 S6까지는 버킷, 키, 시크릿(ADR 0016, ADR 0019)이고, `ONLY=instances`이면 건너뛴다. 앱 `store`는 demo에 s3 리소스를 더하고 `DEMO_MESSAGE: plain`을 둔 저장소 `e2e/store`다.

S1. `POST /v1/apps`로 `store`를 등록하고 `healthy`까지 따라간다. gitops 파일에 봉인한 버킷 키(`values.sealedSecrets.s3`)가 있고 시크릿은 없어야 한다. 컨트롤러가 연 Secret `app-s3`의 키로 버킷 `e2e-store`에 쓰고 읽을 수 있고, `e2e-other`에는 AccessDenied여야 한다. gitops 파일에 평문 키가 없어야 한다.
S2. 시크릿 `DEMO_MESSAGE=sealed one`을 넣고 재배포한다. 재배포는 trigger `redeploy`, build id 없음, 첫 배포와 같은 이미지여야 한다. gitops 파일에 `appChecksum`이 있고 평문 값과 `platform.yaml`의 `DEMO_MESSAGE`가 없어야 하며, `Host: store.e2e.test`가 `v1 sealed one`을 답해야 한다(시크릿이 이김). 버킷 키는 그대로여야 한다.
S3. 값을 `sealed two`로 바꾸고 다시 재배포한다. `v1 sealed two`를 답해야 한다.
S4. 브랜치 `dev`를 인스턴스 `store--dev`로 더한다. 앱 단위 시크릿을 받아 `v1 sealed two`를 답하고, 자기 버킷 `e2e-store--dev`와 다른 키를 받는다. 두 키 모두 서로의 버킷에는 AccessDenied여야 한다.
S5. 인스턴스를 지운다. Application과 네임스페이스, 버킷 `e2e-store--dev`가 사라지고, 그 키는 InvalidAccessKeyId여야 한다. `store`의 키는 그대로 동작해야 한다.
S6. 앱을 지운다. 버킷 `e2e-store`가 사라지고 그 키는 InvalidAccessKeyId여야 한다.

14. moimroom 체크아웃(HEAD의 `platform.yaml`)을 저장소 `e2e/moimroom`으로 등록한다. 플랫폼이 버킷 `e2e-moimroom`(손으로 쓰는 부분이 만든 것을 이어받음)과 키를 만들어 봉인한다. `healthy` 뒤 Secret `app-s3`가 있고, `Host: moimroom.e2e.test`의 `/readyz`가 200이고(그 키로 버킷 확인) 공간 목록이 비어 있지 않아야 한다.

15번부터 18번까지는 브랜치 인스턴스(ADR 0015)이고, `INSTANCES=0`이면 건너뛴다.

15. 저장소 `e2e/moimroom`에 브랜치 `dev`를 만든다. main과 다른 점은 SPA 빌드가 그대로 싣는 파일 `web/public/e2e-branch.txt` 하나다. `POST /v1/apps/moimroom/instances {"branch":"dev"}`의 인스턴스 이름이 `moimroom--dev`여야 하고, 첫 배포를 `healthy`까지 따라간다. 플랫폼이 버킷 `e2e-moimroom--dev`와 키를 만든다. 확인하는 것:
    - Application `moimroom--dev-kind`가 `Synced Healthy`이고, 네임스페이스 `moimroom--dev`에 CNPG Cluster `moimroom-db`가 따로 있다.
    - `Host: moimroom--dev.e2e.test`의 `/e2e-branch.txt`가 dev의 내용을 답하고, `/readyz`가 200이고, 공간 목록이 비어 있지 않다(dev의 DB에 따로 시드됨).
    - `Host: moimroom.e2e.test`의 `/e2e-branch.txt`는 404다. main은 그대로 main을 답한다.
16. dev에 커밋을 하나 더 하고 push webhook을 보낸다. 배포가 하나만 생기고 그 인스턴스가 `moimroom--dev`여야 하고, `healthy`로 끝난 뒤 `moimroom--dev`가 새 내용을 답해야 한다. main의 마지막 배포(`GET /v1/apps/moimroom/deployments?branch=main&limit=1`)는 push 전과 같은 배포이고 `healthy`여야 한다.
17. `DELETE /v1/apps/moimroom/instances/moimroom--dev`. Application `moimroom--dev-kind`, 네임스페이스, gitops 파일, 게이트웨이 등록이 사라지고, moimroom은 `Synced Healthy`이고 `/readyz`가 200이어야 한다.
18. dev를 다시 더하고 `healthy`를 기다린다.
19. `DELETE /v1/apps/moimroom`. 기본 인스턴스와 `moimroom--dev`의 Application, 네임스페이스, gitops 파일, 게이트웨이 등록, 버킷이 모두 사라져야 한다.

## 통과 조건

모두 만족해야 종료 코드 0이다. 하나라도 어기면 `FAIL:` 줄을 찍는다.

- 각 단계의 Application이 gitops head 커밋에서 기대한 상태가 된다.
- 실패하는 버전을 배포하는 동안(손으로 쓴 것과 플랫폼 둘 다) 게이트웨이 요청 실패가 0이고, 응답이 모두 v1이다.
- 새 버전으로 바꾸는 동안 요청 실패가 0이고, 마지막 응답이 새 버전이다.
- 플랫폼 배포가 `healthy`, `failed`, `superseded` 가운데 기대한 상태로 끝난다.
- moimroom `/readyz`가 200이고, 공간 목록이 비어 있지 않다.
- 지운 뒤 Application, 네임스페이스, moimroom의 PV, S3 Secret, gitops 파일이 없다.
- 브랜치 인스턴스는 자기 네임스페이스, DB, 주소로 뜨고, 그 브랜치의 push는 그 인스턴스만 배포한다. 인스턴스를 지우는 동안 기본 인스턴스는 계속 답하고, 앱을 지우면 두 인스턴스가 모두 사라진다.

## 실제 환경과 다른 점

- 앱이 Argo CD와 같은 클러스터에 있다. 게이트웨이는 호스트에서 프록시를 거쳐 ClusterIP Service로 간다. 운영은 공개 Service를 internal LoadBalancer로 내고, 플랫폼이 그 주소를 읽어 게이트웨이에 등록한다(`PLATFORM_APP_BACKEND_TEMPLATE`를 비우면 그렇게 함).
- Argo CD는 core 설치라서 API 서버와 UI가 없다. 플랫폼은 Kubernetes API로 Application을 읽고 refresh annotation을 붙이므로 그것으로 충분하다.
- ApplicationSet은 5초마다 gitops 저장소를 다시 읽는다(운영 기본값 30초). 그래도 새 커밋은 refresh 요청이 있어야 바로 보인다. 요청이 없으면 repo server가 revision 캐시(3분)로 답한다.
- 손으로 쓰는 moimroom의 버킷과 Secret `app-s3`는 테스트가 MinIO root 키로 만든다. 플랫폼을 거친 앱은 `minio` 드라이버가 버킷과 그 버킷 전용 키를 만들고, 운영의 EKS는 `aws` 드라이버를 쓴다.
- 플랫폼의 `minio` 드라이버는 MinIO의 root 키를 관리 키로 쓴다. 운영 온프렘에서는 관리 전용 키를 따로 둔다.
- kind의 NetworkPolicy 구현은 kindnet이다. egress에 API 서버 포트 6443과 MinIO 포트 9000을 더했다. 운영에서는 둘 다 443이다.
- GitHub은 가짜다. App JWT와 설치 토큰을 검사하지 않고, clone은 로컬 경로에서 한다.
