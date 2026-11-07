# LLM 품질 카나리 운영 계약

대상은 `cmd/gateway -ai-app <default-app>`의 gcp/laptop 전용 native `/api/chat`이다.
운영 적용 전 별도 검증과 승인이 필요하다. 이 문서는 자동 배포나 트래픽 변경을 실행하지 않는다.

현재 운영 metadata에서 native service identity는 `moimroom-ai`다. `moimroom-mig`의
앱 관측 binding과 별개이며, 정책 `app`은 반드시 이 gateway의 default-app/ai-app과 같아야 한다.
서로 다른 앱이 공유하는 inference gateway에 한 앱의 평가 결과를 전체 적용하면 안 된다.
이 기능은 동일 서비스 소유의 caller와 전용 AI bearer token을 쓰는 경계에만 적용한다.
caller가 보내는 임의 앱명 헤더를 신뢰하거나 모든 caller의 업무 품질을 검증했다고 주장하지 않는다.

## 연결

`AI_QUALITY_POLICY_FILE`(`-ai-quality-policy`)과 `AI_QUALITY_LANGFUSE_CONFIG_FILE`
(`-ai-quality-langfuse-config`)을 함께 지정한다. 빠지거나 잘못된 설정은 시작을 거부한다.
기존 state의 writer는 source `gcp`여야 한다. 운영자가 배포 artifact의 실제 revision을 확인해야 한다.

Langfuse 설정은 기존 `llmobs.Config`와 같다. `baseUrl`, `apps[앱명].projectId`,
`metadataKey`, 절대경로 `publicKeyFile`/`secretKeyFile`만 지정한다. 키 값을 JSON이나 명령행에 넣지 않는다.
앱 관측용 프로젝트와 **분리된 평가 프로젝트** 및 신뢰 평가 생산자 쓰기 권한을 사용한다.
API caller가 URL, project, judge, score 필터나 자격증명을 전달할 수 없다.

정책 JSON 필드:

| 필드 | 의미 |
|---|---|
| app, projectId | 등록 앱명과 평가 전용 프로젝트의 정확한 ID |
| sourceBackend, targetBackend | 현재 지원은 gcp, laptop |
| sourceModel, targetModel | 실제 artifact에 고정한 model revision; 단순 latest alias 사용 금지 |
| datasetRevision, promptRevision, retrievalRevision | 비교에 사용한 불변 버전. 검색을 안 하면 명시적 `none` |
| evaluatorRevision, judgeModel | 사람이 보정하고 허용한 평가자 버전과 독립 judge 모델 |
| scoreSource | `EVAL` 기본값. 분리된 신뢰 평가 프로젝트에만 프로그램 평가용 `API` 허용 |
| items | 원문 없는 dataset item ID 20~100개, 중복 금지 |
| maxAgeSeconds | score 유효기간 60~86400초 |
| minSoakSeconds | 단계 간 관측 시간 30~86400초 |
| metrics | 2~4개. name, configId, minimum(0~1), maxRegression(0~0.2), hardFailure |

필수 `safety` 지표는 `minimum:1, maxRegression:0, hardFailure:true`다.
적어도 하나의 업무 정확성 지표를 함께 둔다. 필요하면 structured output/도구 정확성 지표를 추가한다.

각 Langfuse score는 공식 v3 score API의 `projectId/name/configId/source/dataType/value/timestamp`
및 실제 trace/observation `subject`를 갖는다. NUMERIC [0,1] 또는 BOOLEAN만 지원한다.
반드시 다음 string metadata를 평가 생산자가 기록한다. `app` 키만 binding의 metadataKey로 변경 가능하다:

`app`, `backend`, `modelRevision`, `datasetRevision`, `promptRevision`, `retrievalRevision`,
`evaluatorRevision`, `judgeModel`, `itemId`, `evaluationBatch`.

이 metadata에 입력/출력·토큰·키·URL을 넣지 않는다. item마다 원본/대상의 서로 다른 실제
추론 subject가 필요하며 동일 추론에 서로 다른 지표를 붙이는 것은 허용한다. 최신 batch의 전체
지정 item×backend×metric이 모일 때까지 차단한다. 매 단계 이후 점수를 새로 생성해야 한다.
페이지당 100개, 최대 10페이지이며 초과·반복 cursor·중복 score는 unknown이다.

## 관리 API

기존 gateway 관리 주소(기본 9901)의 Bearer admin 인증을 사용한다. 공개 플랫폼 API나
프론트엔드에 새 secret/제어 권한을 노출하지 않는다. `/apps/{default-app}` prefix도 지원한다.

`GET /v1/ai/canary`는 `version`, `targetPercent`(의도한 비율), `effectiveTargetPercent`
(현재 admission에서 허용한 비율), `sourceBackend`, `targetBackend`, `lastAdvanceAt`,
`policyDigest`, `verdict`를 반환한다. 시각은 RFC3339, 비율은 정수 0~100이다.
`verdict`에는 status/reason/checkedAt/validUntil/batch/pairs 및 지표 평균·paired 구간만 있다.
원문, 점수 comment, 전체 metadata, 자격증명은 반환하지 않는다.

`POST /v1/ai/canary/advance`:

```http
Idempotency-Key: operator-operation-unique-id
Content-Type: application/json

{"expectedVersion":0,"targetPercent":5}
```

200은 해당 stage가 영속화되어 신규 추론 선택에 적용됐다는 뜻이며 전체 이전 완료가 아니다.
최근 성공 명령의 같은 키/같은 본문 재시도는 중복 실행하지 않는다. 오래된 명령은 version 충돌로
거부한다. 같은 키에 다른 본문은 409다. 다중 gateway 인스턴스 분산 CAS는 지원하지 않는다.
진행은 `0→5→25→50→100`; 0 복구는 모든 stage에서 가능하다.

- 400: 잘못된 JSON, version/key/비율
- 401: 관리 인증 실패
- 404: 통제 비활성 또는 다른 앱
- 409: version/단계/관측시간/hold 충돌 또는 기존 route/weights 우회 시도
- 412: 품질 unknown 또는 regressed, 승인 불가. verdict를 확인한다.
- 503: 상태 저장 실패. 기존 비율이 증가하지 않는다. state/readyz를 조회하고 복구한다.

운영자는 단계별 실제 트래픽·비용·지연·품질 표본을 검토하고 다음 요청을 보낸다.
자동 승격은 하지 않는다. 15초 주기의 품질 판정 변경과 stage/회귀는 기존 구조화 로그에 기록된다.
현재 인증 구조는 공유 admin token이므로 개별 사람의 identity를 별도로 증명하지 않는다.
확인된 회귀는 AI 신규 트래픽만 원본으로 복구하며, 데이터베이스 롤백은 실행하지 않는다.

## 검증 범위

단위·HTTP 테스트는 가짜 upstream을 테스트 코드 안에서만 사용한다. 운영 모델 품질이나
실제 이전 성공의 증거가 아니다. 현재 구현에는 UI, 평가 runner/score publisher, 자동 데이터셋
수집, 세션 affinity, 새 모델 배포 또는 모든 앱 대상의 LLM SDK 프록시가 포함되지 않는다.
