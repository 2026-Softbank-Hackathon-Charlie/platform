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

## DB 쓰기 차단 전 source 읽기 제외

읽기를 target 100%로 설정해도 기존 write backend로의 fallback과 이미 선택된 retry는
source에 연결할 수 있다. DB 접근을 막기 전에 다음 관리자 계약으로 source를 제외한다.
각 앱은 `/apps/{app}` 아래에서도 동일한 경로를 제공한다.

```text
POST /v1/reads/exclude {"backend":"source","lease":"unique-run-owner"}
POST /v1/reads/include {"backend":"source","lease":"unique-run-owner"}
```

- `GET /v1/state`의 `sourceReadExclusionSupported: true`를 먼저 확인한다.
  영속 StateFile이 없으면 false다. 필드가 없는 구버전은 지원으로 추정하지 않는다.
- 새 exclude는 쓰기 Hold와 in-flight 쓰기 0을 요구한다. lease를 저장한 뒤 source의
  기존 읽기·예약된 retry·health probe가 끝나야 200이다. source 제외와 primary·retry
  예약은 같은 router 잠금으로 직렬화한다. 이후 fallback·retry·health도 제외를 지킨다.
- `readExclusions`는 backend별 owner lease, `readInFlight`는 backend별 활성·예약 수다.
  구성된 backend는 0도 명시하며 target 트래픽이 계속돼도 source만 drain한다.
  이미 선택한 retry는 실제 사용하지 않더라도 해당 요청이 끝날 때까지 예약한다.
- drain timeout은 503 `read_drain_timeout`, 저장 실패는 503 `persistence_unavailable`이다.
  실패한 exclude도 현재 프로세스의 제외를 유지하므로 DB 변경 전에 상태를 재확인한다.
  다른 lease는 409이며, timeout만으로 자동 include하지 않는다.
- include는 같은 owner를 검사하고 해제 상태를 먼저 저장한 뒤 메모리에서 해제한다.
  저장 실패는 제외를 유지한다. 제외가 이미 없으면 200이다. 호출자는 먼저 source DB의
  역할·HBA 복원을 확인해야 한다. 게이트웨이는 SQL 상태를 확인하지 않는다.
- lease가 있는 앱은 PUT·DELETE를 거부하고, 제외한 backend로의 Route·양수 읽기 가중치·
  해당 backend가 writer인 상태의 Release를 거부한다. 다른 target으로 Route는 가능하다.
- DELETE한 앱의 이전 읽기·예약 retry·health·쓰기·dispatch 대기가 끝나기 전에는 같은
  이름의 PUT를 409 `app_draining`으로 거부한다. 기존 요청은 끝내고, 삭제 전 router를
  찾아 둔 요청이 이후 새 upstream이나 제어 동작을 시작하는 경계도 차단한다.

lease는 한 실행이 생성한 고유한 1–128자 영숫자·`._:-` 식별자이며 관리자 인증을
대체하지 않는다. restart에도 제외를 복원한다. 활성 lease가 있는 동안 이를 모르는
구버전으로 게이트웨이를 내리면 안 된다. 이 카운터와 ACK는 현재 gateway 인스턴스만의
증명이며 HA 전체 drain이나 HTTP 밖 쓰기 차단 자체의 증명이 아니다.

설계와 호출 순서는 [ADR 0049](../../docs/adr/0049-exclude-source-reads-before-database-fence.md)에 기록한다.

## AI 경로가 앱 위치를 따라가기

`-ai-app`으로 띄운 AI 데모 게이트웨이는 다른 트래픽 게이트웨이에 있는 앱의 쓰기 위치를
따라 AI 경로(`gcp`|`laptop`)를 바꿀 수 있다. 아래 값이 모두 없으면 기능은 꺼지고 동작은 전과 같다.

| 실행 옵션 / 환경 변수 | 의미 |
|---|---|
| `-ai-follow-app` / `AI_FOLLOW_APP` | 따라갈 앱 (예: `moimroom-mig`). 트래픽 게이트웨이에 `PUT /apps/{app}`로 등록된 앱이어야 한다(기본 앱은 지표에 `app` 라벨이 없어 따라갈 수 없음) |
| `-ai-follow-url` / `AI_FOLLOW_URL` | 트래픽 게이트웨이 관리 주소 (http(s), 예: `http://hopcharlie-gateway.charlie-platform.svc.cluster.local:9901`) |
| `-ai-follow-map` / `AI_FOLLOW_MAP` | 앱 backend → AI backend (예: `onprem=laptop,gke=gcp,eks=gcp`), 값은 `gcp`·`laptop`만 |
| `-ai-follow-interval` / `AI_FOLLOW_INTERVAL` | 읽는 주기 (기본 2s, 500ms 이상) |

- 토큰을 쓰지 않는다. 토큰 없이 열린 `GET {url}/metrics?name[]=charlie_gateway_app_write_backend&name[]=charlie_gateway_app_writes_held`
  (1 MiB 제한)에서 그 앱의 `write_backend`(값이 1인 backend가 정확히 하나)와 `writes_held`를 읽는다.
- 시작하자마자 한 번, 그 뒤 주기마다 읽어 바뀌었을 때만 `POST /v1/route`와 같은
  경로(같은 잠금, 진행 중 요청은 원래 backend에서 끝남)로 바꾸고 `AI route follows app` 로그를 남긴다.
- 앱의 `writes_held`가 1이면 `appBackend`·`lastSync`만 갱신하고 바꾸지 않는다. 읽기 실패·200 아님·
  지표 형식 오류·앱 시계열 없음·1인 backend가 0개 또는 2개 이상·맵에 없는 backend면
  경로를 유지하고 `charlie_gateway_ai_follow_errors_total{reason}`을 올린다.
- 대상 AI backend가 unhealthy면 다른 쪽이 healthy일 때 그쪽으로 보낸다(`fallback=true`,
  `charlie_gateway_ai_follow_fallback`). 둘 다 unhealthy면 유지한다.
- 수동 `POST /v1/route`는 따라가기를 멈춘다. `POST /v1/follow {"enabled":true}`(또는
  `/apps/{app}/v1/follow`)로 다시 켜면 바로 한 번 맞추고, `false`로 끌 수 있다. 멈춤은 저장하지 않으므로 재시작하면 다시 따라간다.
- `GET /v1/state`에 `follow: {enabled, app, appBackend, target, fallback, lastSync, lastError}`가 붙는다.
