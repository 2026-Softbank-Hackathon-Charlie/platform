#!/usr/bin/env bash
# Stores one cluster's kubeconfig for the management host, as the SSM
# SecureString /hopcharlie/kube/<name>. The host merges every one of them
# (KUBECONFIG) on the next ./redeploy.sh. Context names must match
# infra/terraform/platform/aws/host/envs.yaml (the production source).
#   ./put-kubeconfig.sh gke ./gke-hopcharlie.kubeconfig
# Use a kubeconfig that needs no local plugin: a ServiceAccount token from
# ../k8s/make-kubeconfig.sh, not one that calls aws or gke-gcloud-auth-plugin.
set -euo pipefail
cd "$(dirname "$0")"

name="${1:?usage: put-kubeconfig.sh <name> <kubeconfig file>}"
file="${2:?usage: put-kubeconfig.sh <name> <kubeconfig file>}"
profile="${AWS_PROFILE:-hopcharlie}"
region="${AWS_REGION:-ap-northeast-2}"
if [[ ! "$name" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
  echo "put-kubeconfig.sh: name must be lowercase letters, digits and '-'" >&2
  exit 2
fi
if grep -q "exec:" "$file"; then
  echo "put-kubeconfig.sh: $file runs a credential plugin (exec:), which the host does not have; use a ServiceAccount token kubeconfig" >&2
  exit 2
fi
aws ssm put-parameter --profile "$profile" --region "$region" --overwrite \
  --name "/hopcharlie/kube/$name" --type SecureString --tier Intelligent-Tiering \
  --value "file://$file" --query Version --output text
echo "stored /hopcharlie/kube/$name; run ./redeploy.sh to apply it"
