# platform #87 최초 파일 검사 개선 검증

## 변경과 결과

최초 전체 검사를 생략하지 않고 목록과 manifest를 디스크 기반 streaming으로 처리한다. 객체별 rclone cat 대신 최대 256개씩 제한된 copy를 사용한다. 전체 검사·정책 승인·staging 재검사 이후에만 target에 전송한다. 설계와 한계는 [ADR 0034](../../adr/0034-fileguard-cold-start-pipeline.md)에 기록했다. 완성도·클라우드 활용 수준과 데모 (a)의 파일 이전 검증에 기여한다.

동일 로컬 MinIO에서 각 데이터셋을 3회 측정한 중앙값이다. 원시 로그는 baseline-minio.log와 improved-minio.log에 보존했다.

| 데이터셋 | 변경 전 | 변경 후 | 전체 프로세스 수 |
| --- | ---: | ---: | ---: |
| 작은 텍스트 32개, 각 256 B | 12.839초 | 2.460초 | 35 → 5 |
| 텍스트 4개, 각 1 MiB | 3.961초 | 3.331초 | 7 → 5 |
| ZIP 4개, 각 압축 전 256 KiB | 3.095초 | 2.292초 | 7 → 5 |
| 정책 거부 데이터셋 | 12.185초 | 1.423초 | 34 → 3 |

작은 파일의 다운로드 프로세스는 32→1, 원격 요청은 230→233이다. 프로세스 감소가 요청 수 감소를 뜻하지 않는다. 거부 데이터셋은 양쪽 모두 업로드 0건이다. 검사 이력은 비어 있지만 OS/backend cache는 flush하지 않았다. RSS는 parent와 최대 개별 child를 각각 측정하며 전체 동시 RSS가 아니다. staging peak는 10ms sampling 값이다.

## 확인한 검사

- 기존 Linux 전체 `go test -p 2 -race ./...`: exit 0 (full-race.log, full-race-status.log). Windows checkout을 격리 Linux 사본에서 LF로 정규화했다.
- 기존 `go vet ./...`, `go build ./...`: 모두 exit 0 (vet-build.log).
- 기존 실제 rclone 로컬 통합 테스트: 통과 (rclone-integration.log).
- 기존 fileguard/preflight race: 통과 (related-race.log).
- 기존 전용 1 MiB tmpfs의 실제 staging 용량 부족: 승인·업로드 이전 실패 및 임시 파일 정리 통과 (staging-enospc.log).
- 2026-11-07 Windows Go 1.26.5에서 현재 코드의 `go test -count=1 ./internal/fileguard ./internal/preflight/...`: 통과 (windows-final-related.log). Windows에 rclone이 없어 실제 rclone 통합 테스트는 이 실행에서 건너뛰었다. 위 Linux 증거와 구분한다.
- 최종 `git diff --check`: 통과.

부분/실패 목록, 객체 제한, chunk 간 경로 충돌, 원본 추가·삭제·크기 변경, 다운로드 오류, 취소, manifest 호환성과 inventory 변조를 회귀 테스트로 확인한다. 실행 경계 검토에서는 서버 소유 remote 환경, 비식별 오류, 승인 이전 target 전송 차단, 승인 후 staging digest 비교를 확인했다.

## 재현과 범위

platform87-test.sh는 Linux 전체 race runner, issue87-bench.sh는 MinIO baseline/변경 후 benchmark runner다. /source, /baseline, /evidence, /tools 및 전용 sandbox MinIO 설정이 필요하며 완전한 환경 생성 스크립트는 아니다. baseline에도 동일 benchmark harness가 필요하다. 스크립트의 sandbox 자격증명은 로컬 테스트 전용이다. 최초 기록은 workspace migration-engine/docs/test-records/2026-11-06-platform-87에 있으며 해당 원본은 수정하지 않았다.

2026-11-07 남아 있던 전용 hop87-minio 컨테이너를 중지했다. 컨테이너와 볼륨은 보존했다. 실제 AWS S3/GCS, 운영 버킷, 실시간 migration, 원격 동시 쓰기의 정합성은 검증하지 않았다. 같은 크기의 원본 변경을 항상 감지하는 snapshot/fence나 검사 이력 재사용은 제공하지 않는다. 커밋·푸시·배포는 수행하지 않았다.
