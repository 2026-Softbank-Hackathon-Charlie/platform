#!/usr/bin/env bash
# Builds the console (frontend repo) against this deployment's API and pushes
# it to ECR as hopcharlie/console:<frontend sha> and :latest. Then ./redeploy.sh.
#   ./push-console.sh                      # frontend checked out next to platform
#   FRONTEND_DIR=/path/to/frontend ./push-console.sh
#   API_URL=https://api.hopcharlie.dev ./push-console.sh   # the management EKS cluster
# Needs Node, Docker and the AWS CLI. AWS_PROFILE defaults to hopcharlie.
set -euo pipefail
cd "$(dirname "$0")"

profile="${AWS_PROFILE:-hopcharlie}"
region="${AWS_REGION:-ap-northeast-2}"
frontend="$(cd "${FRONTEND_DIR:-../../../frontend}" && pwd)"
api_url="${API_URL:-$(./tf.sh output -raw api_url)}"
repo="$(./tf.sh output -raw console_repository_url)"
registry="${repo%%/*}"
tag="$(git -C "$frontend" rev-parse --short HEAD)"
if [ -n "$(git -C "$frontend" status --porcelain -- src index.html package.json package-lock.json)" ]; then
  tag="${tag}-dirty"
fi

(cd "$frontend" && npm ci && VITE_PLATFORM_URL="$api_url" npm run build)
docker build --platform linux/amd64 -t "${repo}:${tag}" -t "${repo}:latest" -f ../console/Dockerfile "$frontend/dist"
aws ecr get-login-password --profile "$profile" --region "$region" | docker login --username AWS --password-stdin "$registry"
docker push "${repo}:${tag}"
docker push "${repo}:latest"
echo "pushed ${repo}:${tag} (also :latest), built for ${api_url}"
