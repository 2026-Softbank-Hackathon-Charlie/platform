#!/bin/bash
set -uo pipefail
export GOMODCACHE=/go/pkg/mod GOCACHE=/go/build
export GOPROXY=file:///windows-mod/cache/download,https://proxy.golang.org GOMAXPROCS=4
cp -a /source /tmp/platform
cd /tmp/platform
find . -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.md' -o -name '*.go' \) -exec sed -i 's/\r$//' {} +
go test -count=1 -v ./internal/store -run '^TestClusterLeaseAcrossPools$' > /evidence/postgres-locks.log 2>&1
locks=$?
go test -p 2 -race -count=1 ./... > /evidence/linux-full-race.log 2>&1
race=$?
go vet ./... > /evidence/vet.log 2>&1
vet=$?
go build ./... > /evidence/build.log 2>&1
build=$?
printf 'postgres-locks=%s race=%s vet=%s build=%s\n' "$locks" "$race" "$vet" "$build" > /evidence/status.log
cat /evidence/status.log
if [ "$locks" -ne 0 ] || [ "$race" -ne 0 ] || [ "$vet" -ne 0 ] || [ "$build" -ne 0 ]; then exit 1; fi
