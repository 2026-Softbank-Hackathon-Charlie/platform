#!/usr/bin/env bash
# Compatibility for application helpers; Terraform is owned by the infra repo.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
infra="${INFRA_DIR:-$root/../infra}"
target="$infra/terraform/platform/aws/tf.sh"

if [ "${1:-}" != output ]; then
  echo 'tf.sh: only output is supported here; run Terraform operations from infra/terraform/platform/aws (set INFRA_DIR to the infra repository root).' >&2
  exit 2
fi
for arg in "$@"; do
  case "$arg" in
    -chdir|-chdir=*)
      echo 'tf.sh: -chdir is not supported here; use infra/terraform/platform/aws directly.' >&2
      exit 2 ;;
  esac
done
if [ ! -f "$target" ]; then
  echo "tf.sh: infra helper not found at '$target'; set INFRA_DIR to the infra repository root." >&2
  exit 1
fi
exec "$target" "$@"
