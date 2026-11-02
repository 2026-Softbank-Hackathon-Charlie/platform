#!/usr/bin/env bash
# Pulls the latest image on the host and restarts changed containers, through
# SSM Run Command (no SSH).
#   ./redeploy.sh
set -euo pipefail
cd "$(dirname "$0")"

profile="${AWS_PROFILE:-hopcharlie}"
region="${AWS_REGION:-ap-northeast-2}"
instance="$(./tf.sh output -raw instance_id)"
registry="$(./tf.sh output -raw ecr_repository_url)"
registry="${registry%%/*}"

cmd_id="$(aws ssm send-command --profile "$profile" --region "$region" \
  --instance-ids "$instance" --document-name AWS-RunShellScript \
  --comment "hopcharlie redeploy" \
  --parameters "commands=[\"cd /opt/hopcharlie\",\"aws ecr get-login-password --region ${region} | docker login --username AWS --password-stdin ${registry}\",\"docker compose pull\",\"docker compose up -d\",\"docker compose ps\"]" \
  --query Command.CommandId --output text)"
aws ssm wait command-executed --profile "$profile" --region "$region" --command-id "$cmd_id" --instance-id "$instance" || true
aws ssm get-command-invocation --profile "$profile" --region "$region" --command-id "$cmd_id" --instance-id "$instance" \
  --query '[Status,StandardOutputContent,StandardErrorContent]' --output text
