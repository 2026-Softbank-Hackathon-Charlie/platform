# 0055. 앱별 Langfuse generation 표본만 읽고 미관측 지표는 null로 남긴다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

앱 관측 화면은 HyperDX 로그만 요약하며 Langfuse 연결은 없었다. Langfuse가 설치돼 있다는 것만으로 앱의 실제 LLM generation, 토큰 또는 비용을 관측했다고 판단할 수 없다. HTTP AI 의존성 span 역시 Langfuse generation과 다르다.

## 결정

- `GET /v1/apps/{name}/llm-observability?minutes=60`은 기존 viewer 이상 역할과 등록 앱 확인을 거친다. 앱명과 기간 외의 project·URL·filter 선택을 허용하지 않는다.
- 서버의 `PLATFORM_LANGFUSE_CONFIG` 파일에 앱별 프로젝트 ID, generation metadata 키와 mounted public/secret key 파일 경로를 지정한다. metadata 값은 등록 앱명과 정확히 일치해야 한다. 호출자는 이 binding을 바꿀 수 없다.
- 고정 base URL에 GET만 보낸다. 외부 연결은 HTTPS, HTTP는 loopback 또는 클러스터 service DNS만 허용하고 redirect와 환경 proxy를 사용하지 않는다. 프로젝트 키의 `/api/public/projects` 결과가 설정한 단일 프로젝트와 같아야 한다.
- `/api/public/health`의 버전으로 v3 observations v1, v4 observations v2를 선택한다. 알 수 없는 버전/API, 인증·수집 오류, 설정 누락, 앱 범위 누락과 일치하는 generation 미관측을 구분한다.
- 기간·GENERATION·앱 metadata를 서버 query에 넣고 각 반환 행의 프로젝트·앱 metadata·기간·유형을 다시 확인한다. 최대 100개 generation만 읽으며 표본의 수치임을 명시한다. 같은 trace의 여러 generation은 별도로 보존한다.
- 입력·출력·프롬프트·provider 오류 원문·전체 metadata를 반환하거나 로그로 남기지 않는다. v4는 I/O field group을 요청하지 않는다. v3의 전체 응답은 서버 메모리에서 whitelist DTO로 축소한다.
- 실제 zero와 미관측을 구분하기 위해 지표를 nullable로 둔다. 요약의 metric은 모든 표본 행에서 그 값이 확인될 때만 합산하고, 평균 latency는 모든 duration이 확인될 때만 계산한다. 수집 성공이나 이 표본을 품질 평가·이전 gate 통과로 바꾸지 않는다.

## 검토한 대안

- 브라우저가 Langfuse API를 직접 호출: project secret을 브라우저에 공급해야 하므로 제외했다.
- 전 프로젝트 데이터를 조회해 앱 이름 부분 문자열로 분류: 다른 앱의 trace가 섞일 수 있어 exact metadata와 프로젝트 확인을 선택했다.
- generation 없는 HTTP span의 토큰·비용을 추정: 관측하지 않은 수치를 생성하므로 제외했다.
- 모든 generation을 무제한 조회해 전체 수치로 집계: 응답 크기와 조회 시간을 제한하기 위해 표본을 선택했다. 전체 비용 집계는 별도 metrics 계약이 필요하다.

## 결과

HTTP fixture 테스트로 v3/v4, project/app scope, 0/null, redaction, redirect와 등록·인증 경계를 검증한다. 2026-11-08 운영 Pod metadata에서 Langfuse Web/Worker 4.75.0과 ClusterIP 서비스를 확인했다. 실제 프로젝트 키와 앱 metadata binding을 조회하지 않았으며 실제 앱 generation 조회·설정 배포·앱 LLM 계측은 검증하거나 수행하지 않았다. 연결되지 않은 운영 서버는 미연결 또는 범위 미설정으로 표시한다.

공식 계약: [Public API](https://langfuse.com/docs/api-and-data-platform/features/public-api), [호환성](https://langfuse.com/self-hosting/upgrade/versioning), [v3.133.0 OpenAPI](https://github.com/langfuse/langfuse/blob/v3.133.0/web/public/generated/api/openapi.yml).
