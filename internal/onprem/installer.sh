#!/bin/sh
# Install a persistent, least-privilege connector. The administrator kubeconfig
# stays on this host; the connector renews its own delegated short-lived tokens.
set -eu
umask 077
die() { printf '%s\n' "$1" >&2; exit 1; }
server= enrollment= environment= code= install_k3s=false
needs_install=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --server|--enrollment|--environment|--code)
      [ "$#" -ge 2 ] || die 'Missing option value'
      case "$1" in
        --server) server=$2;; --enrollment) enrollment=$2;;
        --environment) environment=$2;; --code) code=$2;;
      esac
      shift 2;;
    --install-k3s) install_k3s=true; shift;;
    *) die 'Unknown installer option';;
  esac
done
[ "$(uname -s)" = Linux ] || die 'Run this script on Linux or inside WSL2.'
case "$server" in https://*) ;; *) die 'An HTTPS API origin is required';; esac
case "$server" in *\?*|*\#*|*@*|*[\ \'\"\\]*) die 'Invalid API origin';; esac
case "$enrollment" in ''|*[!a-zA-Z0-9_-]*) die 'Invalid enrollment ID';; esac
case "$environment" in ''|*[!a-z0-9-]*) die 'Invalid environment';; esac
[ "${#code}" -eq 47 ] || die 'Invalid bootstrap code'
case "$code" in onp_*) ;; *) die 'Invalid bootstrap code';; esac
case "$code" in *[!a-zA-Z0-9_-]*) die 'Invalid bootstrap code';; esac
command -v curl >/dev/null 2>&1 || die 'Install curl before continuing.'
command -v python3 >/dev/null 2>&1 || die 'Install python3 before continuing.'
command -v sha256sum >/dev/null 2>&1 || die 'sha256sum is required.'
[ "$(id -u)" -eq 0 ] || die 'Run the issued command explicitly as root to configure the local connector. This script does not run sudo.'
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) die 'Only Linux amd64 and arm64 are supported.';; esac
work=$(mktemp -d)
# Remove only this process's exact private files; never traverse directories.
cleanup() { rm -f "$work/request.json" "$work/response.json" "$work/k3s-install.sh" "$work/headers" "$work/bootstrap.json" "$work/status.json" "$work/access.json" "$work/connector.json"; rmdir "$work" 2>/dev/null || true; }
trap cleanup EXIT HUP INT TERM
if ! command -v k3s >/dev/null 2>&1; then
  [ "$install_k3s" = true ] || die 'k3s is absent. Install it yourself or explicitly select k3s installation.'
  [ "$(id -u)" -eq 0 ] || die 'Installing k3s requires an explicit rerun as root. This script does not run sudo.'
  [ ! -e /etc/rancher/k3s ] && [ ! -e /etc/systemd/system/k3s.service ] || die 'Existing k3s configuration found; refusing to replace it.'
  if [ ! -d /run/systemd/system ] && ! command -v rc-service >/dev/null 2>&1; then
    die 'Enable systemd in WSL2 or use a systemd/openrc Linux host before installation.'
  fi
  command -v sha256sum >/dev/null 2>&1 || die 'sha256sum is required.'
  needs_install=true
else
  k3s --version >/dev/null 2>&1 || die 'The existing k3s binary could not be checked.'
fi
# Keep the one-use grant out of curl arguments and URLs. The temporary body is private.
printf '{"code":"%s","environment":"%s","installK3s":%s}' "$code" "$environment" "$install_k3s" > "$work/request.json"
status=$(curl --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 20 -sS \
  -H 'Content-Type: application/json' --data-binary "@$work/request.json" \
  -o "$work/response.json" -w '%{http_code}' "$server/v1/onprem/enrollments/$enrollment/claim")
code=
[ "$status" = 200 ] || die 'Bootstrap authentication failed, expired, or was already used. Revoke and issue a new grant in the console.'
python3 - "$work/response.json" "$work/headers" "$enrollment" <<'PY' || die 'The API did not acknowledge this enrollment with a connection session. Revoke and issue a new grant.'
import json,re,sys
with open(sys.argv[1]) as f: value=json.load(f)
e=value.get('enrollment', {})
token=value.get('connectionToken', '')
if e.get('id') != sys.argv[3] or e.get('status') != 'claimed' or not isinstance(token,str) or not re.fullmatch(r'[A-Za-z0-9._:-]{32,512}', token):
    raise SystemExit(1)
with open(sys.argv[2], 'w') as f: f.write('Authorization: Bearer '+token+'\n')
PY
if [ "$needs_install" = true ]; then
  # No installation side effect happens until the live server consumes this grant.
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 -fsS \
    'https://raw.githubusercontent.com/k3s-io/k3s/dca306c81c44fed1dd0621d5579785c980080ae8/install.sh' -o "$work/k3s-install.sh"
  printf '%s  %s\n' 'c2a2643dd7c259121a876d16538844c60928de3d38ae98f22f7b95a7ffcfab9d' "$work/k3s-install.sh" | sha256sum -c - >/dev/null
  INSTALL_K3S_VERSION='v1.36.5+k3s1' sh "$work/k3s-install.sh"
  k3s --version >/dev/null 2>&1 || die 'k3s installation did not provide a usable binary. The grant is already consumed.'
fi
printf '%s\n' 'Waiting for the local Kubernetes API...'
attempt=0
until k3s kubectl --request-timeout=15s get --raw=/readyz >/dev/null 2>&1; do
  attempt=$((attempt + 1)); [ "$attempt" -lt 60 ] || die 'The local Kubernetes API did not become ready. The grant is already consumed.'
  sleep 2
done
curl --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 -fsS \
  -H "@$work/headers" "$server/v1/onprem/enrollments/$enrollment/bootstrap.yaml?arch=$arch" -o "$work/bootstrap.json" \
  || die 'Could not download this enrollment connector. Check its status or revoke and issue a new grant.'
# Apply RBAC and admission completely before any connector Pod may start.
# kubectl continues through a List even if one object fails, so one List apply
# could otherwise start an agent after its security policy failed installation.
python3 - "$work" "$enrollment" <<'PY' || die 'Invalid bootstrap manifest.'
import json,sys
directory,enrollment=sys.argv[1:]
with open(directory+'/bootstrap.json') as f: value=json.load(f)
if value.get('kind') != 'List' or not isinstance(value.get('items'),list): raise SystemExit(1)
connectors=[o for o in value['items'] if o.get('kind')=='Deployment']
if len(connectors)!=1 or connectors[0].get('metadata',{}).get('labels',{}).get('hopcharlie.dev/enrollment') != enrollment: raise SystemExit(1)
for name,items in [('access',[o for o in value['items'] if o.get('kind')!='Deployment']),('connector',connectors)]:
    with open(directory+'/'+name+'.json','w') as f: json.dump({'apiVersion':'v1','kind':'List','items':items},f)
PY
# No kubeconfig, administrator key, or shell command is uploaded.
k3s kubectl --request-timeout=30s apply --server-side --field-manager=hopcharlie-onprem -f "$work/access.json" \
  || die 'Connector access controls could not be applied; the connector was not started.'
k3s kubectl --request-timeout=30s apply --server-side --field-manager=hopcharlie-onprem -f "$work/connector.json" \
  || die 'Connector resources could not be applied. Existing resources were not forcibly replaced.'
printf '%s\n' 'Connector installed. Waiting for server-side network, permissions, image and Hello World verification...'
attempt=0
while [ "$attempt" -lt 120 ]; do
  status=$(curl --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 20 -sS \
    -H "@$work/headers" -o "$work/status.json" -w '%{http_code}' "$server/v1/onprem/enrollments/$enrollment/connection/status") || status=000
  case "$status" in
    401|403|409) die 'The connection session was rejected or revoked. Check this enrollment in the console.';;
    200)
      if python3 - "$work/status.json" "$enrollment" <<'PY'
import json,sys
with open(sys.argv[1]) as f: value=json.load(f)
e=value.get('enrollment', {})
if e.get('id') != sys.argv[2]: raise SystemExit(2)
if e.get('status') in ('failed','revoked','expired'): raise SystemExit(2)
checks={c.get('id'):c.get('status') for c in e.get('checks', [])}
if e.get('status') == 'ready' and all(checks.get(k)=='passed' for k in ('k3s','network','permissions','image_pull','hello_world')):
    raise SystemExit(0)
raise SystemExit(1)
PY
      then
        printf '%s\n' 'On-premises cluster connected. All server-side registration checks passed.'
        exit 0
      else
        result=$?
        [ "$result" -eq 1 ] || die 'Registration failed or returned an invalid identity. Check this enrollment in the console.'
      fi;;
  esac
  attempt=$((attempt + 1)); sleep 5
done
die 'The connector remains installed, but registration has not passed all checks. See the real check results in the console; do not treat this environment as ready.'
