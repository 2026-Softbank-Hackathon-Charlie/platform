#!/usr/bin/env bash
# E2E: deploy apps from a gitops repository with the app chart and the
# ApplicationSet (deploy/charts/hopcharlie-app, deploy/argocd) in Argo CD, on
# kind, without AWS or GitHub.
#
# kind runs Argo CD, a git server and a proxy (kit.yaml), CNPG and MinIO; a
# local registry container holds the images. run.sh writes the gitops files
# apps/<app>/kind.yaml by hand, the way the platform will, and checks what
# Argo CD makes of them: a stateless app, an update that never gets ready
# while the old version keeps serving through the gateway, a good update, a
# stateful app (moimroom with a database and a bucket) and the deletions.
# Then the platform does the same from a fake GitHub, and deploys a branch
# of moimroom as its own instance (ADR 0015). See README.md.
#
# Needs: docker, kubectl, helm, go, git, curl, and the moimroom checkout
# next to this repository (MOIMROOM_DIR).
#   test/e2e/deploy/run.sh                  # run, then remove everything
#   KEEP=1 test/e2e/deploy/run.sh           # leave everything running
#   ONLY=instances test/e2e/deploy/run.sh   # only the platform's moimroom and its branch instance
#   INSTANCES=0 test/e2e/deploy/run.sh      # everything but the branch instance
set -euo pipefail
cd "$(dirname "$0")/../../.."
here=test/e2e/deploy
. "$here/lib.sh"
export KUBECONFIG="$work/kubeconfig" # never touches ~/.kube/config
export GIT_TERMINAL_PROMPT=0
began=$(date +%s)
only=${ONLY:-}
instances=${INSTANCES:-1}
case "$only/$instances" in
/0 | /1 | instances/1) ;;
*) echo "ONLY must be empty or instances, INSTANCES 0 or 1, and ONLY=instances needs INSTANCES=1" >&2; exit 1 ;;
esac
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
# (WSAEFAULT) on a free port; a fresh process then binds fine (ADR 0010).
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
# wait_for seconds what command...: run command every second until it succeeds.
wait_for() {
  local n=$1 what=$2 i
  shift 2
  for i in $(seq 1 "$n"); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "FAIL: $what within ${n}s" >&2
  return 1
}
fail() { echo "FAIL: $*" >&2; exit 1; }
since() { echo $(($(date +%s) - $1)); }
# into the cluster through the proxy, by the names the cluster's programs use
incluster() { curl -sS --max-time 10 -x "http://127.0.0.1:$PROXY_PORT" "$@"; }
incluster_says() { [ "$(curl -s --max-time 3 -x "http://127.0.0.1:$PROXY_PORT" "$1")" = "$2" ]; }
incluster_status() { [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 -x "http://127.0.0.1:$PROXY_PORT" "$1")" = "$2" ]; }

argo() { kubectl -n argocd "$@"; }
# app_status instance: "<sync> <health>" of the Application <instance>-kind,
# "-" if there is none. The default instance is named after the app, a
# branch's <app>--<slug> (ADR 0015).
app_status() { argo get application "$1-$ENV_NAME" -o jsonpath='{.status.sync.status} {.status.health.status}' 2>/dev/null || echo -; }
app_is() { [ "$(app_status "$1")" = "$2" ]; }
no_namespace() { ! kubectl get namespace "$1" >/dev/null 2>&1; }
# rolled_out instance service image: the Deployment <app>-<service> in the
# instance's namespace runs only image, all pods ready
rolled_out() {
  [ "$(kubectl -n "$1" get deploy "${1%%--*}-$2" -o jsonpath='{.spec.template.spec.containers[0].image} {.status.replicas} {.status.updatedReplicas} {.status.readyReplicas}')" = "$3 1 1 1" ]
}
# at_head app: Argo CD compared the Application at the gitops repository's
# head, the check the platform makes (status.sync.revisions holds the chart's
# and the gitops revision).
at_head() {
  case " $(argo get application "$1-$ENV_NAME" -o jsonpath='{.status.sync.revisions[*]}') " in
  *" $(g "$gitops" rev-parse HEAD) "*) return 0 ;;
  esac
  return 1
}
healthy_with() { at_head "$1" && app_is "$1" "Synced Healthy" && rolled_out "$1" web "$2"; }
no_apps() { [ -z "$(argo get applications -o name)" ]; }
# app_gone instance...: no Application is left, nor the instances' namespaces
app_gone() {
  local i
  no_apps || return 1
  for i in "$@"; do no_namespace "$i" || return 1; done
}
# instance_gone instance: its Application and namespace are gone
instance_gone() { ! argo get application "$1-$ENV_NAME" >/dev/null 2>&1 && no_namespace "$1"; }
# argo_refresh app...: after a gitops commit, what the platform does: refresh
# the ApplicationSet (new and removed files) and the apps' Applications.
argo_refresh() {
  local a
  argo annotate applicationset hopcharlie-apps argocd.argoproj.io/application-set-refresh=true --overwrite >/dev/null
  for a in "$@"; do
    argo annotate application "$a-$ENV_NAME" argocd.argoproj.io/refresh=normal --overwrite >/dev/null 2>&1 || true
  done
}
# gateway_says instance body [path]: Host <instance>.e2e.test answers body
gateway_says() { [ "$(curl -s -H "Host: $1.$APP_DOMAIN" "http://127.0.0.1:$GW_PORT${3:-/}")" = "$2" ]; }
# gateway_status instance path code: Host <instance>.e2e.test answers with code
gateway_status() { [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $1.$APP_DOMAIN" "http://127.0.0.1:$GW_PORT$2")" = "$3" ]; }
show_app() { # app: why it is not there yet
  echo "== Application $1-$ENV_NAME: $(app_status "$1")" >&2
  argo get application "$1-$ENV_NAME" -o jsonpath='{.status.conditions}{"\n"}{.status.operationState.message}{"\n"}' >&2 2>/dev/null || true
  kubectl -n "$1" get deploy,rs,pods,cluster 2>/dev/null >&2 || true
}

# git in a test repository, as a committer of its own and without helpers
g() {
  local dir=$1
  shift
  git -C "$dir" -c user.name="HopCharlie E2E" -c user.email=e2e@hopcharlie.invalid \
    -c commit.gpgsign=false -c credential.helper= -c core.autocrlf=false "$@"
}
# new_repo dir name: an empty checkout whose origin is the git server's <name>.git
new_repo() {
  rm -rf "$1"
  mkdir -p "$1"
  g "$1" init -q -b main
  g "$1" remote add origin "http://127.0.0.1:$GIT_PORT/$2.git"
}
# publish dir message: commit everything and replace the git server's main with it
publish() {
  g "$1" add -A
  g "$1" commit -q -m "$2"
  retry 5 g "$1" push -q --force origin main
}
# commit_gitops message app...: publish the gitops checkout, then refresh as
# the platform does
commit_gitops() {
  publish "$gitops" "$1"
  shift
  argo_refresh "$@"
}
degraded() { at_head "$1" && app_is "$1" "Synced Degraded"; }

gitops=$work/repos/gitops
# demo_file image [healthy]: apps/demo/kind.yaml as the platform writes it;
# healthy "false" makes the version's /healthz answer 500
demo_file() {
  local extra=""
  [ -z "${2:-}" ] || extra=$'\n'"        DEMO_HEALTHY: \"$2\""
  mkdir -p "$gitops/apps/demo"
  cat >"$gitops/apps/demo/$ENV_NAME.yaml" <<EOF
# Generated by the HopCharlie platform. Do not edit.
app: demo
env: $ENV_NAME
namespace: demo
commit: $(rand 20)
deployment: e2e-$(rand 4)
values:
  app: demo
  services:
    - name: web
      image: "$1"
      port: 8080
      healthCheck: /healthz
      public: true
      replicas: 1
      env:
        DEMO_MESSAGE: "hello"$extra
  resources: []
EOF
}

# probe_start name: send requests to demo through the gateway until probe_stop
probe_start() {
  rm -f "$work/$1.stop" "$work/$1.json"
  start "$1" "$bin/probe$exe" -url "http://127.0.0.1:$GW_PORT/" -host "demo.$APP_DOMAIN" \
    -stop "$work/$1.stop" -out "$work/$1.json" "${@:2}"
}
# probe_stop name: stop it; fails when it saw a failed request or the wrong version
probe_stop() {
  local pid code=0
  pid=$(cat "$work/$1.pid")
  touch "$work/$1.stop"
  wait "$pid" || code=$?
  rm -f "$work/$1.pid"
  echo "$1: $(cat "$work/$1.log")"
  [ "$code" = 0 ] || fail "$1 (see $work/$1.json)"
}

finish() {
  local code=$?
  if [ "$code" != 0 ]; then
    for f in gateway api fakegithub; do
      [ -s "$work/$f.log" ] && { echo "== last lines of $f.log"; tail -n 15 "$work/$f.log"; }
    done
    for a in demo moimroom moimroom--dev; do argo get application "$a-$ENV_NAME" >/dev/null 2>&1 && show_app "$a"; done
    echo "== Argo CD"; argo logs deploy/argocd-applicationset-controller --tail=10 2>/dev/null || true
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

for t in docker kubectl helm go git curl; do
  command -v "$t" >/dev/null || { echo "$t is not on PATH" >&2; exit 1; }
done
[ -d "$MOIMROOM_DIR" ] || { echo "$MOIMROOM_DIR not found (set MOIMROOM_DIR)" >&2; exit 1; }

echo "== build (work dir $work)"
stop_processes
rm -rf "$work"/*.log "$work"/*.json "$work"/*.stop "$work/gateway-state.json" "$work/repos" "$work/kit" "$work/github" "$work/runs"
mkdir -p "$bin" "$work/kit"
GOBIN="$bin" go install "sigs.k8s.io/kind@$KIND_VERSION"
for c in api gateway; do go build -o "$bin/$c$exe" "./cmd/$c"; done
for c in probe fakegithub driver; do go build -o "$bin/$c$exe" "./$here/$c"; done
arch=$(docker version -f '{{.Server.Arch}}')
for c in gitserver proxy; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$work/kit/$c" "./$here/$c"
done
cp "$here/kit.Dockerfile" "$work/kit/Dockerfile"
for p in $GW_PORT $GW_ADMIN_PORT $API_PORT $GITHUB_PORT $STORE_PORT; do
  if busy "$p"; then echo "port $p is in use (see $here/lib.sh)" >&2; exit 1; fi
done
[ -f "$MOIMROOM_DIR/platform.yaml" ] || { echo "$MOIMROOM_DIR has no platform.yaml (check out moimroom's main)" >&2; exit 1; }
if [ -n "$(docker ps -aq --filter "name=^$STORE\$")" ]; then
  [ -n "$(docker ps -aq --filter "name=^$STORE\$" --filter "label=$LABEL")" ] ||
    { echo "container $STORE exists but this test did not create it" >&2; exit 1; }
  docker rm -f -v "$STORE" >/dev/null
fi

echo "== registry $REGISTRY on 127.0.0.1:$REGISTRY_PORT, images"
if [ -n "$(docker ps -aq --filter "name=^$REGISTRY\$")" ]; then
  [ -n "$(docker ps -aq --filter "name=^$REGISTRY\$" --filter "label=$LABEL")" ] ||
    { echo "container $REGISTRY exists but this test did not create it" >&2; exit 1; }
  docker start "$REGISTRY" >/dev/null
else
  busy "$REGISTRY_PORT" && { echo "port $REGISTRY_PORT is in use (see $here/lib.sh)" >&2; exit 1; }
  docker run -d --name "$REGISTRY" --label "$LABEL=1" -p "127.0.0.1:$REGISTRY_PORT:5000" "$REGISTRY_IMAGE" >/dev/null
fi
wait_http "http://127.0.0.1:$REGISTRY_PORT/v2/"
reg=localhost:$REGISTRY_PORT
# push name context: build, push and print the image pinned by digest
push() {
  docker build -q -t "$reg/$1" "$2" >/dev/null
  docker push -q "$reg/$1" >/dev/null
  docker inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' "$reg/$1" | grep -m1 "^$reg/${1%%:*}@"
}
kit_image=$(push hopcharlie-e2e/kit:dev "$work/kit")
if [ -z "$only" ]; then
  demo_v1=$(push hopcharlie/apps/demo/web:v1 "$here/demo")
  cp -r "$here/demo" "$work/demo-v2"
  echo v2 >"$work/demo-v2/version.txt"
  demo_v2=$(push hopcharlie/apps/demo/web:v2 "$work/demo-v2")
  moimroom_image=$(push hopcharlie/apps/moimroom/web:e2e "$MOIMROOM_DIR")
  echo "demo v1 $demo_v1"
  echo "demo v2 $demo_v2"
fi

echo "== kind cluster $CLUSTER"
if ! "$kind" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  for p in $GIT_PORT $PROXY_PORT; do
    if busy "$p"; then echo "port $p is in use (see $here/lib.sh)" >&2; exit 1; fi
  done
  docker network inspect kind >/dev/null 2>&1 || touch "$work/created-kind-network"
  "$kind" create cluster --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG"
fi
"$kind" export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
# kind's local-registry pattern: localhost:$REGISTRY_PORT in a pod is the
# registry container on the kind network.
for node in $("$kind" get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/$reg"
  printf '[host."http://%s:5000"]\n' "$REGISTRY" | docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/$reg/hosts.toml"
done
if [ "$(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "$REGISTRY")" = null ]; then
  docker network connect kind "$REGISTRY"
fi
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ConfigMap
metadata: { name: local-registry-hosting, namespace: kube-public }
data:
  localRegistryHosting.v1: |
    host: "$reg"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

echo "== CNPG $CNPG_VERSION, git server and proxy, Argo CD $ARGOCD_VERSION"
kubectl apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml" >/dev/null
sed "s|KIT_IMAGE|$kit_image|" "$here/kit.yaml" | kubectl apply -f - >/dev/null
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# core-install: the controllers and the repo server, no API server or UI.
argo apply --server-side --force-conflicts \
  -f "https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD_VERSION/manifests/core-install.yaml" >/dev/null
kubectl -n e2e-kit rollout status deploy/gitserver deploy/proxy --timeout=300s
wait_http "http://127.0.0.1:$GIT_PORT/healthz"
wait_http "http://127.0.0.1:$PROXY_PORT/healthz"
for d in deploy/argocd-repo-server deploy/argocd-applicationset-controller deploy/argocd-redis statefulset/argocd-application-controller; do
  argo rollout status "$d" --timeout=300s
done

echo "== repositories infra (the chart) and gitops; AppProject and ApplicationSet (deploy/argocd)"
new_repo "$work/repos/infra" infra
mkdir -p "$work/repos/infra/helm/charts"
cp -r deploy/charts/hopcharlie-app "$work/repos/infra/helm/charts/"
publish "$work/repos/infra" "hopcharlie-app chart"
new_repo "$gitops" gitops
echo "# Generated by the HopCharlie platform." >"$gitops/README.md"
publish "$gitops" "empty"
helm template hopcharlie-apps deploy/argocd -f "$here/argocd-values.yaml" | argo apply -f - >/dev/null
# Without a refresh request the repo server may answer from its revision
# cache (3 minutes) and miss the new commit.
argo_refresh
# A kept cluster may still have the apps of the last run: the empty gitops
# repository makes the ApplicationSet delete them.
wait_for 300 "Applications of an earlier run are deleted" no_apps
kubectl delete namespace demo moimroom moimroom--dev --ignore-not-found --wait=true >/dev/null

echo "== gateway (app domain $APP_DOMAIN, backends through the proxy)"
gw_token=$(rand 24)
# The default app is unused: every request here names demo.
HTTP_PROXY=http://127.0.0.1:$PROXY_PORT GATEWAY_ADMIN_TOKEN=$gw_token \
  GATEWAY_BACKENDS=unused=http://127.0.0.1:9 \
  GATEWAY_LISTEN=127.0.0.1:$GW_PORT GATEWAY_ADMIN_ADDR=127.0.0.1:$GW_ADMIN_PORT \
  GATEWAY_STATE_FILE=$work/gateway-state.json GATEWAY_APP_DOMAIN=$APP_DOMAIN \
  serve gateway "http://127.0.0.1:$GW_ADMIN_PORT/healthz" "$bin/gateway$exe"
gw_admin() { # method path [json]
  curl -sSf -o /dev/null -X "$1" -H "Authorization: Bearer $gw_token" -H "Content-Type: application/json" \
    ${3:+-d "$3"} "http://127.0.0.1:$GW_ADMIN_PORT$2"
}

echo "== CNPG, MinIO and the buckets"
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=300s
s3_user=moimroom
s3_key=$(rand 16)
# A fresh MinIO with new credentials and no buckets. A MinIO of a kept
# cluster goes first, so no request lands on a pod that is stopping.
kubectl -n minio delete deploy minio --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl -n minio wait --for=delete pod -l app=minio --timeout=120s >/dev/null 2>&1 || true
kubectl create namespace minio --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n minio create secret generic minio-root --from-literal=MINIO_ROOT_USER=$s3_user \
  --from-literal=MINIO_ROOT_PASSWORD="$s3_key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed "s|MINIO_IMAGE|$MINIO_IMAGE|" "$here/minio.yaml" | kubectl apply -f - >/dev/null
kubectl -n minio rollout status deploy/minio --timeout=300s
# infra's part: the bucket <prefix><namespace>-<resource> (no prefix here),
# for moimroom and for its branch instance moimroom--dev
s3() { incluster -f -o /dev/null --aws-sigv4 "aws:amz:us-east-1:s3" --user "$s3_user:$s3_key" "$@"; }
for b in moimroom-photos moimroom--dev-photos; do
  retry 30 s3 -X PUT "http://minio.minio.svc.cluster.local:9000/$b"
  s3 -I "http://minio.minio.svc.cluster.local:9000/$b" || fail "bucket $b is not there"
done
# s3_secret namespace: infra's other part, the bucket's keys in the namespace
s3_secret() {
  kubectl -n "$1" create secret generic s3-credentials --from-literal=accessKey=$s3_user \
    --from-literal=secretKey="$s3_key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

# ---------------------------------------------------------------------------
# By hand: the gitops files as run.sh writes them (skipped with ONLY=instances).
if [ -z "$only" ]; then
  echo "== demo v1: commit apps/demo/$ENV_NAME.yaml"
  demo_file "$demo_v1"
  commit_gitops "deploy demo v1" demo
  t0=$(date +%s)
  wait_for 300 "demo-$ENV_NAME is Synced Healthy with v1" healthy_with demo "$demo_v1" || { show_app demo; exit 1; }
  v1_healthy=$(since "$t0")
  # A connection opened before kube-proxy knew the new Service stays
  # unanswered, so the first answer may take a few tries.
  wait_for 60 "demo-web answers 'v1 hello' in the cluster" incluster_says "http://demo-web.demo.svc.cluster.local:8080/" "v1 hello"
  body="v1 hello"
  psa=$(kubectl get namespace demo -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')
  [ "$psa" = restricted ] || fail "namespace demo enforces '$psa', want restricted"
  guards=$(kubectl -n demo get resourcequota,limitrange,networkpolicy -o name | tr '\n' ' ')
  [ "$(echo "$guards" | wc -w | tr -d ' ')" = 3 ] || fail "namespace demo has guardrails '$guards', want a ResourceQuota, a LimitRange and a NetworkPolicy"
  echo "demo v1 Synced Healthy ${v1_healthy}s after the commit; demo-web answers '$body'; namespace demo enforces $psa; $guards"
  # The NetworkPolicy lets in the proxy's namespace (the gateway's path) and
  # keeps out the rest of the cluster.
  reach() { # namespace: a pod there gets an answer from demo-web
    kubectl -n "$1" run "reach-$(rand 3)" --image="$kit_image" --restart=Never --rm -i --quiet --command -- \
      wget -q -T 5 -O - http://demo-web.demo.svc.cluster.local:8080/ >/dev/null 2>&1
  }
  kubectl create namespace e2e-outside --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  reach e2e-kit || fail "a pod in namespace e2e-kit (gatewayFrom) did not reach demo-web"
  ! reach e2e-outside || fail "a pod in namespace e2e-outside reached demo-web through the NetworkPolicy"
  echo "NetworkPolicy: a pod in e2e-kit reaches demo-web, one in e2e-outside does not"

  gw_admin PUT /apps/demo '{"backends":{"kind":"http://demo-web.demo.svc.cluster.local:8080"},"writeBackend":"kind"}'
  wait_for 30 "the gateway serves demo" gateway_says demo "v1 hello"
  echo "Host demo.$APP_DOMAIN answers 'v1 hello'"

  echo "== a version that never gets ready (DEMO_HEALTHY=false): v1 keeps serving"
  probe_start probe-bad -only "v1 hello"
  sleep 2
  demo_file "$demo_v1" false
  commit_gitops "deploy demo with a failing health check" demo
  t0=$(date +%s)
  wait_for 300 "demo-$ENV_NAME is Degraded" degraded demo || { show_app demo; exit 1; }
  bad_degraded=$(since "$t0")
  reason=$(kubectl -n demo get deploy demo-web -o jsonpath='{.status.conditions[?(@.type=="Progressing")].reason}')
  [ "$reason" = ProgressDeadlineExceeded ] || fail "demo-web Progressing reason $reason, want ProgressDeadlineExceeded"
  sleep 5 # and after
  probe_stop probe-bad
  echo "Degraded ${bad_degraded}s after the commit ($reason); the old pod served every request"

  echo "== demo v2: becomes healthy, still without a failed request"
  probe_start probe-v2 -last "v2 hello"
  sleep 2
  demo_file "$demo_v2"
  commit_gitops "deploy demo v2" demo
  t0=$(date +%s)
  wait_for 300 "demo-$ENV_NAME is Synced Healthy with v2" healthy_with demo "$demo_v2" || { show_app demo; exit 1; }
  v2_healthy=$(since "$t0")
  wait_for 30 "the gateway serves v2" gateway_says demo "v2 hello"
  sleep 8 # the old pod's preStop sleep and shutdown
  probe_stop probe-v2
  echo "demo v2 Synced Healthy ${v2_healthy}s after the commit"

  echo "== moimroom with a postgres and an s3 resource (MinIO)"
  mkdir -p "$gitops/apps/moimroom"
  cat >"$gitops/apps/moimroom/$ENV_NAME.yaml" <<EOF
# Generated by the HopCharlie platform. Do not edit.
app: moimroom
env: $ENV_NAME
namespace: moimroom
commit: $(rand 20)
deployment: e2e-$(rand 4)
values:
  app: moimroom
  services:
    - {name: web, image: "$moimroom_image", port: 8080, healthCheck: /healthz, public: true, replicas: 1, env: {}}
  resources:
    - {name: db, type: postgres}
    - {name: photos, type: s3}
EOF
  commit_gitops "deploy moimroom" moimroom
  t0=$(date +%s)
  # infra's other part: the credentials Secret in the app's namespace
  wait_for 120 "namespace moimroom exists" kubectl get namespace moimroom
  kubectl -n moimroom create secret generic s3-credentials --from-literal=accessKey=$s3_user \
    --from-literal=secretKey="$s3_key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  wait_for 600 "moimroom-$ENV_NAME is Synced Healthy" healthy_with moimroom "$moimroom_image" || { show_app moimroom; exit 1; }
  moimroom_healthy=$(since "$t0")
  # /readyz pings the database (DATABASE_URL) and checks the bucket with the keys (S3_*).
  wait_for 60 "moimroom /readyz answers 200" incluster_status "http://moimroom-web.moimroom.svc.cluster.local:8080/readyz" 200
  spaces=$(incluster "http://moimroom-web.moimroom.svc.cluster.local:8080/api/spaces?limit=50" | grep -o '"id":' | wc -l | tr -d ' ')
  [ "$spaces" -gt 0 ] || fail "moimroom lists no spaces (its seed did not reach the database)"
  echo "moimroom Synced Healthy ${moimroom_healthy}s after the commit; /readyz 200; $spaces spaces from the database"

  echo "== delete: remove apps/*/ from the gitops repository"
  g "$gitops" rm -rq apps
  commit_gitops "remove demo and moimroom" demo moimroom
  t0=$(date +%s)
  # The chart owns the namespace, so it goes with the Application, and with it
  # the database's volume and the S3 Secret the test put there.
  wait_for 300 "the Applications are deleted" no_apps
  apps_gone=$(since "$t0")
  wait_for 300 "namespace demo is deleted" no_namespace demo
  wait_for 300 "namespace moimroom is deleted" no_namespace moimroom
  ns_gone=$(since "$t0")
  no_volumes() { [ -z "$(kubectl get pv -o jsonpath="{range .items[?(@.spec.claimRef.namespace==\"$1\")]}{.metadata.name}{end}")" ]; }
  wait_for 120 "the volumes of moimroom are deleted" no_volumes moimroom
  ! kubectl -n moimroom get secret s3-credentials >/dev/null 2>&1 || fail "the S3 Secret outlived its namespace"
  echo "Applications gone ${apps_gone}s, namespaces (with the database volume and the S3 Secret) ${ns_gone}s after the commit"
fi

# ---------------------------------------------------------------------------
# The same through the platform: it reads platform.yaml from "GitHub",
# builds, commits the gitops file and registers the app at the gateway.

echo "== the platform: store, fake GitHub, API (docker builder, git gitops, Argo CD, template backends)"
[ -n "$only" ] || gw_admin DELETE /apps/demo # the platform registers it itself
store_pw=$(rand 16)
for i in 1 2 3; do # Docker's port proxy can hit the Windows bind error of serve()
  docker run -d --name "$STORE" --label "$LABEL=1" -p "127.0.0.1:$STORE_PORT:5432" \
    -e POSTGRES_USER=platform -e POSTGRES_PASSWORD="$store_pw" -e POSTGRES_DB=platform "$STORE_IMAGE" >/dev/null && break
  docker rm -f -v "$STORE" >/dev/null 2>&1 || true
  [ "$i" = 3 ] && exit 1
  sleep 2
done
retry 30 docker exec "$STORE" pg_isready -q -h 127.0.0.1 -U platform
# Repository owner/name is the checkout $work/github/<owner>/<name>: the fake
# GitHub API reads it, the docker builder clones it.
ghroot=$work/github
demo_repo=$ghroot/e2e/demo
mkdir -p "$demo_repo"
g "$demo_repo" init -q -b main
cp -r "$here/demo/." "$demo_repo/"
g "$demo_repo" add -A
g "$demo_repo" commit -q -m "demo v1"
serve fakegithub "http://127.0.0.1:$GITHUB_PORT/healthz" \
  "$bin/fakegithub$exe" -listen "127.0.0.1:$GITHUB_PORT" -root "$ghroot" -key "$work/github-app.pem"
op_token=$(rand 24)
hook_secret=$(rand 16)
PLATFORM_DATABASE_URL="postgres://platform:$store_pw@127.0.0.1:$STORE_PORT/platform?sslmode=disable" \
  PLATFORM_TOKENS="operator:operator:$op_token" \
  PLATFORM_ENVS_FILE=$root/$here/envs.yaml \
  PLATFORM_GATEWAY_URL=http://127.0.0.1:$GW_ADMIN_PORT GATEWAY_TOKEN=$gw_token \
  PLATFORM_ADDR=127.0.0.1:$API_PORT PLATFORM_STATE_DIR=$work/runs \
  PLATFORM_GITHUB_APP_ID=1 PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE=$work/github-app.pem \
  PLATFORM_GITHUB_INSTALLATION_ID=1 PLATFORM_GITHUB_ORG=e2e \
  PLATFORM_GITHUB_API_URL=http://127.0.0.1:$GITHUB_PORT PLATFORM_GITHUB_WEBHOOK_SECRET=$hook_secret \
  PLATFORM_APP_DOMAIN=$APP_DOMAIN \
  PLATFORM_DEPLOY_BUILDER=docker PLATFORM_IMAGE_REGISTRY=$reg PLATFORM_DOCKER_CLONE_URL="$ghroot/{repo}" \
  PLATFORM_GITOPS_MODE=git PLATFORM_GITOPS_URL=http://127.0.0.1:$GIT_PORT/gitops.git \
  PLATFORM_ARGOCD_KUBECONTEXT=kind-$CLUSTER PLATFORM_ARGOCD_TIMEOUT=100s \
  PLATFORM_APP_BACKEND_TEMPLATE='http://{app}-{service}.{instance}.svc.cluster.local:{port}' \
  serve api "http://127.0.0.1:$API_PORT/healthz" "$bin/api$exe"
export E2E_OPERATOR_TOKEN=$op_token E2E_WEBHOOK_SECRET=$hook_secret
drv() { "$bin/driver$exe" "$1" -api "http://127.0.0.1:$API_PORT" "${@:2}"; }
# follow id want: watch a deployment until it ends; fails unless it ends as want
follow() { drv watch -id "$1" -want "$2" | tee "$work/watch-$1.log"; }
# demo_commit version [healthy]: commit the demo repository; prints the commit
demo_commit() {
  local extra=""
  [ -z "${2:-}" ] || extra=$'\n'"      DEMO_HEALTHY: \"$2\""
  echo "$1" >"$demo_repo/version.txt"
  cat >"$demo_repo/platform.yaml" <<EOF
name: demo
services:
  - name: web
    port: 8080
    healthCheck: /healthz
    public: true
    env:
      DEMO_MESSAGE: hello$extra
EOF
  g "$demo_repo" add -A
  g "$demo_repo" commit -q -m "demo $1${2:+, healthy $2}"
  g "$demo_repo" rev-parse HEAD
}
gitops_file() { # instance: apps/<instance>/kind.yaml at the gitops repository's main
  g "$gitops" fetch -q origin
  g "$gitops" show "origin/main:apps/$1/$ENV_NAME.yaml"
}

if [ -z "$only" ]; then
  echo "== register e2e/demo (POST /v1/apps) and follow its first deployment"
  t0=$(date +%s)
  dep=$(drv register -repo e2e/demo -env "$ENV_NAME")
  follow "$dep" healthy
  p_register=$(since "$t0")
  file=$(gitops_file demo)
  for want in "deployment: \"$dep\"" "commit: \"$(g "$demo_repo" rev-parse HEAD)\"" \
    "image: \"$reg/hopcharlie/apps/demo/web@sha256:" '"DEMO_MESSAGE": "hello"'; do
    case "$file" in *"$want"*) ;; *) fail "apps/demo/$ENV_NAME.yaml lacks $want; it is:"$'\n'"$file" ;; esac
  done
  wait_for 30 "the gateway serves demo" gateway_says demo "v1 hello"
  echo "registered to healthy in ${p_register}s; the gitops file names the deployment, the commit and the image by digest; Host demo.$APP_DOMAIN answers 'v1 hello'"

  echo "== push a version whose health check fails (webhook): the deployment fails, v1 keeps serving"
  probe_start probe-platform-bad -only "v1 hello"
  sleep 2
  t0=$(date +%s)
  dep=$(drv push -repo e2e/demo -after "$(demo_commit v1 false)")
  follow "$dep" failed
  p_bad=$(since "$t0")
  probe_stop probe-platform-bad

  echo "== push twice in a row: the first deployment is superseded, the second becomes healthy"
  probe_start probe-platform-v3 -last "v3 hello"
  sleep 2
  t0=$(date +%s)
  dep_a=$(drv push -repo e2e/demo -after "$(demo_commit v2)")
  dep_b=$(drv push -repo e2e/demo -after "$(demo_commit v3)")
  follow "$dep_a" superseded
  follow "$dep_b" healthy
  p_v3=$(since "$t0")
  wait_for 30 "the gateway serves v3" gateway_says demo "v3 hello"
  sleep 8 # the old pod's preStop sleep and shutdown
  probe_stop probe-platform-v3

  echo "== DELETE /v1/apps/demo"
  t0=$(date +%s)
  drv delete -app demo
  wait_for 90 "demo-$ENV_NAME and namespace demo are deleted" app_gone demo
  p_delete=$(since "$t0")
  ! gitops_file demo >/dev/null 2>&1 || fail "apps/demo/$ENV_NAME.yaml is still in the gitops repository"
  echo "Application and namespace gone ${p_delete}s after the DELETE; apps/demo/ is gone from the gitops repository"
fi

echo "== register e2e/moimroom ($MOIMROOM_DIR at HEAD: postgres and s3)"
moim_repo=$ghroot/e2e/moimroom
mkdir -p "$moim_repo"
g "$MOIMROOM_DIR" archive HEAD | tar -x -C "$moim_repo"
g "$moim_repo" init -q -b main
g "$moim_repo" add -A
g "$moim_repo" commit -q -m "moimroom"
t0=$(date +%s)
dep=$(drv register -repo e2e/moimroom -env "$ENV_NAME")
follow "$dep" healthy >"$work/watch-moimroom.out" 2>&1 &
watcher=$!
# infra's part again: the S3 keys in the namespace once Argo CD made it (the
# bucket moimroom-photos is in MinIO)
wait_for 300 "namespace moimroom exists" kubectl get namespace moimroom
s3_secret moimroom
wait "$watcher" || { cat "$work/watch-moimroom.out"; fail "moimroom's first deployment did not become healthy"; }
cat "$work/watch-moimroom.out"
p_moimroom=$(since "$t0")
wait_for 60 "Host moimroom.$APP_DOMAIN /readyz answers 200" gateway_status moimroom /readyz 200
# spaces instance: how many spaces the instance lists through the gateway
spaces() { curl -s -H "Host: $1.$APP_DOMAIN" "http://127.0.0.1:$GW_PORT/api/spaces?limit=50" | grep -o '"id":' | wc -l | tr -d ' '; }
spaces=$(spaces moimroom)
[ "$spaces" -gt 0 ] || fail "moimroom through the gateway lists no spaces"
echo "moimroom registered to healthy in ${p_moimroom}s; through the gateway /readyz 200 and $spaces spaces"

if [ "$instances" = 1 ]; then
  # Branch dev differs from main by one file that the SPA build copies, so
  # /e2e-branch.txt answers on dev and is a 404 on main.
  # dev_commit text: commit text as that file on branch dev; prints the commit
  dev_commit() {
    g "$moim_repo" checkout -q dev 2>/dev/null || g "$moim_repo" checkout -q -b dev
    echo "$1" >"$moim_repo/web/public/e2e-branch.txt"
    g "$moim_repo" add -A
    g "$moim_repo" commit -q -m "$1"
    g "$moim_repo" rev-parse HEAD
    g "$moim_repo" checkout -q main
  }
  # add_dev: POST /v1/apps/moimroom/instances {"branch":"dev"}, then follow
  # the first deployment to healthy, putting the S3 keys in the new namespace
  add_dev() {
    local out dep watcher
    out=$(drv add-instance -app moimroom -branch dev)
    dep=${out% *}
    [ "${out#* }" = moimroom--dev ] || fail "branch dev became instance '${out#* }', want moimroom--dev"
    follow "$dep" healthy >"$work/watch-dev.out" 2>&1 &
    watcher=$!
    wait_for 300 "namespace moimroom--dev exists" kubectl get namespace moimroom--dev
    s3_secret moimroom--dev
    wait "$watcher" || { cat "$work/watch-dev.out"; fail "the first deployment of moimroom--dev did not become healthy"; }
    cat "$work/watch-dev.out"
  }

  echo "== branch instance: add moimroom's branch dev (POST /v1/apps/moimroom/instances)"
  main_last=$(drv last -app moimroom -branch main)
  dev_commit "dev 1" >/dev/null
  t0=$(date +%s)
  add_dev
  p_dev=$(since "$t0")
  app_is moimroom--dev "Synced Healthy" || fail "moimroom--dev-$ENV_NAME is $(app_status moimroom--dev)"
  [ -n "$(kubectl -n moimroom--dev get cluster moimroom-db -o name 2>/dev/null)" ] ||
    fail "namespace moimroom--dev has no CNPG Cluster moimroom-db of its own"
  wait_for 60 "Host moimroom--dev.$APP_DOMAIN serves dev's file" gateway_says moimroom--dev "dev 1" /e2e-branch.txt
  gateway_status moimroom /e2e-branch.txt 404 || fail "Host moimroom.$APP_DOMAIN serves dev's file"
  wait_for 60 "Host moimroom--dev.$APP_DOMAIN /readyz answers 200" gateway_status moimroom--dev /readyz 200
  dev_spaces=$(spaces moimroom--dev)
  [ "$dev_spaces" -gt 0 ] || fail "moimroom--dev through the gateway lists no spaces (its database is not seeded)"
  gateway_status moimroom /readyz 200 || fail "moimroom stopped answering /readyz"
  echo "moimroom--dev healthy ${p_dev}s after POST instances, with its own database ($dev_spaces spaces); Host moimroom--dev.$APP_DOMAIN serves dev, moimroom.$APP_DOMAIN still main"

  echo "== push to dev: only moimroom--dev deploys, main's last deployment stays"
  t0=$(date +%s)
  dep=$(drv push -repo e2e/moimroom -branch dev -after "$(dev_commit "dev 2")" -instance moimroom--dev)
  follow "$dep" healthy
  p_dev_push=$(since "$t0")
  wait_for 60 "Host moimroom--dev.$APP_DOMAIN serves dev 2" gateway_says moimroom--dev "dev 2" /e2e-branch.txt
  [ "$(drv last -app moimroom -branch main)" = "$main_last" ] ||
    fail "main's last deployment changed from '$main_last' to '$(drv last -app moimroom -branch main)'"
  case "$main_last" in *" healthy") ;; *) fail "main's last deployment is '$main_last', want healthy" ;; esac
  gateway_status moimroom /e2e-branch.txt 404 || fail "Host moimroom.$APP_DOMAIN serves dev's file"
  echo "push to dev healthy in ${p_dev_push}s; main's last deployment is still $main_last"

  echo "== DELETE /v1/apps/moimroom/instances/moimroom--dev: main keeps serving"
  t0=$(date +%s)
  drv delete-instance -app moimroom -instance moimroom--dev
  wait_for 180 "moimroom--dev-$ENV_NAME and namespace moimroom--dev are deleted" instance_gone moimroom--dev
  p_dev_delete=$(since "$t0")
  ! gitops_file moimroom--dev >/dev/null 2>&1 || fail "apps/moimroom--dev/$ENV_NAME.yaml is still in the gitops repository"
  gateway_status moimroom--dev / 404 || fail "the gateway still serves moimroom--dev"
  app_is moimroom "Synced Healthy" || fail "moimroom-$ENV_NAME is $(app_status moimroom) after deleting moimroom--dev"
  gateway_status moimroom /readyz 200 || fail "moimroom stopped answering /readyz after deleting moimroom--dev"
  echo "moimroom--dev gone ${p_dev_delete}s after the DELETE (Application, namespace, gitops file, gateway); moimroom still serves"

  echo "== add dev again, so deleting the app removes both instances"
  t0=$(date +%s)
  add_dev
  p_dev_again=$(since "$t0")
  wait_for 60 "Host moimroom--dev.$APP_DOMAIN serves dev 2" gateway_says moimroom--dev "dev 2" /e2e-branch.txt
fi

echo "== DELETE /v1/apps/moimroom"
t0=$(date +%s)
drv delete -app moimroom
wait_for 180 "the Applications and the namespaces of moimroom are deleted" app_gone moimroom moimroom--dev
p_moimroom_delete=$(since "$t0")
for i in moimroom moimroom--dev; do
  ! gitops_file "$i" >/dev/null 2>&1 || fail "apps/$i/$ENV_NAME.yaml is still in the gitops repository"
  gateway_status "$i" / 404 || fail "the gateway still serves $i"
done
echo "moimroom deleted with its namespaces in ${p_moimroom_delete}s"

echo "== results"
if [ -z "$only" ]; then
  echo "by hand (gitops files committed by run.sh):"
  echo "  demo v1 healthy     ${v1_healthy}s after the commit"
  echo "  failing version     Degraded after ${bad_degraded}s; $(cat "$work/probe-bad.log")"
  echo "  demo v2 healthy     ${v2_healthy}s after the commit; $(cat "$work/probe-v2.log")"
  echo "  moimroom healthy    ${moimroom_healthy}s after the commit (CNPG and MinIO)"
  echo "  deleted             Applications ${apps_gone}s, namespaces ${ns_gone}s"
fi
echo "through the platform:"
if [ -z "$only" ]; then
  echo "  demo registered     healthy ${p_register}s after POST /v1/apps"
  echo "  failing push        failed ${p_bad}s after the webhook; $(cat "$work/probe-platform-bad.log")"
  echo "  two pushes          first superseded, second healthy ${p_v3}s after the webhooks; $(cat "$work/probe-platform-v3.log")"
  echo "  DELETE              Application and namespace gone in ${p_delete}s"
fi
echo "  moimroom            healthy ${p_moimroom}s after POST /v1/apps"
if [ "$instances" = 1 ]; then
  echo "  instance dev        healthy ${p_dev}s after POST instances; a push to dev healthy in ${p_dev_push}s, main untouched"
  echo "  DELETE instance     Application and namespace gone in ${p_dev_delete}s, main kept serving; added again in ${p_dev_again}s"
fi
echo "  DELETE moimroom     every instance's Application and namespace gone in ${p_moimroom_delete}s"
echo "== E2E passed"
