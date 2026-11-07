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

쿼리는 5초 제한, 최대 2,000행이고 서비스 이름과 시작 시각은 파라미터로 묶는다. 읽기 전용은 ClickHouse 사용자의 profile이 정한다(요청에서 `readonly` 설정을 보내지 않는다. 이 클러스터의 `app`은 `readonly: 2`라 그 설정을 바꿀 수 없다). ClickHouse 오류의 본문은 응답과 로그에 싣지 않고 상태 코드만 남긴다(본문이 쿼리나 비밀을 되풀이할 수 있다).

## 배포하려면

infra 저장소 PR이 하는 일(chart의 `api.aiobs`, NetworkPolicy, 관리 환경 values)과 ADR 0077을 따른다.

1. ClickHouse 사용자는 이미 있는 읽기 전용 `app`(`GRANT SELECT ON default.*`, profile `hyperdx-read`)을 쓴다. 전용 사용자 분리는 후속이다.
2. 사용자가 `scripts/put-clickhouse-read-password.sh`로 `app`의 비밀번호를 관측 Secret에서 SSM `/hopcharlie/clickhouse-read-password`로 옮긴다(값은 출력되지 않는다).
3. API가 init container에서 그 값을 `/config/aiobs-clickhouse-password`로 받고 `PLATFORM_AIOBS_CLICKHOUSE_PASSWORD_FILE`로 읽는다.
4. NetworkPolicy가 API Pod에서 `observability`의 ClickHouse Pod 8123으로 가는 길을 연다.

`GET /v1/ai-activity`는 `viewer` 권한이면 된다.

## 앱별 관측 요약 (`GET /v1/apps/{name}/observability`)

콘솔의 앱 화면에서 "관측" 팝업이 쓴다. 같은 ClickHouse 설정을 쓰고 `viewer` 권한이면 된다.

- **등록된 앱만** 답한다(파일 앱 또는 저장소의 앱). 모르는 이름은 ClickHouse를 부르기 전에 404이므로, 이름으로 플랫폼 자신의 서비스 로그를 읽을 수 없다.
- 앱의 서비스는 `ServiceName`이 앱 이름과 같거나 `<앱>-`로 시작하는 것이다(`docker-doom-web`, `moimroom-mig-db`). 앱 이름이 다른 앱 이름의 앞부분이면(예: `moimroom`과 `moimroom-mig`) 앞의 앱이 뒤 앱의 로그도 센다.
- 답: 줄/오류/경고 합계, 서비스별 합계와 마지막 시각, 5분 단위 시계열, 많이 반복된 오류·경고 패턴(최대 10개, 오류 먼저).
- **일반 로그 줄은 돌려주지 않는다.** 오류·경고 줄만 패턴으로 돌려주고, 그 전에 가린다(`Redact`): URL의 비밀번호, Bearer/Basic 토큰, 이름에 password·token·secret·key·cookie가 든 항목의 값, 이메일, AWS 키, 글자와 숫자가 섞인 긴 문자열이나 32자 이상 문자열. 숫자와 id는 `#`, `«id»`로 접어 같은 실패를 하나로 센다. 모든 비밀을 알아볼 수는 없다.
- 쿼리 세 개(서비스별, 5분별, 오류·경고 줄 최대 500개). 모두 파라미터로 묶고 5초 제한이다.
- 미확인: 실제 ClickHouse에서의 실행, `SeverityText`가 소문자 `error`/`warn`으로 들어온다는 가정(수집기의 transform이 소문자로 바꾼다는 설정을 읽었다).
