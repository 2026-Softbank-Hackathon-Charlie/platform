# AWS 애플리케이션 배포 도우미

Terraform과 운영 호스트 설정은 [infra/terraform/platform/aws](https://github.com/2026-Softbank-Hackathon-Charlie/infra/tree/main/terraform/platform/aws)로 이전했습니다. 관리 호스트·EKS Pod Identity(루트), 독립된 상태 버킷 스택(`state/`), ACM 인증서·Cloudflare 레코드(`dns/`), SSM 설정, 토큰, 클러스터 연결 및 정리 절차는 그 디렉터리의 README를 따릅니다. [이전·검증 절차](https://github.com/2026-Softbank-Hackathon-Charlie/infra/blob/main/docs/platform-terraform-migration.md)도 infra에서 관리합니다.

이 폴더는 애플리케이션 이미지 빌드·푸시와 호스트 재배포 도우미만 소유합니다. 기존 로컬 Terraform 캐시, 상태, tfvars 파일은 계속 Git에서 제외합니다. 이 파일들을 새 위치로 복사하거나 이전 디렉터리에서 Terraform을 직접 실행하지 않습니다.

## 체크아웃 연결

기본 배치는 `platform`과 `infra`가 같은 상위 폴더에 있는 형태입니다. 플랫폼 폴더 이름은 달라도 됩니다. 다른 위치에 체크아웃했다면 infra **레포 루트**를 지정합니다. 공백이 있는 경로도 지원합니다.

```bash
# platform 레포 루트에서
export INFRA_DIR="/path/to/infra checkout" # 기본값: platform 레포 루트의 ../infra
export AWS_PROFILE=<내 해커톤 프로필>     # 기본값 hopcharlie
cd deploy/aws
./tf.sh output -raw api_url
```

`tf.sh`는 `output`과 그 인수만 infra의 `terraform/platform/aws/tf.sh`로 전달합니다. 환경 변수와 종료 코드도 그대로 전달합니다. infra helper가 없으면 AWS나 Docker를 실행하기 전에 실패합니다. `init`, `plan`, `apply`, `destroy`, `-chdir` 등 다른 명령은 새 실행 위치를 안내하고 거부합니다.

인프라 명령은 infra에서 실행합니다. 호환 스크립트는 상태를 이동하거나 초기화하지 않습니다.

```bash
# platform 레포 루트에서, 기본 sibling 배치일 때
cd ../infra/terraform/platform/aws
./tf.sh init
./tf.sh plan
# apply와 state/, dns/ 절차는 infra README의 검토·운영 절차를 따른다.
```

## 애플리케이션 코드 배포

관리 EKS에 내보낼 플랫폼 이미지는 GitHub Actions의 **Publish platform image**(`.github/workflows/publish-image.yml`)가 만듭니다. main에 push해서 `ci`가 통과하면 자동으로 실행되고, 이미지를 ECR에 `:<커밋 7자리>`로 올린 뒤 infra main에 그 이미지를 고정하는 커밋을 바로 넣습니다. 연달아 push하면 마지막 커밋만 올라갑니다. Argo CD가 infra main을 배포합니다(infra ADR 0024). 필요한 Actions 변수와 secret은 workflow 파일 머리말에 있습니다. 콘솔 이미지는 frontend 저장소의 **Deploy frontend** workflow가 같은 방식으로 만듭니다.

아래 도우미는 workflow를 쓸 수 없을 때와 EC2 호스트용입니다. 이 도우미는 실제 AWS에 쓰기 작업을 합니다. Docker와 AWS CLI가 필요하고, 플랫폼 이미지는 Go, 콘솔 이미지는 Node도 필요합니다. 콘솔 소스는 기본적으로 platform 옆의 `frontend`이며, 다른 경로는 `FRONTEND_DIR`로 지정합니다.

```bash
# platform/deploy/aws에서
./push-image.sh     # 플랫폼 이미지 빌드·ECR 푸시
bash ./push-console.sh   # 콘솔 이미지 빌드·ECR 푸시
./redeploy.sh       # SSM에서 운영 설정을 받아 pull, up -d
```

관리 EKS API 주소로 콘솔을 빌드할 때는 `API_URL=https://api.hopcharlie.dev bash ./push-console.sh`를 씁니다. 이 override는 API 주소만 바꾸며 ECR 주소는 계속 infra 출력에서 읽습니다. 관리 EKS 배포와 이미지 digest 반영은 infra의 Argo CD 운영 절차를 따릅니다.

`push-console.sh`와 `redeploy.sh`는 output 호환 스크립트로 infra의 출력을 읽습니다. `put-kubeconfig.sh <이름> <파일>`은 팀이 제공한 kubeconfig를 SSM SecureString에 저장합니다. 클러스터 권한과 토큰 생성 절차는 infra README 및 [플랫폼 Kubernetes 도우미](../k8s/)를 따릅니다.

## 로컬 예시와 운영 설정

| 목적 | 소유 위치 |
| --- | --- |
| 로컬 API 개발 예시 | platform의 `examples/envs.yaml`, `examples/apps.yaml` |
| 운영 환경·앱 설정 | infra의 `terraform/platform/aws/host/envs.yaml`, `host/apps.yaml` |
| 운영 Compose·Caddy·동기화 스크립트 | infra의 `terraform/platform/aws/host/` |
| 관리 호스트·EKS Pod Identity Terraform | infra의 `terraform/platform/aws/` |
| 독립 상태 버킷·ACM/Cloudflare 스택 | infra의 `terraform/platform/aws/state/`, `dns/` |

운영 설정 변경은 infra에서 검토하고 적용한 뒤 이 폴더의 `./redeploy.sh`로 반영합니다. 플랫폼의 examples를 고쳐도 운영 SSM payload는 바뀌지 않습니다. 호스트 파일은 SSM에서 다시 받으므로 호스트에서 직접 고치지 않습니다. 비밀값은 기존 SSM 운영 절차를 따르고 Git이나 로그에 넣지 않습니다.
