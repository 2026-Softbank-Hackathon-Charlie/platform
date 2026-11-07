#!/bin/bash
set -uo pipefail
export GOMODCACHE=/go/pkg/mod GOCACHE=/go/build GOPROXY=file:///windows-mod/cache/download,https://proxy.golang.org GOMAXPROCS=4
cp -a /source /tmp/platform
cd /tmp/platform
find . -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.md' -o -name '*.go' \) -exec sed -i 's/\r$//' {} +
go test -p 2 -race -count=1 ./internal/retention ./internal/runs ./internal/api ./internal/store ./cmd/api > /evidence/final-related-race.log 2>&1
race=$?
go vet ./... > /evidence/final-vet.log 2>&1
vet=$?
go build ./... > /evidence/final-build.log 2>&1
build=$?
printf 'final-related-race=%s vet=%s build=%s\n' "$race" "$vet" "$build" > /evidence/final-status.log
cat /evidence/final-status.log
if [ "$race" -ne 0 ] || [ "$vet" -ne 0 ] || [ "$build" -ne 0 ]; then exit 1; fi
