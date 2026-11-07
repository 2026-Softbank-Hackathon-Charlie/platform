#!/usr/bin/env bash
# Builds the platform image from this checkout and pushes it to ECR as
# :<git sha> and :latest.
#   ./push-image.sh
# Needs Go, Docker and the AWS CLI. AWS_PROFILE defaults to hopcharlie.
set -euo pipefail
cd "$(dirname "$0")/../.."

profile="${AWS_PROFILE:-hopcharlie}"
region="${AWS_REGION:-ap-northeast-2}"
repo="$(aws ecr describe-repositories --profile "$profile" --region "$region" \
  --repository-names hopcharlie/platform --query 'repositories[0].repositoryUri' --output text)"
registry="${repo%%/*}"
tag="$(git rev-parse --short HEAD)"
if [ -n "$(git status --porcelain -- cmd internal pkg api go.mod go.sum)" ]; then
  tag="${tag}-dirty"
fi

mkdir -p deploy/docker/bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o deploy/docker/bin/hopcharlie-api ./cmd/api
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o deploy/docker/bin/hopcharlie-gateway ./cmd/gateway
for arch in amd64 arm64; do
  mkdir -p deploy/docker/bin/onprem-agent
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "deploy/docker/bin/onprem-agent/$arch" ./cmd/onprem-agent
done

docker build --platform linux/amd64 -t "${repo}:${tag}" -t "${repo}:latest" deploy/docker
aws ecr get-login-password --profile "$profile" --region "$region" | docker login --username AWS --password-stdin "$registry"
docker push "${repo}:${tag}"
docker push "${repo}:latest"
echo "pushed ${repo}:${tag} (also :latest)"
