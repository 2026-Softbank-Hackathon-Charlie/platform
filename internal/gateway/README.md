# 게이트웨이 쓰기 보호와 지연 관측

읽기 GET/HEAD/OPTIONS는 가중치, 다른 메서드는 단일 write backend로 전달한다.
안전한 이전 제어 순서는 `HoldWrites → Route → ReleaseWrites`이다.

## 시간·부하 설정

| 실행 옵션 | 기본값 | 의미 |
|---|---|---|
| `-max-hold` | 55s | hold와 dispatch 슬롯을 기다릴 수 있는 요청별 시간 |
| `-held-body-timeout` | 10s | 보류 중 처음 64KiB 본문 수신 시간, max-hold로도 제한 |
| `-admin-body-timeout` | 10s | 관리자 JSON 본문 수신 시간. 이후 drain·dispatch 대기는 포함하지 않음 |
| `-max-concurrent-writes` | 64 | 앱별 동시 전달 요청, DB 내부 실행 수의 보장은 아님 |
| `-max-buffered-bytes` | 67108864 | 모든 앱의 보류 본문 예약 예산 합계 |
| `-max-held` | 2000 | 앱별 보류 요청 수, 본문 수신 중인 요청 포함 |
| `-write-timeout` | 2m | 실제 전달을 시작한 쓰기 요청의 기존 제한 |

정상 부하/전환 부하를 나눠 아래 지표의 p95/p99와 실패율을 보고 동시성을 정한다.
제한 시간을 낮춰 요청을 실패시키는 것은 성공한 쓰기의 지연 최적화가 아니다.

## 메트릭

기본 앱은 `charlie_gateway_`, 등록 앱은 `charlie_gateway_app_` 접두사와 `app` label을 쓴다.

- `request_duration_seconds{backend,class}`: 기존 backend 처리 지연, hold 제외.
- `end_to_end_duration_seconds{class,code}`: handler 도착부터 종료까지, hold/dispatch 대기 포함.
- `hold_wait_duration_seconds{outcome}`: 요청이 hold를 빠져나오기까지, 초기 본문 읽기 포함.
- `write_queue_duration_seconds{outcome}`: 전달 동시성 슬롯 대기, backend 실행 제외.
- `write_pause_duration_seconds{outcome}`: 전체 hold 시작→해제, drain 포함. hold 종료 시 기록.
- `charlie_gateway_buffered_bytes`, `charlie_gateway_buffer_budget_bytes`: 전 앱의 예약량/한도.
- `charlie_gateway_persistence_enabled`, `charlie_gateway_persistence_healthy`: 저장 설정/오류 상태.

Prometheus/HyperDX의 Prometheus 데이터 소스에서는 예를 들어 아래처럼 등록 앱별 쓰기
전체 응답 p95를 조회할 수 있다. 실제 scrape·dashboard 연결 여부는 따로 확인해야 한다.

```promql
histogram_quantile(0.95,
  sum by (app, le) (
    rate(charlie_gateway_app_end_to_end_duration_seconds_bucket{class="write"}[5m])
  )
)
```

5분을 넘는 표본은 overflow bucket에 들어간다. API의 백분위는 유한 버킷 기반 추정치로,
이 경우 마지막 경계값을 정확한 실제 지연으로 해석하면 안 된다.

## 저장 오류와 준비 검사

루트 `/healthz`는 생존 검사다. `/readyz`와 인증된 `/apps/{app}/healthz`는 저장 오류가
있으면 503 `persistence_unavailable`이다. raw filesystem 오류나 경로를 응답에 넣지 않는다.
이미 적용한 route를 저장 오류만으로 원복하지 않는다. release 전 저장 실패는 hold를 유지한다.
새 hold의 저장 실패는 해당 hold를 취소하고 503으로 거부한다.

`PUT /apps/{app}`에 선택적 `healthPath`를 지정할 수 있고 GET/응답과 재시작 상태에 보존된다.
예: `{"backends":{"eks":"http://shop-web:80"},"writeBackend":"eks","healthPath":"/readyz"}`.
생략 시 기존 gateway 기본 경로를 사용한다. 배포 실행기는 public service `healthCheck`를
연결한다. 실제 DB·스토리지 readiness 여부는 이 경로를 구현한 앱의 책임이다.

`dial`(백엔드 이름에서 `host:port`로 가는 맵)은 ADR 0042의 계약이다. 백엔드 URL의 호스트를
TLS 이름과 Host로 두고 그 주소로 접속한다. 아직 구현하지 않아서 비어 있지 않으면 400
`dial_not_supported`다.

## 관리자 요청 본문과 제어 잠금

`PUT /apps/{app}`의 본문 수신·순수 검증은 전역 제어 잠금 밖에서 한다. 느린 등록
요청 때문에 다른 앱의 Hold·Route·Release가 기다리지 않는다. 실제 교체는 잠금
안에서 현재 Hold 상태를 다시 검사하므로 본문을 받는 동안 시작된 Hold를 덮어쓰지 않는다.

관리자 JSON 본문은 전체 64KiB와 수신 10초를 제한한다. 제한 시간 초과는
408 `body_timeout`, 크기 초과는 413 `body_too_large`, 잘못된 JSON·추가 JSON 값은
400 `invalid_request`이며 제어 상태는 바꾸지 않는다. JSON 한 개 뒤 공백은 허용한다.
이 제한은 일반 앱 요청이나 Hold의 drain·Release의 dispatch 대기를 제한하지 않는다.

설계 근거: [ADR 0040](../../docs/adr/0040-gateway-durability-and-write-bounds.md).
