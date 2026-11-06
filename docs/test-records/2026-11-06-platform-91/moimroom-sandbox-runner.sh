#!/bin/bash
set -euo pipefail
export PATH=/tools:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
cd /work
export MOIMROOM_TEST_DATABASE_URL='postgres://postgres:sandbox@hop-issue91-postgres:5432/moimroom?sslmode=disable'
go test -race -count=1 -v ./... 2>&1 | tee /evidence/moimroom-full-race.log
go vet ./... 2>&1 | tee /evidence/moimroom-vet.log
go build ./... 2>&1 | tee /evidence/moimroom-build.log
export MOIMROOM_RESTORE_ORACLE=/tmp/moimroom-oracle.json
MOIMROOM_RESTORE_STAGE=prepare go test -count=1 -v ./internal/api -run '^TestSandboxBackupRestoreRetry$' 2>&1 | tee /evidence/restore-prepare.log
# Both databases and the private backup remain inside this disposable local
# PostgreSQL instance. This is not a cross-environment residency authorization.
docker exec hop-issue91-postgres pg_dump -U postgres -d moimroom -Fc -f /tmp/issue91.dump
start=$(date +%s)
docker exec hop-issue91-postgres createdb -U postgres moimroom_restored
docker exec hop-issue91-postgres pg_restore -U postgres -d moimroom_restored --exit-on-error /tmp/issue91.dump
MOIMROOM_RESTORE_STAGE=replay MOIMROOM_TEST_DATABASE_URL='postgres://postgres:sandbox@hop-issue91-postgres:5432/moimroom_restored?sslmode=disable' go test -count=1 -v ./internal/api -run '^TestSandboxBackupRestoreRetry$' 2>&1 | tee /evidence/restore-replay.log
elapsed=$(( $(date +%s) - start ))
printf 'restore-and-app-validation-seconds=%s\nfixture-writes-after-backup=0\nresidency-scope=same-disposable-local-instance\n' "$elapsed" > /evidence/restore-summary.log
