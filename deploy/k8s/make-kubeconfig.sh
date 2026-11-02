#!/usr/bin/env bash
# Gives the HopCharlie platform access to the cluster of the current kubectl
# context and writes a kubeconfig for it (ServiceAccount token, no plugin).
#   ./make-kubeconfig.sh <context name> <out file> [app namespace ...]
#   ./make-kubeconfig.sh gke-hopcharlie gke.kubeconfig moimroom
# The context name must match examples/envs.yaml. App namespaces default to
# "moimroom". Set SERVER to the API address the management host reaches
# (for example a Tailscale address) when it differs from your own.
# Then: ../aws/put-kubeconfig.sh gke gke.kubeconfig && ../aws/redeploy.sh
set -euo pipefail
cd "$(dirname "$0")"

context="${1:?usage: make-kubeconfig.sh <context name> <out file> [namespace ...]}"
out="${2:?usage: make-kubeconfig.sh <context name> <out file> [namespace ...]}"
shift 2
namespaces=("${@:-moimroom}")

echo "cluster: $(kubectl config current-context)"
kubectl apply -f hopcharlie-platform.yaml
for ns in "${namespaces[@]}"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
  kubectl create rolebinding hopcharlie-migrator -n "$ns" --clusterrole=hopcharlie-migrator \
    --serviceaccount=hopcharlie-system:hopcharlie-platform --dry-run=client -o yaml | kubectl apply -f -
done

# The token controller fills the Secret asynchronously.
for _ in $(seq 1 30); do
  token="$(kubectl get secret hopcharlie-platform-token -n hopcharlie-system -o jsonpath='{.data.token}' 2>/dev/null || true)"
  [ -n "$token" ] && break
  sleep 1
done
[ -n "$token" ] || { echo "make-kubeconfig.sh: the token Secret was not filled" >&2; exit 1; }
ca="$(kubectl get secret hopcharlie-platform-token -n hopcharlie-system -o jsonpath='{.data.ca\.crt}')"
server="${SERVER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')}"

umask 077
cat > "$out" <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
  - name: $context
    cluster:
      server: $server
      certificate-authority-data: $ca
users:
  - name: hopcharlie-platform@$context
    user:
      token: $(printf '%s' "$token" | base64 -d)
contexts:
  - name: $context
    context:
      cluster: $context
      user: hopcharlie-platform@$context
current-context: $context
KUBECONFIG
echo "wrote $out (context $context, server $server)"
kubectl --kubeconfig "$out" auth can-i list nodes
