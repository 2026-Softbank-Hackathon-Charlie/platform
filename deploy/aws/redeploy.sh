#!/usr/bin/env bash
# Applies the current SSM settings on the host and restarts what changed,
# through SSM Run Command (no SSH): fetches host/sync.sh and runs it.
#   ./redeploy.sh
# Use it after ./push-image.sh, ./push-console.sh, an infra Terraform apply, or after
# changing a team secret (db-passwords, rclone-env, kube/*, tailscale-authkey).
set -euo pipefail
cd "$(dirname "$0")"

profile="${AWS_PROFILE:-hopcharlie}"
region="${AWS_REGION:-ap-northeast-2}"
prefix="/hopcharlie"
instance="$(./tf.sh output -raw instance_id)"

cmd_id="$(aws ssm send-command --profile "$profile" --region "$region" \
  --instance-ids "$instance" --document-name AWS-RunShellScript \
  --comment "hopcharlie sync" \
  --parameters "commands=[\"aws ssm get-parameter --region ${region} --name ${prefix}/host/sync.sh --query Parameter.Value --output text > /opt/hopcharlie/sync.sh\",\"bash /opt/hopcharlie/sync.sh ${region} ${prefix}\"]" \
  --query Command.CommandId --output text)"
aws ssm wait command-executed --profile "$profile" --region "$region" --command-id "$cmd_id" --instance-id "$instance" || true
aws ssm get-command-invocation --profile "$profile" --region "$region" --command-id "$cmd_id" --instance-id "$instance" \
  --query '[Status,StandardOutputContent,StandardErrorContent]' --output text
