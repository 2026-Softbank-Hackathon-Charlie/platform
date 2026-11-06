#!/bin/bash
set -uo pipefail
export GOMODCACHE=/go/pkg/mod
export GOPROXY=file:///windows-mod/cache/download,https://proxy.golang.org
export GOMAXPROCS=4
cp -a /source /tmp/platform
cd /tmp/platform
find . -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.md' -o -name '*.go' \) -exec sed -i 's/\r$//' {} +
go test -p 2 -race ./... > /evidence/full-race.log 2>&1
code=$?
cat /evidence/full-race.log
printf 'linux-final-race-exit=%s\n' "$code" > /evidence/full-race-status.log
exit "$code"
