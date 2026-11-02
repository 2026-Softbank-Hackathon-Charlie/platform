# Shared by run.sh and down.sh. Sourced from the platform repository root.
# Ports also appear in kind.yaml, nodeports.yaml, minio.yaml, plan.json and
# decommission.json; change them together.

KIND_VERSION=v0.33.0
CNPG_VERSION=1.25.1
STORE_IMAGE=postgres:17.11-alpine@sha256:266d3c257594e195b309ae9d1805e54c788f1d7b375f56bf6c367aac6c917e7b
MINIO_IMAGE=pgsty/minio:RELEASE.2026-08-04T00-00-00Z # the image moimroom's compose.yaml uses
APP_IMAGE=moimroom:e2e

CLUSTER=hop-moimroom            # kind cluster; kube context kind-hop-moimroom
NS=moimroom
STORE=hop-moimroom-store        # platform store container
LABEL=hopcharlie-moimroom-e2e   # on every container this test creates

SRC_DB_PORT=55742               # moimroom-src-db through a kind NodePort
DST_DB_PORT=55743
SRC_S3_PORT=55762               # minio-src
DST_S3_PORT=55763
SRC_APP_PORT=18201              # moimroom-src release
DST_APP_PORT=18202
STORE_PORT=55752
API_PORT=18280
GW_PORT=18200
GW_ADMIN_PORT=19902

root=$(pwd -W 2>/dev/null || pwd)
work=$root/.hopcharlie/e2e-moimroom # logs, binaries, kubeconfig, engine state (gitignored)
bin=$work/bin
exe=$(go env GOEXE)
kind="$bin/kind$exe"
# Sibling checkouts in the workspace: the app (image and Helm chart) and the
# engine (hopmigrate decommission is not in the platform API yet).
MOIMROOM_DIR=${MOIMROOM_DIR:-$root/../moimroom}
ENGINE_DIR=${ENGINE_DIR:-$root/../migration-engine}

stop_processes() {
  local f pid
  for f in "$work"/*.pid; do
    [ -e "$f" ] || continue
    pid=$(cat "$f")
    kill "$pid" 2>/dev/null || true
    rm -f "$f"
  done
}
