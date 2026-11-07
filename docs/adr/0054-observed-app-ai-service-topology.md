# 0054. 앱 AI 호출의 관측 근거로 실행 위치 지도를 구성한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

앱이 배포된 환경과 모델이 실행되는 환경은 다르다. GKE 앱이 온프레미스 GPU나 외부 API를 호출할 수 있으므로 앱 배치, 모델 이름, 엔드포인트 문자열만으로 GPU와 트래픽을 표시하면 잘못된 지도가 된다. 기존 `hopai` 활동은 플랫폼 계획 에이전트의 기록이며 앱 AI 사용량이 아니다. moimroom의 `dependency.name=ai` 클라이언트 span은 실제 의존성 호출 근거지만 기존 계측에는 모델 호스트 정보가 없다.

## 결정

`GET /v1/apps/{name}/ai-services?minutes=5`를 viewer 권한의 등록 앱 전용 읽기 API로 추가한다. `minutes` 범위는 1~60이다. 기존 HyperDX/ClickStack 연결의 같은 데이터베이스에 있는 `otel_traces`를 조회한다. 새 키나 권한은 만들지 않는다. 테이블이 없거나 조회가 실패하면 `sources[].status=unavailable`, 미설정이면 `not_configured`, 결과가 없으면 `unobserved`를 반환한다. 모두 AI 미사용의 증명이 아니다. 원본 오류, URL, 프롬프트, 요청/응답 본문은 반환하지 않는다.

등록 앱의 정확한 서비스 이름 또는 `app-` 접두사만 선택하고, 같은 접두사의 더 긴 등록 앱과 플랫폼 `charlie-platform-hopai` 서비스는 제외한다. 24시간 이내 AI 클라이언트 span의 서비스 정보를 최대 128개 집계한다. 최근 요청은 선택한 시간창의 서로 다른 TraceId/SpanId 수이며 샘플링·내보내기 누락이 있는 전체 트래픽 추정치가 아니다. 최근 양수일 때만 `activity.state=active`다. 오래된 관측은 `unknown`이며 유휴 상태를 증명하지 않는다. 호출 환경은 resource의 `deployment.environment.name`/`deployment.environment`, 실제 호스트는 span의 별도 `hopcharlie.ai.environment`다. 모델 이름별로 물리 GPU 노드를 복제하지 않는다.

표준 `gen_ai.provider.name`/`gen_ai.system`으로 Bedrock·Vertex AI·Azure OpenAI를 managed, OpenAI·Anthropic 등의 API를 external로 분류한다. GPU/CPU는 명시적 self-hosted accelerator 속성에만 붙인다. 이 속성은 보고된 배치 근거이며 독립적인 하드웨어 증명은 아니다. 모르는 위치·가속기는 그대로 unknown이다. 모든 표시용 식별자는 길이와 문자 집합을 제한한다.

네이티브 AI 게이트웨이는 실제 선택한 backend와 함께 안전한 응답 메타데이터를 내보낸다. 앱이 이 응답을 자신의 AI 의존성 span에 기록하므로 게이트웨이 기본 앱의 집계값을 모든 호출 앱/백엔드로 복제하지 않는다. 게이트웨이는 클라이언트가 보낸 배치 헤더를 제거하고, upstream 응답·trailer의 동명 헤더도 제거한 뒤 서버 구성값으로 덮어쓴다. 구성하지 않으면 route만 증명하고 위치는 unknown이다.

선택적 서버 환경변수 `AI_GATEWAY_PLACEMENTS` 예시:

```json
{"gcp":{"serviceId":"qwen-gcp","kind":"self_hosted","environment":"gke","accelerator":"gpu"},"laptop":{"serviceId":"qwen-onprem","kind":"self_hosted","environment":"onprem","accelerator":"gpu"}}
```

이는 설정 형식 예시이며 현재 운영 GPU 배치를 증명하지 않는다. 운영자는 실제 배치를 확인한 뒤 값을 설정해야 한다. 환경 이름은 지도와 동일한 `gke`, `eks`, `onprem`을 사용한다. `managed`는 provider `aws.bedrock`, `gcp.vertex_ai`, `azure.ai.openai`와 선택적 region을 받으며 GPU 속성을 허용하지 않는다. `external`은 제공자만 받는다. URL·키·프롬프트는 이 구성에 넣지 않는다. 구성값은 라우팅과 이전 실행을 변경하지 않는다.

응답 헤더 계약은 `X-Hop-Backend`, `X-Hopcharlie-AI-Service-Id`, `Kind`, `Environment`, `Provider`, `Region`, `Accelerator`, `Evidence`다. 뒤의 짧은 이름에는 모두 `X-Hopcharlie-AI-` 접두사가 붙는다. Evidence는 `gateway-route` 또는 `operator-declared-placement`다. moimroom의 별도 계측 PR이 이 계약을 소비하며, 기존 이미지에서는 HTTP AI 사용은 관측되어도 목적지 배치는 unknown이다. 앱의 OTEL resource 환경도 설정되어야 출발 박스에 연결된다.

## 검증과 한계

API 인증·등록 범위·시간창·실패 시 비밀 비노출, 바인딩된 SQL과 앱 제외, 호출 위치/실행 위치 분리, 모델별 중복 제거, upstream·caller 헤더 위조 방어를 단위 테스트한다. 실제 운영 호출·수집 성공·GPU 배치는 이 테스트로 증명하지 않는다. 배포 후 게이트웨이 구성과 앱 이미지 갱신, OTEL 내보내기 및 `/ai-services`의 최신 관측을 별도로 확인해야 한다. 토큰·비용·Langfuse generation 수는 이 API가 생성하지 않는다.
