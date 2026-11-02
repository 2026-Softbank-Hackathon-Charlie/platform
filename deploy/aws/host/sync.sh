#!/bin/bash
# Brings the management host to the settings in SSM Parameter Store and
# (re)starts the stack. Runs as root on the host, at first boot (user_data)
# and on every ./redeploy.sh. Safe to run again: volumes (PostgreSQL data,
# certificates) are kept.
#   bash sync.sh <region> <parameter prefix>     e.g. ap-northeast-2 /hopcharlie
# Log of the last run: /var/log/hopcharlie-sync.log
set -euo pipefail
region="$1"
prefix="$2"
exec > >(tee /var/log/hopcharlie-sync.log) 2>&1

install -d -m 0755 /opt/hopcharlie
cd /opt/hopcharlie

get() {
  aws ssm get-parameter --region "$region" --with-decryption \
    --name "$prefix/$1" --query Parameter.Value --output text
}

# Write to a temporary file first so a failed fetch never leaves a half file.
put() { # put <file> <mode> <parameter>
  local tmp
  tmp="$(mktemp "$1.XXXX")"
  chmod "$2" "$tmp"
  get "$3" > "$tmp"
  mv -f "$tmp" "$1"
}

# --- files managed by Terraform (deploy/aws/host, examples/) ---
for f in compose.yaml compose.tailscale.yaml Caddyfile envs.yaml apps.yaml; do
  put "$f" 0644 "host/$f"
done

# --- secrets: only in these 0600 files and in SSM ---
umask 077
env_tmp="$(mktemp .env.XXXX)"
{
  get host/settings.env
  echo "DB_PASSWORD=$(get db-password)"
  echo "GATEWAY_TOKEN=$(get gateway-admin-token)"
  echo "PLATFORM_TOKENS=$(get platform-tokens)"
} > "$env_tmp"
# Team-managed: database passwords for plans and rclone remotes (KEY=value lines).
put db-passwords.env 0600 db-passwords
put rclone.env 0600 rclone-env

# --- kubeconfigs: one SecureString per cluster under <prefix>/kube/ ---
# The API container runs as uid 10001 and reads them through KUBECONFIG.
install -d -m 0750 -o 10001 -g 10001 kube
files=""
names="$(aws ssm get-parameters-by-path --region "$region" --path "$prefix/kube" \
  --query 'Parameters[].Name' --output text)"
for name in $names; do
  [ "$name" = "None" ] && continue
  base="${name##*/}"
  if [[ ! "$base" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
    echo "skipping kubeconfig parameter $name: the last part must be lowercase letters, digits and '-'"
    continue
  fi
  put "kube/$base" 0600 "kube/$base"
  chown 10001:10001 "kube/$base"
  files="$files:/home/app/.kube/$base"
done
echo "KUBECONFIG_FILES=${files#:}" >> "$env_tmp"

# --- Tailscale: joins the tailnet when <prefix>/tailscale-authkey exists ---
if key="$(get tailscale-authkey 2>/dev/null)" && [ -n "$key" ] && [ "$key" != "None" ]; then
  if ! command -v tailscale >/dev/null; then
    dnf config-manager --add-repo https://pkgs.tailscale.com/stable/amazon-linux/2023/tailscale.repo
    dnf install -y tailscale
    systemctl enable --now tailscaled
  fi
  if ! tailscale status >/dev/null 2>&1; then
    keyfile="$(mktemp)"
    printf '%s' "$key" > "$keyfile"
    tailscale up --auth-key="file:$keyfile" --hostname=hopcharlie-mgmt --accept-routes
    rm -f "$keyfile"
  fi
  echo "COMPOSE_FILE=compose.yaml:compose.tailscale.yaml" >> "$env_tmp"
  tailscale status | head -5 || true
fi
unset key
mv -f "$env_tmp" .env
umask 022

# --- start ---
registry="$(sed -n 's/^REGISTRY=//p' .env)"
aws ecr get-login-password --region "$region" | docker login --username AWS --password-stdin "$registry"
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
echo "sync done"
