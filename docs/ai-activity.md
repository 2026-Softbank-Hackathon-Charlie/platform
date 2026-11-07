# AI 실행기 활동 조회 (`GET /v1/ai-activity`)

콘솔의 이주 지도가 hopai(AI 실행기)의 실제 호출을 그리도록, 관측 스택(ClickStack, HyperDX)의 ClickHouse에서 hopai 로그를 읽는 엔드포인트다.

## 무엇을 돌려주나

`charlie-platform-hopai` 서비스의 로그 줄을 읽어 **파싱한 필드만** 돌려준다. 원문(클러스터 스펙이 들어 있을 수 있다)은 응답에 없다.

| kind | 로그 줄 | 필드 |
|---|---|---|
| `model` | `message → {"model":…,"stopReason":…,"toolCalls":[…]} [토큰 입력/출력]` | model, stopReason, toolCalls, tokensIn, tokensOut |
| `tool_call` | `tool_call <도구> {…}` | tool |
| `tool_result` | `tool_result <도구> (<결과>) → …` | tool, outcome |
| `blocked` | `blocked <명령> (expected) …` | tool |
| `rehearsal` | `리허설 N회차 (…): 통과=… 엔진=… N초` | passed, outcome, seconds |

위에 맞지 않는 줄(예: 세션 기록 실패 echo)은 버린다. 로그 문구가 바뀌면 `internal/aiobs`의 정규식과 테스트를 같이 고쳐야 한다.

## 설정

켜지 않으면 503 `observability_not_configured`를 답하고 나머지 API는 그대로다.

| 환경 변수 | 뜻 |
|---|---|
| `PLATFORM_AIOBS_CLICKHOUSE_URL` | ClickHouse HTTP 주소. 이 클러스터는 `http://clickstack-clickhouse-clickhouse-headless.observability.svc:8123` |
| `PLATFORM_AIOBS_CLICKHOUSE_USER` | 읽기 전용 사용자 |
| `PLATFORM_AIOBS_CLICKHOUSE_PASSWORD_FILE` | 비밀번호가 든 파일(Secret을 마운트). 환경 변수로 받지 않는다 |
| `PLATFORM_AIOBS_CLICKHOUSE_TABLE` | 기본 `default.otel_logs` (HyperDX의 Logs 소스와 같다) |

쿼리는 `readonly=1`, 5초 제한이고 서비스 이름과 시작 시각은 파라미터로 묶는다. ClickHouse 오류의 본문은 응답과 로그에 싣지 않고 상태 코드만 남긴다(본문이 쿼리나 비밀을 되풀이할 수 있다).

## 배포하려면 (이 저장소 밖, 사람이 할 일)

1. **ClickHouse에 `SELECT` 권한만 있는 사용자를 만든다** (`default.otel_logs`만). HyperDX가 쓰는 `app` 사용자는 쓰기 권한이 있을 수 있으니 재사용하지 않는 편이 낫다.
2. 그 비밀번호를 platform API가 있는 네임스페이스에 Secret으로 만들고 Pod에 파일로 마운트한다.
3. platform API Pod에서 `observability` 네임스페이스의 ClickHouse(8123)로 가는 NetworkPolicy를 연다. 지금은 다른 네임스페이스에서 ClickHouse로 가는 길이 열려 있는지 확인하지 못했다.
4. 위 환경 변수를 Helm values에 넣는다.

`GET /v1/ai-activity`는 `viewer` 권한이면 된다.
