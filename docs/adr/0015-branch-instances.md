# 0015. 앱 하나에 운영자가 등록한 브랜치마다 인스턴스를 따로 띄운다

- 상태: 채택
- 날짜: 2026-11-03

## 상황

ADR 0011의 앱은 저장소 하나와 브랜치 하나로 되어 있어서, 앱마다 배포되는 주소도 하나다. 엔터프라이즈 앱은 기본 브랜치 말고도 개발 서버나 미리보기 서버를 띄우고 싶어 한다. 예를 들어 `moimroom`의 `main`은 `https://moimroom.hopcharlie.dev`에, `dev`는 `https://moimroom--dev.hopcharlie.dev`에 띄운다.

제약은 이렇다.

- `*.hopcharlie.dev` 인증서는 레이블 한 단계만 덮는다. 그래서 브랜치를 `<branch>.<app>.hopcharlie.dev`로 둘 수 없다.
- 워크로드 EKS 노드는 2대다. 인스턴스마다 내부 NLB, CNPG Cluster, ResourceQuota가 하나씩 생긴다.
- 앱 차트와 ApplicationSet(platform `deploy/`, infra `helm/charts/hopcharlie-app(s)`)은 이미 gitops 파일의 `namespace`로 Application 이름, Helm release 이름, 네임스페이스 라벨, 버킷 이름을 정한다. `test/charts`에 `moimroom--feature-x` 경우가 있다. 바뀌어야 하는 것은 플랫폼의 저장소, API, 웹훅, 배포 실행기, CLI, 콘솔이다.

## 결정

### 인스턴스와 이름

- 인스턴스는 앱의 브랜치 하나를 배포한 단위다. 앱의 기본 브랜치(`apps.branch`, 등록할 때 정한 브랜치)는 인스턴스 이름이 `<app>`이고, 운영자가 추가한 브랜치는 `<app>--<slug>`다.
- 인스턴스 이름은 네임스페이스, gitops 경로 `apps/<instance>/<env>.yaml`, Argo CD Application `<instance>-<env>`, 게이트웨이 앱 이름, 주소 `https://<instance>.<domain>`에 그대로 쓴다.
- gitops 파일의 `app`과 `values.app`은 앱 이름 그대로다. 그래서 Deployment, Service, CNPG Cluster 이름(`<app>-<part>`)은 인스턴스마다 같고, 네임스페이스로 나뉜다. 이미지 저장소도 앱 단위(`hopcharlie/apps/<app>/<service>`)로 함께 쓴다. 같은 커밋이면 같은 이미지다.
- gitops 파일에는 새 헤더 키를 넣지 않는다. infra 차트의 `values.schema.json`이 모르는 키를 거부하기 때문이다. 그래서 이 기능은 infra를 고치지 않는다.
- 슬러그는 브랜치 이름을 소문자로 바꾸고, `[a-z0-9]`가 아닌 글자를 `-`로 바꾸고, 이어진 `-`를 하나로 줄이고, 앞뒤의 `-`를 떼어 만든다. 비면 그 브랜치는 추가할 수 없다.
- 인스턴스 이름은 53자까지다. Helm release 이름(= 네임스페이스)의 상한이 53자라서, 이보다 길면 Argo CD가 렌더링하지 못한다. 길면 슬러그를 잘라 `<app>--<잘린 슬러그>-<해시 6자>`로 만든다. 해시는 원래 브랜치 이름의 SHA-256 앞 6자(16진수)다. 계산은 `appspec.InstanceName`이 한다.
- 앱 이름 규칙에 두 가지를 더한다. `--`를 쓸 수 없고(인스턴스 이름과 겹친다), 3자 이상이어야 한다(2자 앱 `ab`는 `ab--x`처럼 셋째, 넷째 글자가 `--`인 예약 DNS 레이블을 만든다). 앱 이름은 `^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`에 맞고 `--`를 포함하지 않는다.
- 게이트웨이의 앱 이름 상한은 32자에서 63자(DNS 레이블 하나)로 늘린다.

### 추적 브랜치

- 운영자가 등록한 브랜치만 배포한다. push 한 모든 브랜치를 자동으로 미리보기로 띄우지 않는다.
- 앱 하나에 인스턴스는 기본 인스턴스를 포함해 3개까지다(`MaxInstances`).
- 기본 브랜치는 지금처럼 `apps` 행에 둔다. 추가한 브랜치는 새 테이블 `app_branches`에 한 행씩 두고, 행마다 인스턴스 이름, `autoDeploy`, 그 브랜치에서 마지막으로 검사를 통과한 `platform.yaml`(`spec`, `specCommit`)을 둔다. 인스턴스 이름은 전체에서 겹치지 않는다.
- 배포(`deployments`)에 `branch`(어느 인스턴스로 가는지 정하는 추적 브랜치)와 `instance`를 더한다. 기존 행은 마이그레이션에서 앱 이름과 앱의 기본 브랜치로 채운다.

### API

- `POST /v1/apps/{name}/instances` `{branch, autoDeploy}`: 브랜치 head의 `platform.yaml`을 앱 등록과 같은 규칙으로 검사하고(`name`은 앱 이름과 같아야 한다), 브랜치를 저장하고, 첫 배포(trigger `register`)를 큐에 넣는다. 201로 인스턴스와 배포를 돌려준다.
- `DELETE /v1/apps/{name}/instances/{instance}`: 그 인스턴스의 끝나지 않은 배포를 superseded로 끝내고, gitops에서 `apps/<instance>/`를 지우고, 게이트웨이에서 지우고, 브랜치와 그 배포 기록을 지운다. 기본 인스턴스는 지울 수 없다(409 `default_instance`). 앱을 지우면 된다.
- 경로에는 브랜치 이름 대신 인스턴스 이름을 쓴다. 브랜치 이름에는 `/`가 들어가서, 경로에 넣으면 클라이언트와 프록시마다 `%2F` 처리가 다르다.
- `App.deploy.instances`가 기본 인스턴스를 맨 앞으로 모든 인스턴스를 보여 준다. 인스턴스마다 브랜치, 주소, 환경별 상태, 마지막 배포가 있다. `App.deploy`의 기존 필드(`branch`, `autoDeploy`, `spec`, `specCommit`, `url`, `status`, `lastDeployment`)는 기본 인스턴스의 값이다.
- `POST /v1/apps/{name}/deployments`에 `branch`를 더한다. 추적하는 브랜치여야 하고(아니면 400), 비우면 기본 브랜치다. `ref`는 그 인스턴스에 배포할 커밋이나 ref이고, 비우면 `branch`의 head다.
- `GET /v1/apps/{name}/deployments`는 `?branch=`로 거를 수 있다.

### 웹훅

- push 한 브랜치가 앱의 기본 브랜치이고 앱의 `autoDeploy`가 켜져 있으면 기본 인스턴스에, 추적 브랜치이고 그 브랜치의 `autoDeploy`가 켜져 있으면 그 인스턴스에 배포한다. GitHub App은 이미 모든 push를 보내므로 GitHub 쪽 설정은 바꾸지 않는다.

### 배포 실행기

- supersede, 실행기 잠금, `platform.yaml` 갱신을 인스턴스 단위로 한다. 다른 브랜치의 push가 기본 인스턴스의 배포를 superseded로 끝내지 않는다.
- 앱을 지우면 기본 인스턴스와 추적 브랜치 인스턴스의 gitops 디렉터리, 게이트웨이 앱을 모두 지운다.

### 리소스

- DB: 앱 차트가 네임스페이스마다 CNPG Cluster를 만들기 때문에 인스턴스마다 DB가 따로 생긴다. 인스턴스를 지우면 네임스페이스와 함께 DB 데이터도 지워진다.
- 버킷: 차트가 버킷 이름을 `<bucketPrefix><instance>-<resource>`로 정하므로 이름은 인스턴스마다 다르다. 버킷과 키 Secret을 누가 만드는지는 ADR 0011부터 남은 문제이고, 이 ADR은 정하지 않는다(infra "앱 S3 버킷과 자격 증명" 작업).

## 검토한 대안

- `<branch>.<app>.hopcharlie.dev`: 와일드카드 인증서가 한 단계만 덮어서 앱마다 인증서와 DNS 레코드가 필요하다.
- push 한 모든 브랜치를 자동으로 미리보기로 띄우기: 노드 2대에서 브랜치마다 DB와 NLB가 생기면 자원이 모자란다.
- 기본 브랜치도 `app_branches`에 넣는 통합 모델: 한 가지 경로로 다룰 수 있지만, v1의 앱 등록, 웹훅, 실행기, 콘솔이 모두 바뀌어 위험이 크다. 기본 인스턴스는 v1 그대로 두고 추가 인스턴스만 더한다.
- 브랜치 이름을 경로에 넣는 API(`/branches/{branch}`): `/`가 든 브랜치를 `%2F`로 넣어야 하고, 프록시와 라우터마다 다르게 다룬다.
- 슬러그 없이 해시만 쓰는 인스턴스 이름: 겹치지는 않지만 주소만 보고 어느 브랜치인지 알 수 없다.

## 결과

- infra는 바꾸지 않는다. 앱을 더 넣을 때와 마찬가지로 인스턴스를 더할 때도 infra PR이 필요 없다.
- 인스턴스 하나마다 내부 NLB 하나, CNPG PVC 하나, 네임스페이스 Quota 하나가 생긴다. 상한 3개는 이 비용 때문이다.
- `feature/x`와 `feature-x`처럼 슬러그가 같은 두 브랜치는 둘째가 409 `instance_exists`로 거부된다.
- GKE의 Argo CD는 `charlie-apps` 네임스페이스만 관리하므로, 지금은 인스턴스를 eks 환경에만 띄울 수 있다.
- CodeBuild 프로젝트의 동시 빌드가 1개이고 superseded 빌드를 멈추지 않으므로(StopBuild 없음), 여러 인스턴스의 배포가 빌드에서 줄을 선다.
- 앱 이름 규칙이 좁아진다. 이미 등록된 2자 앱이나 `--`가 든 앱은 그대로 동작하지만, 다음 배포에서 `platform.yaml` 검사에 걸린다.
