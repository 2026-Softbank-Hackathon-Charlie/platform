#!/usr/bin/env bash
# E2E: migrate a CNPG database under load, driven through the platform API only.
#
# kind runs one cluster with two CNPG clusters (pg-src, pg-dst), like the
# engine rehearsal. A throwaway container is the platform store. On the host
# run the platform API (engine in-process), the gateway, one test app per
# environment (test/e2e/app) and the driver (test/e2e/driver), which sends the
# load and plays the planner and the operator. See test/e2e/README.md.
#
# Needs: docker, kubectl, go, curl. Run from anywhere:
#   test/e2e/run.sh          # run, then remove everything (test/e2e/down.sh)
#   KEEP=1 test/e2e/run.sh   # leave everything running for inspection
set -euo pipefail
cd "$(dirname "$0")/../.."
here=test/e2e
. "$here/lib.sh"
export KUBECONFIG="$work/kubeconfig" # never touches ~/.kube/config
began=$(date +%s)
k() { kubectl -n hop "$@"; }
rand() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }
busy() { (echo >"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
wait_http() { # url [seconds]
  local i
  for i in $(seq 1 "${2:-60}"); do
    curl -s -o /dev/null "$1" && return 0
    sleep 1
  done
  echo "no answer from $1" >&2
  return 1
}
retry() { # attempts command...; quiet until the last attempt
  local n=$1 i
  shift
  for i in $(seq 2 "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 2; done
  "$@"
}
start() { # name command...; the environment comes from the caller
  local name=$1
  shift
  "$@" >"$work/$name.log" 2>&1 &
  echo $! >"$work/$name.pid"
}

finish() {
  local code=$?
  if [ "$code" != 0 ]; then
    for f in api gateway app-src app-dst; do
      [ -s "$work/$f.log" ] && { echo "== last lines of $f.log"; tail -n 25 "$work/$f.log"; }
    done
  fi
  echo "== wall time $(($(date +%s) - began))s"
  if [ -n "${KEEP:-}" ]; then
    echo "KEEP=1: still running, logs in $work. Remove with $here/down.sh"
  else
    "$here/down.sh"
  fi
  exit "$code"
}
trap finish EXIT

echo "== build (work dir $work)"
# Leftovers of an earlier KEEP=1 run. The kind cluster is reused.
stop_processes
if [ -n "$(docker ps -aq --filter "name=^$STORE\$")" ]; then
  if [ -z "$(docker ps -aq --filter "name=^$STORE\$" --filter "label=$LABEL")" ]; then
    echo "container $STORE exists but this test did not create it" >&2
    exit 1
  fi
  docker rm -f -v "$STORE" >/dev/null
fi
rm -rf "$work/runs" "$work/gateway-state.json" "$work"/*.log
mkdir -p "$bin"
GOBIN="$bin" go install "sigs.k8s.io/kind@$KIND_VERSION"
for c in api gateway; do go build -o "$bin/$c$exe" "./cmd/$c"; done
for c in app driver; do go build -o "$bin/$c$exe" "./$here/$c"; done
for p in $API_PORT $GW_PORT $GW_ADMIN_PORT $SRC_APP_PORT $DST_APP_PORT $STORE_PORT; do
  if busy "$p"; then echo "port $p is in use (see $here/lib.sh)" >&2; exit 1; fi
done

echo "== kind cluster $CLUSTER, CNPG $CNPG_VERSION"
if ! "$kind" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  for p in $SRC_DB_PORT $DST_DB_PORT; do
    if busy "$p"; then echo "port $p is in use (see $here/lib.sh)" >&2; exit 1; fi
  done
  docker network inspect kind >/dev/null 2>&1 || touch "$work/created-kind-network"
  "$kind" create cluster --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG"
fi
"$kind" export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
kubectl apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml" >/dev/null
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=300s

echo "== source pg-src with seed data"
kubectl create namespace hop --dry-run=client -o yaml | kubectl apply -f - >/dev/null
k delete cluster --all --wait=true >/dev/null
k delete pvc --all --wait=true >/dev/null
k apply -f "$here/nodeports.yaml" >/dev/null
# The CNPG webhook may refuse requests for a few seconds after the rollout.
retry 30 k apply -f "$here/source.yaml" >/dev/null
k wait cluster/pg-src --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=600s
k wait pod/pg-src-1 --for=condition=Ready --timeout=300s
k exec pg-src-1 -c postgres -- psql -q -d app -v ON_ERROR_STOP=1 -c "SET ROLE app;
  CREATE TABLE items (id text PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now());
  INSERT INTO items (id) SELECT 'seed-' || g FROM generate_series(1, 20000) g;"
# target.yaml bootstraps pg-dst with this secret, so both DSNs use one password.
pgpass=$(k get secret pg-src-app -o jsonpath='{.data.password}' | base64 -d)
dsn() { echo "postgres://app:$pgpass@127.0.0.1:$1/app?sslmode=require"; }

echo "== platform store $STORE on 127.0.0.1:$STORE_PORT"
store_pw=$(rand 16)
docker run -d --name "$STORE" --label "$LABEL=1" -p "127.0.0.1:$STORE_PORT:5432" \
  -e POSTGRES_USER=platform -e POSTGRES_PASSWORD="$store_pw" -e POSTGRES_DB=platform "$STORE_IMAGE" >/dev/null
# -h waits for the final server, not the one the image runs during initdb.
retry 30 docker exec "$STORE" pg_isready -q -h 127.0.0.1 -U platform

echo "== test apps, gateway, platform API"
op_token=$(rand 24)
agent_token=$(rand 24)
gw_token=$(rand 24)
APP_LISTEN=127.0.0.1:$SRC_APP_PORT APP_DSN=$(dsn "$SRC_DB_PORT") start app-src "$bin/app$exe"
APP_LISTEN=127.0.0.1:$DST_APP_PORT APP_DSN=$(dsn "$DST_DB_PORT") start app-dst "$bin/app$exe"
GATEWAY_ADMIN_TOKEN=$gw_token \
  GATEWAY_BACKENDS="src=http://127.0.0.1:$SRC_APP_PORT,dst=http://127.0.0.1:$DST_APP_PORT" \
  GATEWAY_INITIAL_BACKEND=src \
  GATEWAY_LISTEN=127.0.0.1:$GW_PORT GATEWAY_ADMIN_ADDR=127.0.0.1:$GW_ADMIN_PORT \
  GATEWAY_STATE_FILE=$work/gateway-state.json \
  start gateway "$bin/gateway$exe"
PLATFORM_DATABASE_URL="postgres://platform:$store_pw@127.0.0.1:$STORE_PORT/platform?sslmode=disable" \
  PLATFORM_TOKENS="operator:operator:$op_token,planner:agent:$agent_token" \
  PLATFORM_ENVS_FILE=$root/$here/envs.yaml \
  PLATFORM_GATEWAY_URL=http://127.0.0.1:$GW_ADMIN_PORT GATEWAY_TOKEN=$gw_token \
  PLATFORM_DSN_HOST_SUFFIXES=127.0.0.1 \
  PLATFORM_ADDR=127.0.0.1:$API_PORT PLATFORM_STATE_DIR=$work/runs \
  SRC_PGPASSWORD=$pgpass DST_PGPASSWORD=$pgpass \
  start api "$bin/api$exe"
wait_http "http://127.0.0.1:$SRC_APP_PORT/healthz"
wait_http "http://127.0.0.1:$DST_APP_PORT/healthz" # 503 until the engine creates pg-dst
wait_http "http://127.0.0.1:$GW_ADMIN_PORT/healthz"
wait_http "http://127.0.0.1:$API_PORT/healthz"

echo "== migrate under load"
E2E_AGENT_TOKEN=$agent_token E2E_OPERATOR_TOKEN=$op_token E2E_TARGET_DSN=$(dsn "$DST_DB_PORT") \
  "$bin/driver$exe" -api "http://127.0.0.1:$API_PORT" -gateway "http://127.0.0.1:$GW_PORT" \
  -plan "$here/plan.json" -manifest "$here/target.yaml"
