# SEC-09: OCSF 내보내기 확장과 콘솔 탭 — 설계

- 날짜: 2026-11-07 (해커톤 종료 2026-11-08)
- 상태: 승인된 설계 (브레인스토밍 2026-11-07)
- 관련: ADR 0034, SEC-07 (KEV·EPSS), SEC-08 (예외)
- 목적: 중간발표·설계 문서에 캡처할 수 있도록 HopCharlie 콘솔에서 OCSF 내보내기를 보인다.

## 결정

| 항목 | 결정 |
|---|---|
| API | 기존 `GET /v1/security/ocsf` 응답에 `vulnerabilities`, `controls` 배열을 더한다. 기존 `findings`는 그대로. |
| 취약점 | OCSF Vulnerability Finding(2002). 예외가 있으면 status Suppressed + status_detail 사유, 없으면 New. |
| KEV | OCSF 1.9.0에 KEV 전용 필드가 없다. KEV 등재(listed)일 때만 `is_exploit_available: true`, `exploit_ref_url`에 CISA KEV 카탈로그 URL. 미등재·미확인이면 쓰지 않는다. |
| EPSS | 점수가 있을 때만 `cve.epss`(score 문자열, percentile). 미확인이면 쓰지 않는다. |
| 통제 평가 | OCSF Compliance Finding(2003). 결과 pass→Pass(1), fail→Fail(3), unknown→Unknown(0). not_applicable은 내보내지 않는다. |
| 검증 | 공식 vulnerability_finding 스키마(cloud profile)를 testdata에 추가하고 모든 이벤트를 공식 스키마로 검사한다. |
| 화면 | 보안 화면에 `OCSF 내보내기` 탭: 버전·생성 시각, 종류별·상태별 건수, 이벤트 JSON 펼쳐 보기, JSON 내려받기. 데모 모드도 같은 화면. |
| 범위 밖 | 외부 전송(Security Lake, SIEM), 다른 OCSF 버전. |

## API 세부

- `severity` 필터는 지금처럼 `findings`에만 적용한다.
- 취약점·통제는 `/v1/security/vulnerabilities`, `/v1/security/controls`와 같은 데이터(KEV·EPSS·예외 포함)에서 만든다.
- 취약점·통제를 만들 수 없으면(스캔 저장소 없음 등) 그 배열은 비우고, 응답은 그대로 돌려준다.

## 테스트

- 플랫폼: 세 클래스 모든 이벤트가 공식 스키마 통과, 매핑(심각도, Suppressed, KEV/EPSS 있을 때만, 통제 상태 셋, not_applicable 제외), API가 세 배열을 돌려줌.
- 화면: 탭 표시, 건수, JSON 펼침, 내려받기, 데모 모드.
