# Shared by run.sh and down.sh. Sourced from the platform repository root.
# The git server and proxy ports also appear in kind.yaml and kit.yaml;
# change them together.

KIND_VERSION=v0.33.0
ARGOCD_VERSION=v3.5.3           # what production runs
CNPG_VERSION=1.25.1             # as test/e2e and test/e2e/moimroom
MINIO_IMAGE=pgsty/minio:RELEASE.2026-08-04T00-00-00Z # as test/e2e/moimroom
REGISTRY_IMAGE=registry:3.0.0@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e
STORE_IMAGE=postgres:17.11-alpine@sha256:266d3c257594e195b309ae9d1805e54c788f1d7b375f56bf6c367aac6c917e7b

CLUSTER=hop-deploy              # kind cluster; kube context kind-hop-deploy
REGISTRY=hop-deploy-registry    # local registry container, on the kind network
STORE=hop-deploy-store          # platform store container
LABEL=hopcharlie-deploy-e2e     # on every container this test creates
ENV_NAME=kind                   # the environment: apps/<app>/kind.yaml

REGISTRY_PORT=18393             # images are localhost:$REGISTRY_PORT/... on the host and in kind
GIT_PORT=18390                  # git server (NodePort 30390)
PROXY_PORT=18391                # proxy into the cluster (NodePort 30391)
GW_PORT=18300
GW_ADMIN_PORT=19903
API_PORT=18380                  # platform API
GITHUB_PORT=18392               # fakegithub
STORE_PORT=55852
APP_DOMAIN=e2e.test             # gateway -app-domain: <app>.e2e.test

root=$(pwd -W 2>/dev/null || pwd)
work=$root/.hopcharlie/e2e-deploy # logs, binaries, kubeconfig, repositories (gitignored)
bin=$work/bin
exe=$(go env GOEXE)
kind="$bin/kind$exe"
MOIMROOM_DIR=${MOIMROOM_DIR:-$root/../moimroom}

stop_processes() {
  local f pid
  for f in "$work"/*.pid; do
    [ -e "$f" ] || continue
    pid=$(cat "$f")
    kill "$pid" 2>/dev/null || true
    rm -f "$f"
  done
}
