# 0007. 보안과 거버넌스 신호는 출처마다 수집기를 두고 하나의 형식으로 모은다

- 상태: 채택
- 날짜: 2026-11-02

## 상황

본선 데모 (b)는 여러 환경의 보안과 거버넌스 상태를 벤더별 기능(GuardDuty, Security Command Center 등)과 상관없이 하나의 화면으로 보여 달라고 요구한다. 데모 경로에는 가짜 데이터를 쓸 수 없다.

## 결정

- `GET /v1/security`는 출처별 수집기(collector)의 결과를 하나의 Finding 형식(환경, 출처, 분류, 심각도, 제목, 대상, 권고)으로 모은다. 새 벤더는 수집기 하나를 추가하면 된다.
- 지금 있는 수집기:
  - Kubernetes: 등록된 환경마다 특권 컨테이너, 호스트 네트워크와 경로, root 실행, 자원 제한 없음, NetworkPolicy 없는 namespace, cluster-admin 바인딩, 외부 노출 Service를 본다.
  - AWS 계정: 읽기 전용 호출로 MFA 없는 사용자, root MFA, 오래된 액세스 키, 열린 보안 그룹, 기본 EBS 암호화, CloudTrail, GuardDuty, Security Hub, S3 공개 차단을 본다.
  - 플랫폼: 감사 로그의 거부된 요청(예: AI 토큰의 승인 시도), DB 호스트 제한이 없는 설정.
- 읽지 못한 출처는 빈 결과가 아니라 상태(`unavailable`, `not_configured`, `error`)와 이유로 보여 준다. `not_configured`는 인스턴스 역할이 없다는 응답을 받았을 때만 쓰고, 시간 초과나 거부는 `error`로 보여 준다. SDK 오류 문구는 그대로 내보내지 않는다. GCP와 Cloudflare는 자격 증명이 생기면 수집기를 붙인다.
- Kubernetes 점검에서 빼는 namespace는 kube-system, kube-public, kube-node-lease 세 개뿐이다. GKE와 EKS의 애드온 namespace도 점검한다.
- 관리 호스트 역할에는 수집기가 실제로 부르는 읽기 API 17개만 허용하는 인라인 정책(`read-security-configuration`)을 붙인다. 관리형 `SecurityAudit`는 읽기 전용이지만 필요 없는 읽기 권한까지 넓게 준다.
- Finding id는 출처 안에서 대상을 구분하는 키(GuardDuty id, Security Hub ARN, 보안 그룹 규칙의 프로토콜과 포트와 CIDR 등)로 만든다. 같은 문제는 다시 수집해도 같은 id를 갖는다. 액세스 키 id는 해시로만 쓴다.

## 검토한 대안

- Security Hub 하나로 모으는 방식: AWS 바깥(GKE, 온프레미스, 플랫폼 자체)을 담을 수 없고, 켜는 데 비용과 시간이 든다. 대신 켜져 있으면 그 결과도 하나의 출처로 읽는다.
- 처음부터 OPA나 Kyverno 같은 정책 엔진을 클러스터에 까는 방식: 일주일 일정에서는 클러스터마다 설치와 동기화가 부담이다. 지금의 점검 목록을 나중에 정책으로 옮길 수 있다.

## 결과

- 처음 실제 계정을 점검했을 때 12건이 나왔다(root MFA 없음 1, MFA 없는 사용자 4, CloudTrail 없음, GuardDuty 꺼짐 등). 데모에서 그대로 보여 줄 수 있는 실제 신호다.
- 결과는 60초 캐시한다. 새로 고침은 10초에 한 번만 받는다.
