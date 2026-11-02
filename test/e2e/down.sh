#!/usr/bin/env bash
# Removes what test/e2e/run.sh created and nothing else: its background
# processes, the kind cluster hop-e2e, the platform store container (only
# when it carries this test's label), the docker network "kind" (only when
# run.sh created it and nothing else uses it) and the work directory.
set -euo pipefail
cd "$(dirname "$0")/../.."
. test/e2e/lib.sh

stop_processes
if [ -x "$kind" ]; then
  "$kind" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
fi
# Without the kind binary (work directory already gone) remove the nodes by kind's label.
ids=$(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$CLUSTER")
[ -z "$ids" ] || docker rm -f -v $ids >/dev/null
ids=$(docker ps -aq --filter "name=^$STORE\$" --filter "label=$LABEL")
[ -z "$ids" ] || docker rm -f -v $ids >/dev/null
if [ -e "$work/created-kind-network" ] &&
  [ -z "$(docker network inspect kind -f '{{range .Containers}}x{{end}}' 2>/dev/null)" ]; then
  docker network rm kind >/dev/null 2>&1 || true
fi
rm -rf "$work"
rmdir "$root/.hopcharlie" 2>/dev/null || true
echo "== removed kind cluster $CLUSTER, container $STORE and $work"
