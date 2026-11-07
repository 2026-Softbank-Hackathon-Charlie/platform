#!/bin/sh
# Bootstrap authentication only. No kubeconfig or cluster credentials leave this host.
set -eu
umask 077
die() { printf '%s\n' "$1" >&2; exit 1; }
server= enrollment= environment= code= install_k3s=false
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
work=$(mktemp -d)
# Only the three exact files this process creates are removed; no recursive deletion.
trap 'rm -f "$work/request.json" "$work/response.json" "$work/k3s-install.sh"; rmdir "$work"' EXIT HUP INT TERM
if ! command -v k3s >/dev/null 2>&1; then
  [ "$install_k3s" = true ] || die 'k3s is absent. Install it yourself or explicitly select k3s installation.'
  [ "$(id -u)" -eq 0 ] || die 'Installing k3s requires an explicit rerun as root. This script does not run sudo.'
  [ ! -e /etc/rancher/k3s ] && [ ! -e /etc/systemd/system/k3s.service ] || die 'Existing k3s configuration found; refusing to replace it.'
  if [ ! -d /run/systemd/system ] && ! command -v rc-service >/dev/null 2>&1; then
    die 'Enable systemd in WSL2 or use a systemd/openrc Linux host before installation.'
  fi
  command -v sha256sum >/dev/null 2>&1 || die 'sha256sum is required.'
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 -fsS \
    'https://raw.githubusercontent.com/k3s-io/k3s/dca306c81c44fed1dd0621d5579785c980080ae8/install.sh' -o "$work/k3s-install.sh"
  printf '%s  %s\n' 'c2a2643dd7c259121a876d16538844c60928de3d38ae98f22f7b95a7ffcfab9d' "$work/k3s-install.sh" | sha256sum -c - >/dev/null
  INSTALL_K3S_VERSION='v1.36.5+k3s1' sh "$work/k3s-install.sh"
fi
k3s --version >/dev/null 2>&1 || die 'The existing k3s binary could not be checked.'
# Keep the one-use grant out of curl arguments and URLs. The temporary body is private.
printf '{"code":"%s","environment":"%s","installK3s":%s}' "$code" "$environment" "$install_k3s" > "$work/request.json"
status=$(curl --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 20 -sS \
  -H 'Content-Type: application/json' --data-binary "@$work/request.json" \
  -o "$work/response.json" -w '%{http_code}' "$server/v1/onprem/enrollments/$enrollment/claim")
code=
[ "$status" = 200 ] || die 'Bootstrap authentication failed, expired, or was already used. Revoke and issue a new grant in the console.'
grep -F "\"id\":\"$enrollment\"" "$work/response.json" >/dev/null || die 'The API did not acknowledge this enrollment.'
grep -F '"status":"claimed"' "$work/response.json" >/dev/null || die 'The API did not acknowledge bootstrap consumption.'
printf '%s\n' 'Bootstrap authentication accepted. Cluster registration and server-side deployment checks are still pending; this is not a ready environment.'
