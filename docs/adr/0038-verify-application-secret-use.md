# 0038. 앱의 실제 자격증명 사용을 읽기 전용 서비스 프로브로 검사한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

Secret 메타데이터 존재만으로 앱이 실제 자격증명으로 의존 서비스에 접근했는지 알 수 없다.
사용자가 실제 사용 검증 추가를 요청했다. 정상 이전의 데이터나 시크릿을 바꾸지 않는다.

## 결정

환경별 AppResources에 선택적인 secretUsage를 추가한다. 운영자가 검토한 service/port와
Secret ID별 operation만 설정할 수 있고 Plan이나 AI 출력으로 endpoint를 지정하지 못한다.
등록 환경의 Kubernetes 자격증명으로 같은 앱 namespace의 service proxy를 통해
GET /internal/secret-usage?challenge=<새 32바이트 nonce>를 호출한다. 경로는 고정이며
응답은 8KiB, 호출은 5초로 제한한다. services/proxy GET 권한은 선택한 앱 namespace와
서비스로 제한해 별도 검토·부여해야 한다. 기존 권한을 자동 확대하지 않는다.

계약 응답은 version=1, challenge 원문, checks 맵이다. operation은 database-query와
bucket-access 두 개이며 값은 pass/fail/unknown이다. 일반 readiness 200이나 예전 nonce,
없는 항목·비정상 응답·권한 부족은 통과가 아니다. 모든 필수 Secret에 검사 바인딩이 있고
메타데이터와 실제 작업이 모두 통과한 경우에만 Secret capability를 통과시킨다.
설정이 없으면 기존처럼 사용 여부는 unknown이다. operation과 Secret의 연결은 운영자가
앱 코드와 주입 구성을 확인한 정책이며 이 로직은 모든 Secret 키·최신 버전의 사용이나
불필요한 IAM 권한 부재를 증명하지 않는다.

예시(실제 앱의 서비스와 주입 관계 검토 후에만 사용):

```yaml
secrets:
  - {id: database, name: example-db-app}
  - {id: storage, name: app-s3}
secretUsage:
  service: example-web
  port: 8080
  checks:
    - {id: database, secretId: database, operation: database-query}
    - {id: storage, secretId: storage, operation: bucket-access}
```

```json
{"version":1,"challenge":"<요청 nonce>","checks":{"database-query":"pass","bucket-access":"pass"}}
```

Moimroom adapter는 새 DB 연결에서 nonce SELECT와 서명된 버킷 존재 조회를 수행한다.
DB 쓰기·파일 업로드/다운로드 권한·HOST_CODE 검증은 포함하지 않는다. 앱이 결과를 보고하는
구조이므로 침해된 앱의 정직성을 증명하지 않는다. 대상 서비스와 코드 배포 신뢰가 전제다.

## 검토한 대안

- Secret 값 직접 읽어 플랫폼에서 로그인: 앱의 실제 사용을 입증하지 못하고 비밀 접근이 늘어난다.
- readyz 200만 사용: 프로세스 생존이나 캐시된 연결과 구분할 수 없다.
- 매 이전마다 쓰기 왕복: 운영 상태 변경과 cleanup을 수반하므로 별도 시나리오로 둔다.

## 결과

검사 결과는 기존 parity 감사 기록에 포함되고 시작 차단 계약은 바꾸지 않는다.
HyperDX 전송·대시보드와 클라우드 정책 검사는 별도다. 앱 adapter 배포와 제한된 proxy 권한,
운영 secretUsage 설정을 모두 반영하기 전에는 활성화하지 않는다. 단위/HTTP 경계 시험과
임시 DB·MinIO의 양성·잘못된 자격증명 음성 시험은 운영 실행 증거와 구분한다.
