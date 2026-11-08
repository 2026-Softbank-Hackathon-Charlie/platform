# 0060. 앱·환경에 바인딩한 관측 수신 증거와 선택적 차단

- 상태: 채택(구현), 운영 설정·온프레미스 수집기 활성화 미적용
- 날짜: 2026-11-08

## 배경

온프레미스의 승인된 메타데이터 조회에서 `observability` namespace와 표준 수집기, `moimroom-mig`·`hello-onprem` Deployment의 OTEL 환경변수가 없음을 확인했다. 중앙 관측 저장소의 온프레미스 수신 행 수는 조회하지 않아 미측정이다. 중앙 수집기가 실행 중이라는 사실이나 같은 namespace의 다른 환경 메트릭은 온프레미스의 수신 증거가 아니다.

기존 동등성은 대상 Prometheus `up` 신선도만 검사하며 전체 audit 모드다. 로그·트레이스 전달, 원본과 대상 비교, 환경별 시계열 구분이 부족했다. 운영 API도 오래된 MinIO 이미지여서 새 AI 관측 경로가 404였고 `PLATFORM_LANGFUSE_CONFIG` 설정이 없었다. 이는 AI 미사용이나 요청 0건이라는 증거가 아니다.

## 결정

- `PLATFORM_PARITY_FILE`의 앱별 `observability` 설정으로 필요한 신호와 정확한 서비스 이름을 운영자가 지정한다. 등록 환경마다 서로 다른 실제 collector `clusterName`, 별도 배포 stage(`deploymentEnvironment`)와 최대 관측 나이를 명시한다. 앱 namespace·서비스 allowlist·cluster·stage·시간 범위를 모두 바인딩한 ClickHouse 집계(count/lastSeen)만 조회한다. 기존 읽기 전용 aiobs client를 재사용하며 키·Secret 권한을 추가하지 않는다. 원문 로그·span 본문·프롬프트는 조회하지 않는다.
- logs/traces와 metrics를 원본·대상에 똑같이 검사한다. 표본 없음, 오래된 표본, 잘못된 환경, 조회 오류, 미설정은 unknown이다. Prometheus는 서버 설정의 environment/cluster label을 쿼리와 반환 시계열 양쪽에서 확인한다. 전역 `up`이나 과거 샘플은 통과 근거가 아니다.
- 기본 `mode: audit`와 기존 capability 판정은 유지한다. 앱별 `enforce: true`를 명시한 경우에만 원본·대상의 requiredSignals가 모두 최신 수신으로 증명되어야 새 이전을 시작한다. 설정 digest·현재 원본 baseline·보고서 유효기간도 확인한다. 미검증이면 HTTP 409 `observability_unverified`, 승인·실행 바인딩된 감사 기록을 남기고 예약을 해제한다. 기존 rollback 예외는 보존한다. stateless 앱은 검증된 정확한 `/apps/{name}` 경로를 서버 설정 namespace에 매핑한다.
- 경보 전달 등 미구현 항목은 계속 unknown이다. requiredSignals의 좁은 통과를 전체 보안 동등성 통과로 표시하지 않는다. 조용한 대상은 수신 증거가 없으면 차단되므로, enforce 활성화 전 운영자가 대상의 계측·검증 트래픽과 원본 baseline을 준비해야 한다. 이 코드가 운영 트래픽을 생성하거나 collector를 설치하지 않는다.
- 배포 stage `dev`와 물리 환경 `gke/eks/onprem`을 구분한다. AI 출처는 새 `hopcharlie.environment.name` 속성을 우선 읽고 기존 deployment 속성을 호환 fallback으로 남긴다. collector에서 환경 속성을 추가하는 인프라 변경은 별도 검토한다.

## 대안과 영향

전체 parity enforce 전환은 미구현 경보·복구 등의 unknown으로 모든 이전을 차단하므로 제외했다. 수집기 Pod Ready만으로 통과시키거나 전역 로그를 사용하는 방법도 전달·환경 구분을 증명하지 못한다. 추가 신호 조회는 고정 원본·대상 2개에 한정하고 기존 전체 30초 deadline을 공유한다. Prometheus 및 ClickHouse의 신선한 앱별 표본은 수신만 증명하며 손실 없는 전체 전달, 계측되지 않은 AI 호출, 경보 전달을 증명하지 않는다.

## 검증과 운영 후속

최신 수신·0건·stale·미래 시간·부분 결과·중복·조회 실패·다른 정책/환경/만료 baseline, metrics label 불일치, stateless 앱 scope, 409 감사·재시도 예약 해제를 회귀 테스트한다. 실제 온프레미스 OTel 수신 및 enforce 설정은 미적용이다. Secret/CA·tailnet 경로·수집기 권한을 포함한 온프레미스 설치는 인프라의 구체적인 렌더 결과를 검토하고 별도로 승인해야 한다. 운영 최신 MinIO 이미지 배포와 Langfuse 프로젝트 연결도 별도 작업이다.
