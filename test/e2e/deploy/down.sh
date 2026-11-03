#!/usr/bin/env bash
# Removes what test/e2e/deploy/run.sh created: its processes, the kind
# cluster, the registry container (only if this test labelled it), the docker
# network "kind" (only if run.sh created it and nothing else uses it) and the
# work directory.
set -euo pipefail
cd "$(dirname "$0")/../../.."
. test/e2e/deploy/lib.sh

stop_processes
if [ -x "$kind" ]; then
  "$kind" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
fi
ids=$(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$CLUSTER")
[ -z "$ids" ] || docker rm -f -v $ids >/dev/null
ids=$(docker ps -aq --filter "name=^$REGISTRY\$" --filter "label=$LABEL")
[ -z "$ids" ] || docker rm -f -v $ids >/dev/null
if [ -e "$work/created-kind-network" ] &&
  [ -z "$(docker network inspect kind -f '{{range .Containers}}x{{end}}' 2>/dev/null)" ]; then
  docker network rm kind >/dev/null 2>&1 || true
fi
rm -rf "$work"
rmdir "$root/.hopcharlie" 2>/dev/null || true
echo "== removed kind cluster $CLUSTER, container $REGISTRY and $work"
