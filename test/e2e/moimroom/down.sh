#!/usr/bin/env bash
# Removes what test/e2e/moimroom/run.sh created: processes, the kind cluster,
# the platform store container (only if this test labelled it) and the work dir.
set -euo pipefail
cd "$(dirname "$0")/../../.."
. test/e2e/moimroom/lib.sh

stop_processes
[ -x "$kind" ] && "$kind" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
if [ -n "$(docker ps -aq --filter "name=^$STORE\$" --filter "label=$LABEL")" ]; then
  docker rm -f -v "$STORE" >/dev/null
fi
if [ -e "$work/created-kind-network" ] && [ -z "$(docker network inspect kind -f '{{range .Containers}}x{{end}}' 2>/dev/null)" ]; then
  docker network rm kind >/dev/null 2>&1 || true
fi
rm -rf "$work"
echo "== removed kind cluster $CLUSTER, container $STORE and $work"
