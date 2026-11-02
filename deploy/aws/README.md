# AWS 관리 호스트

플랫폼 API, 트래픽 게이트웨이, PostgreSQL, Caddy를 EC2 한 대에 Docker Compose로 띄웁니다. Notion에서 정한 "AWS 관리 호스트"를 Terraform으로 만든 것입니다.

## 만드는 것

| 리소스 | 내용 |
| --- | --- |
| EC2 | t3.small 1대, Amazon Linux 2023, 기본 VPC, 디스크 gp3 20GiB(암호화) |
| Elastic IP | 고정 공인 IP. `api.<IP>.sslip.io`, `app.<IP>.sslip.io` 도메인으로 쓴다 |
| 보안 그룹 | 80, 443만 연다. SSH 포트는 없고 SSM Session Manager로 들어간다 |
| ECR | `hopcharlie/platform` 이미지 저장소 (최근 10개만 보관) |
| IAM | 호스트 역할: SSM 접속, ECR 읽기, `/hopcharlie/*` 파라미터 읽기 |
| SSM Parameter Store | DB 비밀번호, 게이트웨이 관리 토큰, API 토큰 (SecureString) |

호스트 안의 구성은 다음과 같습니다.

- Caddy가 Let's Encrypt 인증서로 HTTPS를 받습니다.
  - `api.<IP>.sslip.io`는 플랫폼 API로 보냅니다.
  - `app.<IP>.sslip.io`는 게이트웨이로 보냅니다.
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

## 배포

필요한 것은 Go, Docker, AWS CLI, 그리고 `hopcharlie` 프로필(해커톤 계정 525091405886)입니다. Terraform은 설치하지 않아도 됩니다. `tf.sh`가 공식 Docker 이미지로 실행하고, 이 프로필의 키만 컨테이너에 넘깁니다.

```bash
cd deploy/aws
./tf.sh init
./tf.sh apply -target=aws_ecr_repository.platform   # 이미지 저장소 먼저
./push-image.sh                                       # 이미지 빌드, 푸시
./tf.sh apply                                         # 나머지 전부
./tf.sh output api_url
```

호스트는 처음 부팅할 때 Docker를 설치하고, 이미지를 받아 Compose를 띄웁니다. 3분에서 5분쯤 걸립니다.

## 토큰

토큰은 Terraform이 만들고, 상태 파일(`terraform.tfstate`)과 SSM에만 있습니다. 상태 파일은 git에 올리지 않습니다.

```bash
./tf.sh output -json operator_tokens   # 사람마다 operator 토큰 하나
./tf.sh output -raw planner_token      # AI 플래너용 agent 토큰
./tf.sh output -raw viewer_token       # 화면용 읽기 전용 토큰
charlie login --server "$(./tf.sh output -raw api_url)"
```

각자에게는 자기 토큰만 전달합니다. Slack 채널이나 Notion에는 올리지 않습니다.

## 코드 바꾼 뒤 다시 배포

```bash
./push-image.sh
./redeploy.sh   # SSM으로 호스트에서 docker compose pull, up -d
```

## 호스트에 들어가기

```bash
aws ssm start-session --profile hopcharlie --region ap-northeast-2 --target "$(./tf.sh output -raw instance_id)"
sudo -i; cd /opt/hopcharlie; docker compose ps; docker compose logs -f api
```

- 설정 파일은 `/opt/hopcharlie/.env`(비밀값, 0600), `compose.yaml`, `Caddyfile`, `envs.yaml`입니다.
- 클러스터 kubeconfig는 `/opt/hopcharlie/kube/config`에 둡니다. 소유자는 uid 10001입니다.
- 게이트웨이 백엔드는 `.env`의 `GATEWAY_BACKENDS`에 있습니다. 클러스터가 생기면 실제 주소로 바꾸고 `docker compose up -d`를 실행합니다.
- 게이트웨이가 쓰기를 붙잡은 채 엔진이 멈췄다면, 상태를 확인한 뒤 호스트에서 직접 풀 수 있습니다.

```bash
docker compose exec gateway sh -c 'wget -qO- --header "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" --post-data "" http://localhost:9901/v1/writes/release'
```

## 정리

```bash
./tf.sh destroy
```

ECR 이미지, Elastic IP, SSM 파라미터까지 모두 지웁니다. PostgreSQL 데이터도 함께 사라집니다.
