# 0039. 대상 환경의 AWS·GCP 권한 정책을 동등성 검사에 연결한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

Kubernetes SubjectAccessReview만으로 앱의 클라우드 권한을 검사할 수 없다. 사용자가
환경 동등성 검증에 AWS·GCP 권한 정책 점검을 연결하도록 요청했다. 실제 업무 API를
실행하거나 IAM 정책·자격증명을 바꾸지 않는 조회 방식이 필요하다.

## 결정

운영자 소유의 환경별 cloudIAM 설정에 workload principal, permission/resource별
허용·금지 요구를 명시한다. 허용과 금지 검사를 모두 요구하고 최대 16개로 제한한다.
Plan·AI가 principal이나 endpoint를 선택하지 못하며 등록 환경의 provider와 설정을 대조한다.
기존 RBAC와 cloud 결과를 identity capability로 결합해 하나라도 실패하면 실패, 미확인이
있으면 unknown이다. 서버는 등록된 AWS/GCP 환경에 cloud 설정이 없으면
cloud_policy_not_configured를 추가해 RBAC만으로 identity가 통과하지 못하게 한다.
온프렘에는 cloud 요구를 자동 추가하지 않는다. 소스 baseline과 audit 모드 계약은 바꾸지 않는다.

AWS는 서버 자격증명으로 SimulatePrincipalPolicy를 호출해 principal의 현재 identity 정책과
boundary 등 시뮬레이션에 포함된 정책을 평가한다. action과 개별 resource 결과를 대조하고
pagination/누락된 조건/알 수 없는 결과는 unknown이다. resource/session 정책 등을
완전히 입력하지 않으므로 정상 비교에도 coverage=unknown을 추가해 전체 identity를
통과시키지 않는다. simulation mismatch는 설정된 identity-policy 기대와의 불일치이며
실제 API의 최종 접근 가능성을 확정하는 결과가 아니다. SDK 오류 원문은 감사 결과에 없다.

GCP는 Google ADC로 고정된 Policy Troubleshooter v3 endpoint에 accessTuple을 보낸다.
반환 tuple을 대조하고 overallAccessState의 CAN_ACCESS/CANNOT_ACCESS만 비교한다.
allowPolicyExplanation만으로 판정하지 않으며 UNKNOWN_INFO/UNKNOWN_CONDITIONAL은
unknown이다. redirect를 따라가지 않고 응답을 1MiB, 각 요청을 8초로 제한한다.

검사 결과는 요구한 tuple 범위의 증거이지 principal의 모든 권한을 열거하거나 검토했다는
증거가 아니다. 실제 workload의 principal과 설정의 일치도 운영자가 검토해야 한다.
온프렘에 AWS/GCP principal을 자동 복사하거나 cloud API를 호출하지 않는다.

## 검토한 대안

- 정책 문자열에서 AdministratorAccess만 검색: 조건·상속·boundary와 유효 권한을 놓친다.
- testIamPermissions를 다른 서비스 계정 대신 호출: 검사자의 권한을 대상 앱의 권한으로 오인한다.
- 실제 CreateUser/권한 변경 API로 금지 검증: 운영 부작용을 만들므로 제외한다.

## 결과와 운영 준비

예시는 examples/parity-cloud-iam.yaml이다. 운영 설정에는 실제 workload principal과
resource를 확인한 뒤 넣는다. AWS 검사자에 승인된 principal 범위의 iam:SimulatePrincipalPolicy,
GCP 검사자에 해당 resource의 troubleshooting 및 관련 정책 조회 권한이 필요하다.
권한·API 미활성·조건 누락은 unknown이며 이번 PR에서 권한을 자동 부여하지 않는다.
HyperDX가 정책 엔진은 아니며 기존 parity 감사 기록이 결과의 저장 위치다.
실제 클라우드 호출·정책 변경·운영 배포는 단위/HTTP fixture 검사와 구분한다.

## API 근거

- https://docs.aws.amazon.com/IAM/latest/APIReference/API_SimulatePrincipalPolicy.html
- https://docs.cloud.google.com/policy-intelligence/docs/reference/policytroubleshooter/rest/v3/iam/troubleshoot
