#!/bin/bash
set -euo pipefail
export PATH=/tools:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export FILEGUARD_COLD_BENCH=1 FILEGUARD_MINIO_ENDPOINT=http://hop87-minio:9000
export FILEGUARD_BENCH_SOURCE=bench:source FILEGUARD_BENCH_TARGET=bench:target
export RCLONE_CONFIG_BENCH_TYPE=s3 RCLONE_CONFIG_BENCH_PROVIDER=Minio
export RCLONE_CONFIG_BENCH_ACCESS_KEY_ID=sandbox RCLONE_CONFIG_BENCH_SECRET_ACCESS_KEY=sandbox-minio-only
export RCLONE_CONFIG_BENCH_ENDPOINT=http://hop87-minio:9000 RCLONE_CONFIG_BENCH_FORCE_PATH_STYLE=true
rclone mkdir bench:source --config /dev/null --log-level ERROR
rclone mkdir bench:target --config /dev/null --log-level ERROR
if [ "${FILEGUARD_SKIP_BASELINE:-}" != 1 ]; then
 cd /baseline
 go test ./internal/fileguard -run '^$' -bench BenchmarkColdTransfer -benchtime=1x -count=3 -benchmem > /evidence/baseline-minio.log 2>&1
 cat /evidence/baseline-minio.log
fi
cd /source
go test ./internal/fileguard -run '^$' -bench BenchmarkColdTransfer -benchtime=1x -count=3 -benchmem > /evidence/improved-minio.log 2>&1
cat /evidence/improved-minio.log
go test -count=1 -v ./internal/fileguard > /evidence/rclone-integration.log 2>&1
go test -race -count=1 ./internal/fileguard ./internal/preflight/... > /evidence/related-race.log 2>&1
