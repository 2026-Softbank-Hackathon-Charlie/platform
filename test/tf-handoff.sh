#!/usr/bin/env bash
# Exercises the compatibility shim with fake tools; never contacts AWS or Docker.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
platform="$scratch/checkouts/renamed platform"
mkdir -p "$platform/deploy/aws" "$scratch/bin" "$scratch/caller"
cp "$root/deploy/aws/tf.sh" "$platform/deploy/aws/tf.sh"
export HANDOFF_CALLS="$scratch/calls"
export HANDOFF_SENTINEL='fake-secret-must-not-appear'
unset INFRA_DIR

for tool in aws docker terraform; do
  cat > "$scratch/bin/$tool" <<'FAKE'
#!/usr/bin/env bash
echo "unexpected external command" >> "$HANDOFF_CALLS"
exit 99
FAKE
  chmod +x "$scratch/bin/$tool"
done
export PATH="$scratch/bin:$PATH"

fail() { echo "tf-handoff: $*" >&2; exit 1; }
run_shim() {
  if (cd "$scratch/caller" && bash "$platform/deploy/aws/tf.sh" "$@") > "$scratch/out" 2> "$scratch/err"; then
    status=0
  else
    status=$?
  fi
}
assert_private() {
  if grep -q "$HANDOFF_SENTINEL" "$scratch/out" "$scratch/err"; then
    fail 'diagnostics leaked environment contents'
  fi
}

run_shim output -raw api_url
[ "$status" -ne 0 ] || fail 'missing infra checkout succeeded'
grep -q 'INFRA_DIR' "$scratch/err" || fail 'missing target lacks checkout guidance'
[ ! -s "$HANDOFF_CALLS" ] || fail 'missing target invoked an external command'
assert_private

make_target() {
  mkdir -p "$1/terraform/platform/aws"
  cat > "$1/terraform/platform/aws/tf.sh" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$HANDOFF_CALLS"
[ "$HANDOFF_SENTINEL" = 'fake-secret-must-not-appear' ] || exit 97
printf 'fake-output'
exit "${HANDOFF_EXIT:-0}"
FAKE
  chmod +x "$1/terraform/platform/aws/tf.sh"
}
make_target "$scratch/checkouts/infra"
run_shim output -raw api_url
[ "$status" -eq 0 ] || fail 'default sibling checkout did not resolve'
printf 'output\n-raw\napi_url\n' > "$scratch/expected"
cmp -s "$scratch/expected" "$HANDOFF_CALLS" || fail 'raw output arguments changed'
[ "$(cat "$scratch/out")" = fake-output ] || fail 'raw output was decorated'

export INFRA_DIR="$scratch/custom infra checkout"
make_target "$INFRA_DIR"
rm -rf "$scratch/checkouts/infra"
run_shim output -json 'name with spaces'
[ "$status" -eq 0 ] || fail 'explicit INFRA_DIR with whitespace failed'
printf 'output\n-json\nname with spaces\n' > "$scratch/expected"
cmp -s "$scratch/expected" "$HANDOFF_CALLS" || fail 'output arguments were split'
assert_private
export HANDOFF_EXIT=23
run_shim output -raw instance_id
[ "$status" -eq 23 ] || fail 'delegate exit status changed'
unset HANDOFF_EXIT

for command in '' init plan apply destroy state import refresh force-unlock -chdir=state -chdir=dns; do
  : > "$HANDOFF_CALLS"
  run_shim "$command"
  [ "$status" -ne 0 ] || fail "legacy command accepted: $command"
  [ ! -s "$HANDOFF_CALLS" ] || fail "legacy command reached delegate: $command"
  grep -q 'terraform/platform/aws' "$scratch/err" || fail 'missing new location guidance'
  assert_private
done
run_shim
[ "$status" -ne 0 ] || fail 'missing command accepted'
run_shim output -chdir=state -raw instance_id
[ "$status" -ne 0 ] || fail 'output allowed -chdir'
[ ! -s "$HANDOFF_CALLS" ] || fail '-chdir reached delegate'
run_shim output -chdir state -raw instance_id
[ "$status" -ne 0 ] || fail 'output allowed split -chdir'
[ ! -s "$HANDOFF_CALLS" ] || fail 'split -chdir reached delegate'

git -C "$root" ls-files -z deploy/aws > "$scratch/tracked"
while IFS= read -r -d '' file; do
  case "$file" in
    *.tf|*/.terraform.lock.hcl|*.tftpl|deploy/aws/host/*)
      [ ! -e "$root/$file" ] || fail "legacy infrastructure source remains: $file" ;;
  esac
done < "$scratch/tracked"
for helper in push-image.sh push-console.sh redeploy.sh put-kubeconfig.sh; do
  [ -f "$root/deploy/aws/$helper" ] || fail "application helper missing: $helper"
done
echo 'tf-handoff: all checks passed (fake tools only)'
