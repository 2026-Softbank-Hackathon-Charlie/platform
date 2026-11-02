#!/usr/bin/env bash
# Runs Terraform from the official image, so nothing has to be installed.
#   ./tf.sh init | plan | apply | destroy | output ...
# Credentials come from the AWS CLI profile in AWS_PROFILE (default: hopcharlie).
set -euo pipefail
cd "$(dirname "$0")"

profile="${AWS_PROFILE:-hopcharlie}"
# Mount the repository root: the configuration reads ../../examples/envs.yaml.
root="$(cd ../.. && pwd)"
if command -v cygpath >/dev/null; then # Git Bash on Windows
  root="$(cygpath -w "$root")"
fi

# Pass only this profile's keys, not the whole ~/.aws (it may hold other accounts).
AWS_ACCESS_KEY_ID="$(aws configure get aws_access_key_id --profile "$profile")"
AWS_SECRET_ACCESS_KEY="$(aws configure get aws_secret_access_key --profile "$profile")"
AWS_SESSION_TOKEN="$(aws configure get aws_session_token --profile "$profile" || true)"
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN

MSYS_NO_PATHCONV=1 exec docker run --rm -i \
  -v "${root}:/repo" -w /repo/deploy/aws \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
  -e TF_IN_AUTOMATION=1 \
  hashicorp/terraform:1.17.1 "$@"
