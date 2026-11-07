# 부하 예산 SDK 연결 검증

2026-11-07, Docker `golang:1.26.5`, SDK `v0.0.0-20261107034439-c508dbf3ba75`.

`runner.sh`는 읽기 전용 소스 복사본에서 Windows CRLF를 LF로 정규화한 후 검사를 실행한다. 소스 코드나 단언은 변경하지 않는다. 사설 SDK는 Windows Go 모듈 캐시를 읽기 전용으로 사용했다.

- `go test -p 2 -race -count=1 ./...`: 통과. API, runs, retention 및 전체 패키지 포함.
- `TestEngineFactoryRejectsInvalidOperatorLoadPolicy`: 알 수 없는 정책 필드가 있을 때 실제 SDK가 상태 디렉터리 생성 전 거부하는지 확인한다.
- `go vet ./...`, `go build ./...`: 결과는 `status.log` 및 각 원시 로그에 기록한다.

엔진의 PostgreSQL 배치/타임아웃 검사 및 부하 가드 검사는 [엔진 PR #22](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pull/22)의 증거를 참고한다. 이 플랫폼 검사는 실제 운영 Prometheus, CNPG 이전이나 부하 절감률을 검증한 것이 아니다. 정책 및 운영 배포는 수행하지 않았다. 공유 Go 캐시는 유지하고 이 검사에서 생성한 실행 컨테이너만 정리한다.
