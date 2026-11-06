# 0034. 보안 통제와 심각도 정책: 통제는 판정 기준, 심각도는 출처와 정책 버전을 남긴다

- 상태: 채택 (2026-11-06 팀 승인)
- 날짜: 2026-11-06
- 관련: [0027 스토리지 키를 보안 화면에](0027-app-storage-keys-in-security-view.md), 노션 SEC-01·SEC-02·SEC-04·SEC-05
- 번호: 처음 0029로 만들었으나 [0029 동등성 리소스 증거](0029-parity-resource-evidence.md)와 겹쳐 0034로 옮겼다(2026-11-06).

## 상황

`/v1/security`는 수집기마다 정한 심각도와 발견 항목만 보여 줬다. 그래서 세 가지를 구분할 수 없었다.

- 어떤 기준(NIST, CIS, AWS 통제)으로 판정했는지, 그 기준과 정확히 같은지 일부만 같은지.
- 심각도를 누가 정했는지. 벤더(Security Hub, GuardDuty)가 정한 값인지, 플랫폼이 정한 값인지.
- 발견 항목 0건이 "통과"인지, "읽지 못함"인지, "검사하지 않음"인지.

이미지 취약점은 스캐너가 없어 아예 보이지 않았다. 이 상태로 0건을 보여 주면 "취약점 없음"으로 읽힌다.

## 결정

1. **HopCharlie 통제 카탈로그를 버전과 함께 둔다.** `internal/security/controls.go`의 `PolicyVersion`(`hopcharlie-security-policy/2026-11-06.1`)이 카탈로그와 심각도 규칙의 버전이다. 통제마다 판정하는 check, 출처(source), 자산 단위, 조건, 실패 시 심각도, 한계를 적는다.
2. **외부 기준은 참조일 뿐 등급표가 아니다.** 참조마다 framework, 버전, URL, 관계(`exact`, `partial`)를 남긴다. 판정 조건이 AWS Security Hub CSPM 통제와 같을 때(`exact`)만 그 통제의 심각도를 쓰고 basis를 `vendor`로 둔다. 나머지는 basis가 `platform_policy`인 HopCharlie 규칙이다. NIST·CIS 참조는 심각도를 주지 않는다.
3. **결과는 pass, fail, unknown, not_applicable 넷이다.** 소스를 읽지 못했거나 권한이 없으면 unknown이다. 0건을 pass로 치지 않는다. not_applicable은 분모(`applicable`)에서 빠지고, unknown은 `evaluated`에 들지 않는다. 도메인 커버리지는 `evaluated`, `not_connected`(모두 unknown), `unsupported`(그 환경에서 검사하는 통제 없음) 셋이다.
4. **발견 항목은 원래 심각도와 정규화 심각도를 함께 가진다.** `severityBasis`에 출처, 원래 심각도·점수, 원래 finding id, 정책 버전을 남긴다. CVSS는 취약점에만 쓴다. 설정 점검에는 점수를 만들지 않는다.
5. **취약점은 저장된 스캔만 읽는다.** `/v1/security/vulnerabilities`와 HC-VUL-01은 스캔 결과 저장소(`ScanResults`)를 읽기만 한다. 조회 API 안에서 스캔하지 않는다. 스캐너가 없으면 `scanner.status: not_configured`이고, 실행 중인 컨테이너는 `not_scanned`이며 HC-VUL-01은 fail이다. 스캔 실패도 fail이다. 다이제스트를 모르는 컨테이너와 스캔 저장소를 읽지 못한 경우는 unknown이다.
6. **실행 자산은 다이제스트로 잇는다.** 컨테이너 상태의 `imageID`가 `repo@sha256:...`일 때만 레지스트리 다이제스트로 쓴다. 맨 `sha256:...`는 런타임의 로컬 이미지 id라 쓰지 않는다. 이미지(다이제스트), 영향받는 컨테이너, 어드바이저리, 발생 건수는 따로 센다.

## 심각도 변경

Security Hub CSPM 통제 문서(2026-11-06 확인)에 맞췄다.

| check | 이전 | 이후 | 근거 |
|---|---|---|---|
| `aws.user-mfa` | high | medium | IAM.5 Medium |
| `aws.sg-open-ingress` 전체 허용, 또는 EC2.19 차단 포트(20, 21, 22, 3306, 3389, 5432 등 24개)를 포함하는 TCP 범위 | high(전체, 22·3389·5432)·medium(그 밖의 포트) | critical | EC2.19 Critical |
| `aws.sg-open-ingress` 그 밖의 TCP(80, 443만 열린 규칙 제외) | medium(6443·9901은 high) | high | EC2.18 High(기본 허용 포트 80, 443) |
| `aws.sg-open-ingress` UDP | medium | medium, `platform_policy` | 대응하는 벤더 통제 없음 |
| `aws.sg-open-ingress` ICMP | medium | low, `platform_policy` | 대응하는 벤더 통제 없음 |

## 한계

- `aws.root-mfa`는 IAM.9와 일부만 같다. IAM.9는 루트 자격 증명이 없는 계정도 통과시키지만 이 점검은 AccountMFAEnabled만 읽는다.
- `aws.cloudtrail`은 CloudTrail.1과 일부만 같다. 로깅 중인 멀티 리전 트레일만 보고, 이벤트 선택기(관리 이벤트 읽기·쓰기)는 읽지 않는다.
- 앱·인스턴스 연결은 앱 차트 라벨(`app.kubernetes.io/part-of`, `app.kubernetes.io/instance`)에 기대며, 라벨이 없는 워크로드는 앱을 비워 둔다.
- 배포 기록의 다이제스트와 실행 중 다이제스트 비교는 아직 하지 않는다.
- 스캐너(SEC-04)는 아직 없다. 설치, 실행 위치, 레지스트리 읽기 권한, 스캔 저장 테이블은 별도 승인 후 정한다.

## 검토한 대안

- NIST·CIS를 위험 등급표로 쓰기: 이 기준들은 무엇을 점검할지 말할 뿐 심각도를 정하지 않는다. 임의로 등급을 붙이면 근거 없는 숫자가 된다.
- 설정 점검에 CVSS 같은 점수 붙이기: CVSS는 취약점의 속성을 점수로 만든 것이다. 설정 점검에 쓰면 비교할 수 없는 숫자가 섞인다.
- 조회 시 스캔하기: 이미지 하나에 수십 초가 걸리고 레지스트리 권한이 API에 필요해진다. 스캔은 요청 밖에서 돌리고 결과만 읽는다.

## 결과

- 새 API `GET /v1/security/controls`, `GET /v1/security/vulnerabilities`. 기존 `/v1/security`와 OCSF 내보내기는 그대로이며, finding에 선택 필드 `kind`, `controlId`, `severityBasis`가 붙는다.
- 0건과 미검사, 읽기 실패를 화면에서 구분할 수 있다.
- 심각도가 바뀐 finding은 id가 같고 심각도만 바뀐다. 기존 필터(`severity`)의 결과는 달라질 수 있다.

## 2026-11-06 이미지 스캔(SEC-04)과 배포 기록 비교(SEC-05)

- 스캐너는 Grype 하나다. 버전과 체크섬을 workflow(`.github/workflows/scan-images.yml`)에 고정한다. self-hosted runner에서 매시간 돈다. 실행 중 digest를 `GET /v1/security/vulnerabilities`로 읽고, `repository@digest`를 스캔한다. 결과는 `POST /v1/security/scans`(role agent, 전용 토큰)로 보낸다. API는 저장과 조회만 하고 스캔하지 않는다.
- API가 Grype JSON을 해석한다. 심각도는 advisory 출처(배포판, NVD, GitHub)가 준 값을 vendor 근거로 둔다. 값이 없으면 unknown이다. CVSS는 받은 그대로 나열할 뿐, 심각도로 바꾸지 않는다. 원본 보고서는 gzip으로 보관하고 sha256을 함께 남긴다.
- 저장은 digest마다 최신 스캔 하나다(`image_scans`). 더 늦게 끝난 스캔이 이미 있으면 받지 않는다(409). 7일보다 오래된 스캔은 세지 않으므로, 그 컨테이너는 미검사로 보인다.
- 끌어올 수 없는 digest는 failed로 저장한다. 0건으로 저장하지 않는다. 스캐너는 자기 리전의 ECR 자격과, GitHub OIDC로 받은 Artifact Registry 읽기 토큰(`hopcharlie-apps`, `charlie-apps` 저장소 reader, infra ADR 0065)을 가진다. GCP 변수가 없으면 Artifact Registry 이미지는 실패로 보인다.
- 멀티 아키텍처 index는 runner 플랫폼(linux/amd64)의 manifest를 스캔한다. 스캔의 digest는 실행 중 digest(index)로 두고, `message`에 실제 manifest를 남긴다.
- SEC-05: 실행 중 digest와 그 환경에 도달한 최신 배포 기록의 digest를 비교한다(`runningImages[].deployed`). 기록을 읽지 못하면 unknown이며, match로 보지 않는다.
