#!/usr/bin/env bash
# E2E with the moimroom sample app: migrate its database and its photos under
# load through the platform API, then decommission the source with the engine.
#
# kind runs one cluster with, per environment, a CNPG cluster, a MinIO and a
# moimroom Helm release (moimroom-src, moimroom-dst). On the host run the
# platform API (engine and rclone in-process), the gateway and the driver
# (test/e2e/moimroom/driver), which books rooms, uploads photos and reads them
# back through the gateway while it drives the migration. See README.md.
#
# Needs: docker, kubectl, helm, rclone, go, curl, and the moimroom and
# migration-engine checkouts next to this repository (MOIMROOM_DIR, ENGINE_DIR).
#   test/e2e/moimroom/run.sh          # run, then remove everything
#   KEEP=1 test/e2e/moimroom/run.sh   # leave everything running
set -euo pipefail
cd "$(dirname "$0")/../../.."
here=test/e2e/moimroom
. "$here/lib.sh"
export KUBECONFIG="$work/kubeconfig" # never touches ~/.kube/config
began=$(date +%s)
k() { kubectl -n "$NS" "$@"; }
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
start() { # name command...
  local name=$1
  shift
  "$@" >"$work/$name.log" 2>&1 &
  echo $! >"$work/$name.pid"
}
# serve name health-url command...: start a server and wait until it answers.
# On Windows, listen sometimes fails once with "invalid pointer address"
# (WSAEFAULT) on a free port; a fresh process then binds fine, so retry.
serve() {
  local name=$1 url=$2 i
  shift 2
  for i in 1 2 3; do
    start "$name" "$@"
    if wait_http "$url" 20 2>/dev/null; then return 0; fi
    echo "$name did not come up (attempt $i): $(tail -n 1 "$work/$name.log")" >&2
    kill "$(cat "$work/$name.pid")" 2>/dev/null || true
    sleep 1
  done
  return 1
}

finish() {
  local code=$?
  if [ "$code" != 0 ]; then
    for f in api gateway; do
      [ -s "$work/$f.log" ] && { echo "== last lines of $f.log"; tail -n 25 "$work/$f.log"; }
    done
    for r in moimroom-src moimroom-dst; do
      echo "== last lines of $r"; k logs "deploy/$r" --tail=15 2>/dev/null || true
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

for t in docker kubectl helm rclone go curl; do
  command -v "$t" >/dev/null || { echo "$t is not on PATH" >&2; exit 1; }
done
for d in "$MOIMROOM_DIR" "$ENGINE_DIR"; do
  [ -d "$d" ] || { echo "$d not found (set MOIMROOM_DIR / ENGINE_DIR)" >&2; exit 1; }
done

echo "== build (work dir $work)"
stop_processes
if [ -n "$(docker ps -aq --filter "name=^$STORE\$")" ]; then
  if [ -z "$(docker ps -aq --filter "name=^$STORE\$" --filter "label=$LABEL")" ]; then
    echo "container $STORE exists but this test did not create it" >&2
    exit 1
  fi
  docker rm -f -v "$STORE" >/dev/null
fi
rm -rf "$work/runs" "$work/gateway-state.json" "$work"/*.log "$work/run-id"
mkdir -p "$bin"
GOBIN="$bin" go install "sigs.k8s.io/kind@$KIND_VERSION"
for c in api gateway; do go build -o "$bin/$c$exe" "./cmd/$c"; done
go build -o "$bin/driver$exe" "./$here/driver"
(cd "$ENGINE_DIR" && go build -o "$bin/hopmigrate$exe" ./cmd/hopmigrate)
docker build -q -t "$APP_IMAGE" "$MOIMROOM_DIR" >/dev/null
for p in $API_PORT $GW_PORT $GW_ADMIN_PORT $STORE_PORT; do
  if busy "$p"; then echo "port $p is in use (see $here/lib.sh)" >&2; exit 1; fi
done

echo "== kind cluster $CLUSTER, CNPG $CNPG_VERSION"
if ! "$kind" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  docker network inspect kind >/dev/null 2>&1 || touch "$work/created-kind-network"
  "$kind" create cluster --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG"
fi
"$kind" export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
"$kind" load docker-image "$APP_IMAGE" --name "$CLUSTER" >/dev/null
kubectl apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml" >/dev/null
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=300s

echo "== namespace $NS: object stores"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm uninstall -n "$NS" moimroom-src moimroom-dst >/dev/null 2>&1 || true
k delete networkpolicy,cluster --all --wait=true >/dev/null
k delete pvc --all --wait=true >/dev/null
s3_user=moimroom
s3_key=$(rand 16)
k create secret generic minio-root --from-literal=MINIO_ROOT_USER=$s3_user --from-literal=MINIO_ROOT_PASSWORD="$s3_key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed "s|MINIO_IMAGE|$MINIO_IMAGE|" "$here/minio.yaml" | k apply -f - >/dev/null
k rollout restart deploy/minio-src deploy/minio-dst >/dev/null # new credentials, empty buckets
k rollout status deploy/minio-src --timeout=300s
k rollout status deploy/minio-dst --timeout=300s
# rclone remotes "src" and "dst": read by this script and by the platform API (engine).
for r in SRC:$SRC_S3_PORT DST:$DST_S3_PORT; do
  n=${r%%:*}
  export RCLONE_CONFIG_${n}_TYPE=s3 RCLONE_CONFIG_${n}_PROVIDER=Minio \
    RCLONE_CONFIG_${n}_ENDPOINT=http://127.0.0.1:${r##*:} \
    RCLONE_CONFIG_${n}_ACCESS_KEY_ID=$s3_user RCLONE_CONFIG_${n}_SECRET_ACCESS_KEY=$s3_key
done
retry 30 rclone mkdir src:moimroom-photos
retry 30 rclone mkdir dst:moimroom-photos

echo "== databases: moimroom-src-db (primary), moimroom-dst-db (replica, prepared like infra would)"
k apply -f "$here/nodeports.yaml" >/dev/null
retry 30 k apply -f "$here/source.yaml" >/dev/null
k wait cluster/moimroom-src-db --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=600s
k wait pod/moimroom-src-db-1 --for=condition=Ready --timeout=300s
retry 30 k apply -f "$here/target.yaml" >/dev/null
k wait cluster/moimroom-dst-db --for=jsonpath='{.status.phase}'="Cluster in healthy state" --timeout=600s
pgpass=$(k get secret moimroom-src-db-app -o jsonpath='{.data.password}' | base64 -d)
dsn() { echo "postgres://app:$pgpass@127.0.0.1:$1/app?sslmode=require"; }

echo "== moimroom releases (Helm chart $MOIMROOM_DIR/deploy/helm/moimroom)"
host_code=$(rand 12)
for env in src dst; do
  k create secret generic "moimroom-$env-dburl" \
    --from-literal=uri="postgres://app:$pgpass@moimroom-$env-db-rw.$NS.svc:5432/app?sslmode=require" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  k create secret generic "moimroom-$env-s3" --from-literal=accessKey=$s3_user --from-literal=secretKey="$s3_key" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  k create secret generic "moimroom-$env-app" --from-literal=hostCode="$host_code" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  seed=false
  [ "$env" = src ] && seed=true
  helm upgrade --install -n "$NS" "moimroom-$env" "$MOIMROOM_DIR/deploy/helm/moimroom" --wait --timeout 5m \
    --set replicaCount=1 --set image.repository=moimroom --set image.tag=e2e --set image.pullPolicy=Never \
    --set siteRegionLabel="$env" --set seedSpaces=$seed \
    --set database.secretName="moimroom-$env-dburl" \
    --set s3.endpoint="http://minio-$env.$NS.svc:9000" --set s3.region=us-east-1 \
    --set s3.bucket=moimroom-photos --set s3.pathStyle=true >/dev/null
done
wait_http "http://127.0.0.1:$SRC_APP_PORT/readyz"
wait_http "http://127.0.0.1:$DST_APP_PORT/readyz" # the target app runs on the replica

echo "== platform store $STORE on 127.0.0.1:$STORE_PORT"
store_pw=$(rand 16)
for i in 1 2 3; do # the same Windows bind error as in serve() can hit Docker's port proxy
  docker run -d --name "$STORE" --label "$LABEL=1" -p "127.0.0.1:$STORE_PORT:5432" \
    -e POSTGRES_USER=platform -e POSTGRES_PASSWORD="$store_pw" -e POSTGRES_DB=platform "$STORE_IMAGE" >/dev/null && break
  docker rm -f -v "$STORE" >/dev/null 2>&1 || true
  [ "$i" = 3 ] && exit 1
  sleep 2
done
retry 30 docker exec "$STORE" pg_isready -q -h 127.0.0.1 -U platform

echo "== gateway, platform API"
op_token=$(rand 24)
agent_token=$(rand 24)
gw_token=$(rand 24)
GATEWAY_ADMIN_TOKEN=$gw_token \
  GATEWAY_BACKENDS="src=http://127.0.0.1:$SRC_APP_PORT,dst=http://127.0.0.1:$DST_APP_PORT" \
  GATEWAY_INITIAL_BACKEND=src \
  GATEWAY_LISTEN=127.0.0.1:$GW_PORT GATEWAY_ADMIN_ADDR=127.0.0.1:$GW_ADMIN_PORT \
  GATEWAY_STATE_FILE=$work/gateway-state.json \
  serve gateway "http://127.0.0.1:$GW_ADMIN_PORT/healthz" "$bin/gateway$exe"
PLATFORM_DATABASE_URL="postgres://platform:$store_pw@127.0.0.1:$STORE_PORT/platform?sslmode=disable" \
  PLATFORM_TOKENS="operator:operator:$op_token,planner:agent:$agent_token" \
  PLATFORM_ENVS_FILE=$root/$here/envs.yaml \
  PLATFORM_GATEWAY_URL=http://127.0.0.1:$GW_ADMIN_PORT GATEWAY_TOKEN=$gw_token \
  PLATFORM_DSN_HOST_SUFFIXES=127.0.0.1 \
  PLATFORM_PLAN_BUCKETS=src:moimroom-photos,dst:moimroom-photos \
  PLATFORM_ADDR=127.0.0.1:$API_PORT PLATFORM_STATE_DIR=$work/runs \
  SRC_PGPASSWORD=$pgpass DST_PGPASSWORD=$pgpass \
  serve api "http://127.0.0.1:$API_PORT/healthz" "$bin/api$exe"
curl -sf -o /dev/null "http://127.0.0.1:$GW_PORT/healthz" || { echo "gateway public port not answering" >&2; exit 1; }

echo "== migrate under load (database and photos)"
E2E_AGENT_TOKEN=$agent_token E2E_OPERATOR_TOKEN=$op_token E2E_TARGET_DSN=$(dsn "$DST_DB_PORT") E2E_HOST_CODE=$host_code \
  "$bin/driver$exe" -api "http://127.0.0.1:$API_PORT" -gateway "http://127.0.0.1:$GW_PORT" \
  -plan "$here/plan.json" -manifest "$here/target.yaml" -run-id-file "$work/run-id"

echo "== decommission the source (engine)"
SRC_PGPASSWORD=$pgpass DST_PGPASSWORD=$pgpass GATEWAY_TOKEN=$gw_token \
  "$bin/hopmigrate$exe" decommission -plan "$here/decommission.json" -run "$(cat "$work/run-id")" -state-dir "$work/runs"
echo "== E2E passed"
