# AWS 관리 호스트

플랫폼 API, 트래픽 게이트웨이, PostgreSQL, Caddy를 EC2 한 대에 Docker Compose로 띄웁니다. Notion에서 정한 "AWS 관리 호스트"를 Terraform으로 만든 것입니다.

## 관리 EKS로 옮기는 중

2026-11-02 팀 회의에서 플랫폼을 infra 레포의 관리 EKS(`charlie-dev-management`)로 옮기기로 했습니다. 클러스터와 그 위의 pod는 infra 레포가 Argo CD로 배포합니다(infra `docs/adr/0015`, `helm/charts/hopcharlie-platform`). 이 폴더는 플랫폼 전용 AWS 리소스를 계속 맡습니다.

| 파일 | 내용 |
| --- | --- |
| `eks.tf` | pod용 IAM 역할과 Pod Identity 연결 (`charlie-platform/hopcharlie-platform`). 권한은 호스트 역할과 같다 |
| `dns/` | `*.hopcharlie.dev` ACM 인증서와 Cloudflare 레코드. Cloudflare 토큰이 있는 사람만 실행한다 |

pod는 지금과 같은 SSM 파라미터(`/hopcharlie/*`)를 init 컨테이너에서 읽습니다. 그래서 토큰과 비밀값은 그대로이고, 값을 바꾸는 방법도 같습니다(put-parameter 후 pod 재시작).

옮기는 순서는 다음과 같습니다. 각 단계는 apply 전에 팀 승인을 받습니다.

1. `./tf.sh apply`로 pod 역할과 Pod Identity 연결을 만든다.
2. infra PR을 merge하면 Argo CD가 플랫폼을 배포한다. pod가 Ready인지 확인한다.
3. `hopcharlie.dev`의 네임서버가 Cloudflare로 바뀌면 인증서를 발급한다.
   ```bash
   export CLOUDFLARE_API_TOKEN=...   # Zone.DNS:Edit 권한
   ./tf.sh -chdir=dns init
   ./tf.sh -chdir=dns apply
   ./tf.sh -chdir=dns output -raw certificate_arn
   ```
4. infra에서 baseline `externalAlb`(인증서 ARN)와 플랫폼 `ingress.enabled`를 켜는 PR을 올린다. ALB가 생기면 레코드를 연결한다.
   ```bash
   ./tf.sh -chdir=dns apply -var alb_name=charlie-management-external
   API_URL=https://api.hopcharlie.dev ./push-console.sh
   ```
5. 새 콘솔 이미지의 digest를 infra values에 반영하고, `charlie login --server https://api.hopcharlie.dev`로 확인한 뒤 EC2 호스트를 정리한다.

## 만드는 것

| 리소스 | 내용 |
| --- | --- |
| EC2 | t3.small 1대, Amazon Linux 2023, 기본 VPC, 디스크 gp3 20GiB(암호화) |
| Elastic IP | 고정 공인 IP. `api.<IP>.sslip.io`, `app.<IP>.sslip.io` 도메인으로 쓴다 |
| 보안 그룹 | 80, 443만 연다. SSH 포트는 없고 SSM Session Manager로 들어간다 |
| ECR | `hopcharlie/platform`, `hopcharlie/console` 이미지 저장소 (각각 최근 10개만 보관) |
| IAM | 호스트 역할: SSM 접속, ECR 읽기, `/hopcharlie/*` 파라미터 읽기, 보안 뷰가 부르는 보안 설정 읽기 API만 (`read-security-configuration`) |
| SSM Parameter Store | 호스트 설정 파일(`/hopcharlie/host/*`), 생성한 비밀값(DB 비밀번호, 게이트웨이 관리 토큰, API 토큰), 팀이 넣는 비밀값(아래 "설정 바꾸기") |

호스트 안의 구성은 다음과 같습니다.

- Caddy가 Let's Encrypt 인증서로 HTTPS를 받습니다.
  - `api.<IP>.sslip.io`는 플랫폼 API로 보냅니다.
  - `app.<IP>.sslip.io`는 게이트웨이로 보냅니다.
  - `console.<IP>.sslip.io`는 콘솔(frontend 레포를 빌드한 정적 파일)로 보냅니다. 콘솔 주소는 CORS 허용 목록에 자동으로 들어갑니다.
- 호스트의 설정 파일은 모두 SSM에 있고, `host/sync.sh`가 받아서 `/opt/hopcharlie`에 씁니다. 처음 부팅할 때와 `./redeploy.sh`를 실행할 때마다 돕니다. 그래서 설정을 바꿔도 EC2가 교체되지 않습니다(user_data는 첫 부팅 뒤 무시합니다).
- 게이트웨이 관리 포트(9901)와 PostgreSQL은 밖으로 열지 않습니다.
- sslip.io는 팀 도메인(Cloudflare)이 생기기 전까지 쓰는 임시 주소입니다.

## 비용

서울 리전 온디맨드 기준입니다.

| 항목 | 비용 |
| --- | --- |
| t3.small | 시간당 약 0.026달러 |
| 공인 IPv4 | 시간당 0.005달러 |
| 디스크 | 하루 약 0.06달러 |
| 합계 | 하루 약 0.8달러, 11/8까지 6달러 미만 |

계정의 EC2 vCPU 할당량 5개 중 2개를 씁니다.

## 준비

팀원 누구나 같은 상태로 계획하고 적용합니다. 필요한 것은 다음과 같습니다.

- Docker, Go, AWS CLI v2.9 이상
- 해커톤 계정(525091405886)용 AWS CLI 프로필. 액세스 키, `aws login`, SSO 중 무엇이든 됩니다.

Terraform은 설치하지 않아도 됩니다. `tf.sh`가 공식 Docker 이미지로 실행하고, 지정한 프로필의 자격 증명만 컨테이너에 넘깁니다. 프로필이 다른 계정을 가리키면 Terraform을 실행하기 전에 멈춥니다. 프로필 이름이 `hopcharlie`가 아니면 `AWS_PROFILE`로 지정합니다.

```bash
cd deploy/aws
export AWS_PROFILE=<내 해커톤 프로필>   # 기본값 hopcharlie
./tf.sh init
./tf.sh plan
```

## 상태 파일

| 상태 | 위치 |
| --- | --- |
| 관리 호스트 (이 폴더) | `s3://hopcharlie-tfstate-525091405886-apne2/platform/management-host/terraform.tfstate` |
| 상태 버킷 자체 (`state/`) | `s3://hopcharlie-tfstate-525091405886-apne2/state/terraform.tfstate` |

- 버킷은 버전 관리, 암호화, 공개 차단, HTTPS 전용 정책이 걸려 있고, 실수로 지워지지 않게 `prevent_destroy`로 막아 두었습니다. 지난 버전은 90일 동안 남습니다.
- 잠금은 S3 잠금 파일(`use_lockfile`)로 합니다. 두 사람이 동시에 apply하면 나중 사람은 `Error acquiring the state lock`과 함께 잠금을 가진 사람(`Who:`)을 보게 됩니다. 그 사람의 작업이 끝난 뒤 다시 실행합니다. 강제로 푸는 `force-unlock`은 상대가 확실히 멈춘 경우에만 씁니다.
- 상태에는 생성한 토큰과 DB 비밀번호가 들어 있습니다. 로컬에 내려받거나 다른 곳에 복사하지 않습니다.
- 버킷 스택을 처음부터 다시 만들어야 하면 `state/backend.tf`의 주석을 따릅니다.

## 배포

```bash
./tf.sh apply -target=aws_ecr_repository.platform -target=aws_ecr_repository.console   # 이미지 저장소 먼저 (처음 한 번)
./push-image.sh                                       # 플랫폼 이미지 빌드, 푸시
./push-console.sh                                     # 콘솔 빌드, 푸시 (frontend 레포가 platform 옆에 있어야 함)
./tf.sh apply                                         # 나머지 전부
./tf.sh output api_url
```

호스트는 처음 부팅할 때 Docker를 설치하고, 이미지를 받아 Compose를 띄웁니다. 3분에서 5분쯤 걸립니다.

## 토큰

토큰은 Terraform이 만들고, 원격 상태와 SSM에만 있습니다.

```bash
./tf.sh output -json operator_tokens   # 사람마다 operator 토큰 하나
./tf.sh output -raw planner_token      # AI 플래너용 agent 토큰
./tf.sh output -raw viewer_token       # 화면용 읽기 전용 토큰
charlie login --server "$(./tf.sh output -raw api_url)"
```

각자에게는 자기 토큰만 전달합니다. Slack 채널이나 Notion에는 올리지 않습니다.

## 코드 바꾼 뒤 다시 배포

```bash
./push-image.sh     # platform 코드가 바뀌었을 때
./push-console.sh   # frontend 코드가 바뀌었을 때
./redeploy.sh       # SSM으로 호스트에서 sync.sh 실행 (설정 받기, docker compose pull, up -d)
```

## 설정 바꾸기

Terraform이 관리하는 설정(`host/compose.yaml`, `host/Caddyfile`, `examples/envs.yaml`, `examples/apps.yaml`, 변수)은 `./tf.sh apply` 다음 `./redeploy.sh`로 반영합니다.

팀이 직접 넣는 비밀값은 SSM SecureString에 두고, 바꾼 뒤 `./redeploy.sh`를 실행합니다. Terraform은 이 값을 만들기만 하고 apply할 때 덮어쓰지 않습니다. 값은 Slack이나 Notion에 올리지 않습니다.

| 파라미터 | 내용 | 넣는 방법 |
| --- | --- | --- |
| `/hopcharlie/db-passwords` | Plan이 참조하는 DB 비밀번호. 한 줄에 `이름=값` (`SRC_PGPASSWORD`, `GKE_MOIMROOM_PGPASSWORD` 등) | 아래 명령 |
| `/hopcharlie/rclone-env` | 파일 이전에 쓰는 rclone 원격. 한 줄에 `RCLONE_CONFIG_<원격>_<옵션>=값` | 아래 명령 |
| `/hopcharlie/kube/<이름>` | 클러스터마다 kubeconfig 하나 | `./put-kubeconfig.sh` |
| `/hopcharlie/tailscale-authkey` | 있으면 호스트가 Tailscale에 붙음 | 아래 명령 |

```bash
# 파일 내용을 그대로 넣는다. 파일은 넣은 뒤 지운다.
aws ssm put-parameter --profile hopcharlie --region ap-northeast-2 --overwrite \
  --name /hopcharlie/db-passwords --type SecureString --value file://db-passwords.env
aws ssm put-parameter --profile hopcharlie --region ap-northeast-2 --overwrite \
  --name /hopcharlie/rclone-env --type SecureString --value file://rclone.env
aws ssm put-parameter --profile hopcharlie --region ap-northeast-2 \
  --name /hopcharlie/tailscale-authkey --type SecureString --value 'tskey-auth-...'
./redeploy.sh
```

### 클러스터 연결

호스트에는 aws CLI나 gke-gcloud-auth-plugin 같은 인증 플러그인이 없습니다. 그래서 클러스터마다 ServiceAccount 토큰으로 된 kubeconfig를 만들어 넣습니다.

```bash
# 그 클러스터의 관리자 권한 kubectl 컨텍스트에서
../k8s/make-kubeconfig.sh gke-hopcharlie gke.kubeconfig moimroom   # 컨텍스트 이름은 examples/envs.yaml과 같게
./put-kubeconfig.sh gke gke.kubeconfig
rm gke.kubeconfig
./redeploy.sh
```

- 권한은 `../k8s/hopcharlie-platform.yaml`에 있습니다. 클러스터 전체에서는 읽기만 하고(환경 상태, 보안 뷰), 이전에 필요한 변경 권한은 앱 namespace에서만 받습니다.
- 클러스터 API가 private 주소라면, 호스트가 닿는 주소를 `SERVER=https://...`로 지정해서 kubeconfig를 만듭니다. 이때 호스트는 Tailscale로 닿습니다.
- 토큰을 끊으려면 그 클러스터에서 `kubectl delete secret hopcharlie-platform-token -n hopcharlie-system`을 실행합니다.

### Tailscale

`/hopcharlie/tailscale-authkey`가 있으면 `sync.sh`가 Tailscale을 설치하고 `hopcharlie-mgmt`라는 이름으로 tailnet에 붙습니다. 이때 API와 게이트웨이 컨테이너는 MagicDNS(100.100.100.100)로 이름을 찾으므로, Plan의 `*.ts.net` DB 주소와 게이트웨이 백엔드 주소에 닿습니다. 일회용이 아닌 재사용 가능 키를 쓰고, 호스트를 다시 만들 일이 없으면 만료 기간은 짧게 둡니다. 실제 tailnet에서 아직 확인하지 않았으므로 처음 붙일 때 `tailscale status`와 컨테이너 안의 이름 해석을 확인합니다.

## 호스트에 들어가기

```bash
aws ssm start-session --profile hopcharlie --region ap-northeast-2 --target "$(./tf.sh output -raw instance_id)"
sudo -i; cd /opt/hopcharlie; docker compose ps; docker compose logs -f api
```

- 설정 파일은 `/opt/hopcharlie/.env`, `db-passwords.env`, `rclone.env`(비밀값, 0600), `compose.yaml`, `Caddyfile`, `envs.yaml`, `apps.yaml`, `kube/<이름>`입니다. 모두 `sync.sh`가 SSM에서 받아 쓰므로 호스트에서 직접 고치지 않습니다. 직접 고친 내용은 다음 `./redeploy.sh` 때 사라집니다.
- 마지막 동기화 기록은 `/var/log/hopcharlie-sync.log`에 있습니다.
- 게이트웨이 백엔드는 Terraform 변수 `gateway_backends`입니다. 클러스터가 생기면 실제 주소로 바꾸고 `./tf.sh apply`, `./redeploy.sh` 순서로 반영합니다.
- 게이트웨이가 쓰기를 붙잡은 채 엔진이 멈췄다면, 상태를 확인한 뒤 호스트에서 직접 풀 수 있습니다.

```bash
docker compose exec gateway sh -c 'wget -qO- --header "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" --post-data "" http://localhost:9901/v1/writes/release'
```

## 정리

```bash
./tf.sh destroy
```

ECR 이미지, Elastic IP, SSM 파라미터까지 모두 지웁니다. PostgreSQL 데이터도 함께 사라집니다.

상태 버킷은 `prevent_destroy`로 막혀 있습니다. 해커톤이 끝나고 정말 지울 때만 `state/main.tf`의 `prevent_destroy`를 풀고, 버킷 안의 모든 객체 버전을 비운 뒤 `./tf.sh -chdir=state destroy`를 실행합니다. 관리 호스트를 먼저 destroy해야 합니다.
