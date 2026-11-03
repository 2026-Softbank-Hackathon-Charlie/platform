# E2E: gitops 파일에서 Argo CD로 앱을 배포하기

GitHub 앱 배포(ADR 0011)의 뒷부분, 곧 gitops 파일 `apps/<instance>/<env>.yaml`을 Argo CD가 앱으로 만드는 과정을 kind 하나에서 돌린다. 앱 차트 `deploy/charts/hopcharlie-app`과 AppProject, ApplicationSet `deploy/argocd`를 운영과 같은 Argo CD v3.5.3에 올리고, 플랫폼이 쓸 gitops 파일을 손으로 커밋해 결과를 확인한다. AWS와 GitHub은 쓰지 않는다.

```bash
test/e2e/deploy/run.sh          # 실행하고 끝나면 전부 지운다
KEEP=1 test/e2e/deploy/run.sh   # 끝난 뒤에도 그대로 둔다 (다시 실행하면 클러스터를 다시 씀)
test/e2e/deploy/down.sh         # 남긴 것을 지운다
```

필요한 것: docker, kubectl, helm, go, git, curl, 그리고 platform과 나란히 있는 moimroom 체크아웃(`MOIMROOM_DIR`로 바꿀 수 있음). kind는 `go install`로 작업 디렉터리(`.hopcharlie/e2e-deploy/bin`)에만 설치한다. Windows(Git Bash, Docker Desktop)와 Linux에서 같은 스크립트를 쓴다.

## 구성

| 무엇 | 어디 | 하는 일 |
| --- | --- | --- |
| kind 클러스터 `hop-deploy` | kubeconfig `.hopcharlie/e2e-deploy/kubeconfig` | Argo CD core(v3.5.3), CNPG 1.25.1, MinIO, 앱 |
| 레지스트리 컨테이너 `hop-deploy-registry` | 호스트 `localhost:18393` | kind의 local registry 방식. 파드에서도 같은 `localhost:18393/...` 이름으로 받음 |
| git 서버 (`gitserver`, 네임스페이스 `e2e-kit`) | 호스트 `127.0.0.1:18390`, 클러스터 `http://gitserver.e2e-kit.svc.cluster.local` | `git http-backend`로 `gitops.git`과 `infra.git`(차트를 `helm/charts/hopcharlie-app`에 둠)을 인증 없이 제공. 호스트가 push하고 Argo CD가 fetch |
| 프록시 (`proxy`, 같은 네임스페이스) | 호스트 `127.0.0.1:18391` | 호스트의 게이트웨이(`HTTP_PROXY`)와 `curl -x`가 `*.svc.cluster.local`을 클러스터 안 이름 그대로 부름 |
| 게이트웨이 | 호스트 `127.0.0.1:18300`, 관리 `19903` | `-app-domain e2e.test`. `Host: demo.e2e.test`를 `http://demo-web.demo.svc.cluster.local:8080`으로 보냄 |
| `probe` | 호스트 | 게이트웨이로 50ms마다 요청하고 실패 수와 받은 버전을 셈 |

테스트 앱 `demo/`는 그 자체로 앱 저장소다(`platform.yaml`, `Dockerfile`, Go 모듈 하나). `/`는 `version.txt`와 `DEMO_MESSAGE`를, `/healthz`는 200을 답하고 `DEMO_HEALTHY=false`이면 500을 답한다.

## 흐름

1. 이미지 네 개(git 서버와 프록시, demo v1, demo v2, moimroom)를 빌드해 레지스트리에 push한다. gitops 파일에는 digest로 고정한 이름을 쓴다.
2. kind, CNPG, git 서버와 프록시, Argo CD를 올리고, `infra.git`에 차트를, `gitops.git`에 빈 커밋을 push한 뒤 `helm template deploy/argocd -f argocd-values.yaml`을 적용한다.
3. demo v1의 gitops 파일을 커밋하고, 플랫폼처럼 ApplicationSet과 Application에 refresh를 요청한다. Application `demo-kind`가 gitops head에서 `Synced Healthy`가 되고, 차트가 만든 네임스페이스 `demo`에 Pod Security `restricted`와 ResourceQuota, LimitRange, NetworkPolicy가 있는지 본다. NetworkPolicy는 프록시의 네임스페이스는 들이고 다른 네임스페이스의 파드는 막아야 한다.
4. 게이트웨이를 띄우고 관리 API로 `demo`를 등록한다(플랫폼이 할 일).
5. 헬스 체크가 실패하는 버전(같은 이미지, `DEMO_HEALTHY=false`)을 커밋한다. 새 파드는 준비되지 않고, `progressDeadlineSeconds`(kind 프로필 60초)가 지나면 Deployment가 `ProgressDeadlineExceeded`, Application이 `Degraded`가 된다. 그동안 그리고 그 뒤 5초까지 게이트웨이로 보낸 요청이 모두 200이고 v1이 답해야 한다.
6. demo v2를 커밋한다. `Healthy`가 되고, 요청이 하나도 실패하지 않은 채 마지막 응답이 v2여야 한다.
7. MinIO를 올리고 버킷 `moimroom-photos`를 만든 뒤 moimroom의 gitops 파일(`postgres`, `s3` 리소스)을 커밋한다. 네임스페이스가 생기면 S3 키 Secret `s3-credentials`를 넣는다. 버킷과 Secret은 운영에서 infra가 준비하는 것이다. `Healthy` 뒤 `/readyz`(DB 핑과 키로 버킷 확인)가 200이고 `/api/spaces`가 DB의 공간을 돌려줘야 한다.
8. `apps/`를 지우는 커밋을 한다. Application 둘과 네임스페이스 둘이 지워져야 한다. 네임스페이스는 차트의 객체라서, 그 안의 DB 볼륨과 테스트가 넣은 S3 Secret도 함께 사라져야 한다.

## 통과 조건

모두 만족해야 종료 코드 0이다. 하나라도 어기면 `FAIL:` 줄을 찍는다.

- 각 단계의 Application이 gitops head 커밋(`status.sync.revisions`)에서 기대한 상태가 됨
- 실패하는 버전을 배포하는 동안 게이트웨이 요청 실패 0, 받은 응답이 모두 v1
- v2로 바꾸는 동안 요청 실패 0, 마지막 응답이 v2
- moimroom `/readyz` 200, 공간 목록이 비어 있지 않음
- 삭제 뒤 Application, 네임스페이스, moimroom의 PV, S3 Secret이 없음

## 실제 환경과 다른 점

- 앱이 Argo CD와 같은 클러스터에 있고, 게이트웨이는 호스트에서 프록시를 거쳐 ClusterIP Service로 간다. 운영은 공개 Service를 internal LoadBalancer로 내고, 플랫폼이 그 주소를 게이트웨이에 등록한다.
- Argo CD는 core 설치(API 서버와 UI 없음)다. 플랫폼은 Kubernetes API로 Application을 읽고 refresh annotation을 붙이므로 그것으로 충분하다.
- ApplicationSet은 5초마다 gitops 저장소를 다시 읽는다(운영 기본값 30초).
- 버킷과 S3 키 Secret은 테스트가 만든다. 차트는 만들지 않는다.
- kind의 NetworkPolicy 구현은 kindnet이다. egress에 API 서버 포트 6443과 MinIO 포트 9000을 더했다(운영은 둘 다 443).
