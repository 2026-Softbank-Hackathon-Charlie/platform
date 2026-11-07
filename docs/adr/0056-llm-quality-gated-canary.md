# 0056. 로컬 LLM 의존성 전환은 실제 품질 점수를 통과한 카나리로 제한한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

전용 AI 게이트웨이의 `/api/chat`은 POST이며 `writeBackend` 한 곳으로만 전달된다.
일반 `readWeights` 조절로는 LLM 카나리가 되지 않는다. HTTP 200, 토큰 수와 비용은
추론 호출의 관측값이며 응답의 업무 정확성이나 안전성 증거가 아니다. 같은 모델을
GCP GPU에서 노트북으로 옮겨도 양자화, 템플릿, 검색 자료와 실행 버전 차이가 있다.

## 결정

운영자 소유 정책 파일과 Langfuse 평가 프로젝트 설정을 지정한 전용 AI 게이트웨이에
별도의 카나리 통제를 적용한다. 기본은 비활성이고 이 변경만으로 운영 트래픽을 바꾸지 않는다.

- 앱, 평가 프로젝트, 원본/대상 모델 revision, 프롬프트·검색·데이터셋 revision,
  평가자 revision과 독립 judge 모델, score config ID 및 지표 기준을 서버에서 고정한다.
- 20~100개의 운영자가 선정한 데이터셋 item 각각에 양쪽 추론과 모든 필수 점수가 있어야 한다.
  동일 배치의 서로 다른 실제 trace/observation을 대조한다. 누락·중복·다른 앱·미래 시각·만료·
  미지원 API는 `unknown`이며 통과로 간주하지 않는다. 최신 배치가 미완성이면 예전 통과 배치로
  대체하지 않는다. 데이터셋 item 원문이나 모델 응답을 이 통제기가 복사하지 않는다.
- 업무 점수의 paired 차이(target-source)에 대한 95% 단측 정규근사 하한이 `-허용폭` 이상이고,
  원본과 대상의 최소 점수 기준을 만족해야 `pass`다. 상한까지 `-허용폭` 미만일 때만
  `regressed`다. 그 사이의 불확실성은 `unknown/quality_inconclusive`다.
  안전 점수는 필수이며 개별 item 실패도 hard failure다. 형식·도구 지표도 필요하면 hard failure로 둔다.
- 운영자가 인증된 관리 API에서 CAS version, Idempotency-Key로 0→5→25→50→100% 순서로
  한 단계씩 요청한다. 매번 점수를 다시 읽고, 이전 단계 이후에 생성된 점수와 최소 관측 시간을
  요구한다. 비율은 디스크에 저장한 뒤에만 실제 POST 추론 선택에 노출한다.
- 요청 ID의 안정적 해시로 한 backend만 선택한다. shadow 추론, 대상 오류 후 원본 재호출,
  tool 중복 실행을 추가하지 않는다. 기존 non-streaming 계약, 요청 인증, 쓰기 hold,
  in-flight 집계와 12초 제한을 유지한다. 세션 정합성을 새로 보장한다고 주장하지 않는다.
- 통제가 활성화된 앱은 기존 `/v1/route`, `/v1/weights`로 우회할 수 없다. default app의
  PUT 교체도 기존 정책대로 금지된다. 설정을 제거해 기존 카나리 저장 상태를 읽는 부팅도 거부한다.
- 15초마다 읽기 전용으로 재평가한다. `unknown`, 만료, 대상 불건전 또는 읽기 제외 시에는
  신규 요청을 원본으로 보낸다. 확인된 퇴행에는 AI 비율만 0으로 내리고 version을 올린다.
  DB writer나 이전 엔진 상태를 변경하거나 `PromoteRequested` 이후 DB를 롤백하지 않는다.
  이미 전달한 요청은 기존 제한 시간 내에 끝나며 되감지 않는다.
- 재시작 후 저장된 통과 판정을 신뢰하지 않고 다시 평가할 때까지 원본을 쓴다.
  운영자가 명시적으로 0% 복구하는 데에는 평가 시스템 가용성이 필요하지 않다.

## 검토한 대안

1. Langfuse UI의 성공 호출·토큰 지표로 판단: 의미 품질이나 퇴행을 증명하지 못한다.
2. 일반 읽기 비율 조절: POST 추론에는 적용되지 않는다.
3. 응답 문자열 동일성: 표현이 다른 정답을 실패시키고, 같은 오답을 통과시킨다.
4. 완전 자동 승격: 대표 데이터·judge 보정·정책이 아직 검증되지 않아 운영자 단계를 유지한다.

## 결과

이 구현은 거친 품질 선별과 실제 전환 차단 장치다. 최소 20쌍이나 정규근사 신뢰구간은
통계 검정력, 무오류, 의미적 동일성을 보장하지 않는다. 높은 위험에는 더 큰/분류별 표본,
사람이 검수한 라벨, 다중 평가자 및 별도 안전 검증이 필요하다. 비용과 지연은 품질 점수와 분리해
관측해야 하며 현재 통과 조건에 비용 절감을 품질 대용으로 쓰지 않는다.

점수 metadata는 신뢰된 평가 생산자의 선언이다. 모델 artifact 실행 증명은 아니다.
평가 프로젝트의 쓰기 권한을 앱 trace writer와 분리하고, 고정 revision의 실제 배포를 운영자가
검증해야 한다. 대상 자신이 judge가 되는 설정은 거부하지만 alias나 허위 metadata를 이 코드가
독립적으로 attestation하지 못한다. 게이트웨이의 public ingest 키에 평가 쓰기 권한을 주면 안 된다.

Langfuse의 공식 `/api/public/v3/scores` cursor API를 사용하며, 해당 endpoint/필드가 없는
서버는 `unknown`으로 차단한다. 평가 실행과 score 발행, 대표 데이터 선정, 승인된 거주성 안에서의
모델 비교는 평가 파이프라인의 책임이다. 이 변경은 유료 judge 호출이나 운영 프롬프트 복제를
자동 실행하지 않는다. 기존 전용 gcp/laptop native 추론 경로에 한정되며, 모든 등록 앱의 LLM
SDK를 자동 interception하거나 앱/DB 이전 전체를 자동 차단하는 기능은 아니다.

근거: [Langfuse score 모델](https://langfuse.com/docs/evaluation/scores/overview),
[평가 데이터셋과 experiments](https://langfuse.com/docs/evaluation/core-concepts),
[judge 보정 지침](https://langfuse.com/docs/evaluation/evaluation-methods/llm-as-a-judge),
[공식 score API 계약](https://github.com/langfuse/langfuse/blob/main/fern/apis/server/definition/scores.yml).
