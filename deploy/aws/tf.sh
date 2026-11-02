#!/usr/bin/env bash
# Runs Terraform from the official image, so nothing has to be installed.
#   ./tf.sh init | plan | apply | destroy | output ...
#   ./tf.sh -chdir=state plan        (the state bucket stack)
# Credentials come from the AWS CLI profile in AWS_PROFILE (default: hopcharlie).
# Any profile type works (access keys, aws login, SSO), as long as it belongs
# to the hackathon account; anything else is refused before Terraform runs.
set -euo pipefail
cd "$(dirname "$0")"

profile="${AWS_PROFILE:-hopcharlie}"
account="${HOPCHARLIE_AWS_ACCOUNT:-525091405886}"
# Mount the repository root: the configuration reads ../../examples/envs.yaml.
root="$(cd ../.. && pwd)"
if command -v cygpath >/dev/null; then # Git Bash on Windows
  root="$(cygpath -w "$root")"
fi

got="$(aws sts get-caller-identity --profile "$profile" --query Account --output text)"
if [ "$got" != "$account" ]; then
  echo "tf.sh: profile '$profile' is account $got, not the hackathon account $account" >&2
  exit 1
fi

# Pass only this profile's credentials, not the whole ~/.aws (it may hold other accounts).
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
eval "$(aws configure export-credentials --profile "$profile" --format env)"
export AWS_SESSION_TOKEN="${AWS_SESSION_TOKEN:-}"

# The hostname shows up as "Who:" in state lock messages, so teammates can see
# whose apply holds the lock.
who="$(whoami 2>/dev/null || echo user)-$(hostname 2>/dev/null || echo host)"

MSYS_NO_PATHCONV=1 exec docker run --rm -i --hostname "${who//[^A-Za-z0-9-]/-}" \
  -v "${root}:/repo" -w /repo/deploy/aws \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
  -e TF_IN_AUTOMATION=1 \
  hashicorp/terraform:1.17.1 "$@"
