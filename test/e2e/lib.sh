# Shared by run.sh and down.sh. Sourced from the repository root.
# The database ports also appear in kind.yaml and plan.json; change them together.

KIND_VERSION=v0.33.0
CNPG_VERSION=1.25.1
STORE_IMAGE=postgres:17.11-alpine@sha256:266d3c257594e195b309ae9d1805e54c788f1d7b375f56bf6c367aac6c917e7b

CLUSTER=hop-e2e                 # kind cluster; kube context kind-hop-e2e
STORE=hop-e2e-store             # platform store container
LABEL=hopcharlie-e2e            # on every container this test creates

SRC_DB_PORT=55642               # pg-src through a kind NodePort
DST_DB_PORT=55643               # pg-dst
STORE_PORT=55652
API_PORT=18080
GW_PORT=18000
GW_ADMIN_PORT=19901
SRC_APP_PORT=18101
DST_APP_PORT=18102

# Absolute path that native Windows programs (go build output, kind, kubectl)
# also understand: pwd -W exists only in Git Bash / MSYS.
root=$(pwd -W 2>/dev/null || pwd)
work=$root/.hopcharlie/e2e      # logs, binaries, kubeconfig, engine state (gitignored)
bin=$work/bin
exe=$(go env GOEXE)
kind="$bin/kind$exe"

# stop_processes kills the background processes run.sh started.
stop_processes() {
  local f pid
  for f in "$work"/*.pid; do
    [ -e "$f" ] || continue
    pid=$(cat "$f")
    kill "$pid" 2>/dev/null || true
    rm -f "$f"
  done
}
