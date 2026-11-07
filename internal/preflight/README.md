# Preflight 코어

**레지던시는 서버에 `PLATFORM_RESIDENCY_POLICY_FILE`이 있을 때 새 run 시작(`plans.Service.Reserve`)을 막는다. 파일 이전은 시작 전 실제 본문을 검사하고, 정상 run의 매 Sync/Check(재개 포함)에서 다시 검사한 staging만 전송·검증한다. 역방향 롤백 run은 기존 복구 면제를 유지한다. 파일 없는 재개의 전체 DB 게이트는 여전히 별도 후속 작업이다. 동등성은 `PLATFORM_PARITY_FILE`이 있을 때 시작 직전에 실행해 감사 로그에 기록만 한다(막지 않음).**

코어(`preflight`, `residency`, `parity`, `probes`)는 엔진·plans·runs·store에 의존하지 않는다. `inventory`는 원본 DB의 시스템 카탈로그만 읽는 수집기이고, `gate`는 엔진 Plan을 레지던시 요청으로 바꾸는 접착 코드이고, `paritygate`는 Plan의 대상에 맞춰 RBAC·Prometheus·선택적 리소스 프로브를 만들어 parity 보고서를 만드는 접착 코드다([ADR 0022](../../docs/adr/0022-parity-audit-mode.md)). 설계 근거는 [ADR 0011](../../docs/adr/0011-preflight-core.md)(코어)과 [ADR 0013](../../docs/adr/0013-residency-start-gate.md)(시작 게이트)에 있다.

## 시작 게이트 흐름

1. 운영자가 `POST /v1/migrations` 또는 롤백을 요청한다.
2. `Reserve`가 Plan을 다시 해석하고 비밀번호를 확장한다.
3. 롤백 Plan이면 검사하지 않고 넘어간다(`rollback_exempt`).
4. `inventory.Collector`가 확장된 `source.dsn`으로 모든 DB를 순회해 테이블·파티션·구체화 뷰 목록을 만든다. 목록이 불완전하면 거부한다(`inventory_incomplete`, `inventory:<code>`).
5. `files`가 있으면 원본·대상 버킷을 `bucketprobe`로 측정한다(복사에 쓸 rclone 원격 그대로). 위치·엔드포인트 IP·원본 1단계 프리픽스·대상 복제 대상·tier를 Rego가 정책의 `expect`, `prefixes`와 대조한다. 정책에 없는 버킷은 접속하지 않고 `unregistered_bucket`, 측정 실패는 `bucket_probe_incomplete`다. 규칙 전체는 [ADR 0021](../../docs/adr/0021-bucket-residency-measured.md).
6. 내장 Rego가 catalog와 대조한다. 허용되는 파일 이전은 KR 내 `inspectionEnvironment` 선언을 확인한 뒤, 원본 전체를 제한된 private staging으로 다운로드하고 UTF-8 본문/지원 ZIP 내부를 검사한다. 내용 분류를 기존 label에 추가해 다시 평가한다. 미지원/실패/상한 초과는 거부한다. 이후 엔진의 모든 정상 Sync/Check에서도 재측정·재검사하며 검사한 staging만 업로드한다([ADR 0031](../../docs/adr/0031-file-content-residency.md)).
7. 거부되면 Plan은 `approved`로 돌아가고 409 `residency_denied`를 반환한다. 사유와 policy digest는 감사 로그에 남는다.
8. 레지던시를 통과하고 manifest를 파일로 쓴 뒤, `pgversion`이 원본과 대상의 PostgreSQL 메이저 버전을 비교한다(기본 켜짐, `PLATFORM_VERSION_GATE=off`로 끔). 원본은 DB의 `server_version_num`, 대상은 접속되면 DB, 아니면 manifest의 `spec.imageCatalogRef.major`나 `spec.imageName` 태그에서 읽는다. 명백히 다르면 409 `version_mismatch`이고, 읽지 못하면 `unknown`으로 기록하고 막지 않는다.
9. 그다음 `PLATFORM_CAPACITY_GATE=on`이면 `capacity`가 원본·대상을 5초 동안 측정한다. 측정해서 명백히 실패하면 409 `capacity_insufficient`를 반환한다. 측정값은 통과·차단 모두 감사 로그에 남는다.

## 구현 범위

| 부분 | 구현 | 아직 하지 않는 것 |
|---|---|---|
| `residency` | 실제 내장 OPA/Rego, catalog/환경 설정 검증, 전체 클러스터 국가 제약 판정, 정책 digest | 실행 Plan 해시, 결과 저장, 승인 화면 미리보기 |
| `inventory` | 실제 PostgreSQL 카탈로그 조회로 모든 DB의 테이블·파티션·구체화 뷰 수집, 접속 불가 DB·대용량 객체·미지원 식별자·timeout은 불완전 | DSN이 그 CNPG 클러스터를 가리키는지의 독립 검증, 이미 존재하는 replica 감지 |
| `capacity` | `PLATFORM_CAPACITY_GATE=on`일 때 시작 전 실측. 대상 접속 한도 부족, replica가 `catchupTimeout` 안에 따라잡지 못함을 차단. 측정 못 한 값은 unknown이며 차단 안 함 ([ADR 0014](../../docs/adr/0014-capacity-preflight.md)) | 7일 피크 접속 수, 대상이 없을 때 회선 처리량, 승인 화면 미리보기 |
| `pgversion` | 시작 전 원본·대상 PostgreSQL 메이저 버전 비교. 다르면 차단, 읽지 못하면 unknown이며 차단 안 함 ([ADR 0020](../../docs/adr/0020-pg-major-version-gate.md)) | 대상 manifest에 이미지가 없을 때 CNPG operator 기본 이미지 조회, 확장 버전 비교 |
| `gate` | Plan → 물리 복제 + 버킷 복사 요청 변환, 시작 및 정상 파일 Sync/Check 경로 연결, 버킷/내용 검사 증거 첨부 | DB 재개·역방향 롤백의 공통 게이트 |
| `fileguard` | 실제 전체 본문/SHA-256 검사, UTF-8/제한된 ZIP, 수동 exact-hash 분류, 매 패스 staging 고정 전송·다운로드 비교. 단위·실제 로컬 rclone·관련 race 검증 통과 | OCR/범용 DLP, 버전별 검사 캐시, 메타데이터 의미 보존, 운영 성능 검증 |
| `paritygate` | `PLATFORM_PARITY_FILE`로 시작 직전 RBAC·Prometheus·선택적 리소스 실검사, 감사 로그 `parity` 기록. 결과와 무관하게 시작 | 차단 모드, 원본 dependency 발견, 승인 화면 표시 |
| `bucketprobe` | rclone 원격 설정으로 S3·MinIO·GCS 버킷 위치, 엔드포인트 IP, 1단계 프리픽스, 복제 대상, MinIO tier 측정. 프리픽스 객체 요약(키·값 없이 개수만) | MinIO site replication, GCS Storage Transfer 작업, 2단계 이하 프리픽스 |
| `parity` | 원본 요구 + 조직 필수 capability의 합집합, 5종 결과와 필수 검사 fail-closed. 시작 경로에는 기록 전용으로 연결(`paritygate`) | 실제 원본 dependency 자동 발견, UI 표시 |
| `probes.RBAC` | 실제 Kubernetes client로 앱 SA의 허용/금지 SubjectAccessReview | 클라우드 IAM 권한, 리소스 동작 자체의 성공 |
| `probes.Prometheus` | 실제 HTTP 조회로 앱 job의 모든 반환 타깃 `up==1`, `timestamp(up)` 기반 최근 scrape 확인 | 알림 규칙 발화·Alertmanager 도착 검증 |
| `probes.Secrets`, `AuditCollector`, `Backup` | Secret 메타데이터 존재, 감사 수집기 Deployment 가용성, CNPG Barman WAL 상태·최근 백업 확인([ADR 0029](../../docs/adr/0029-parity-resource-evidence.md), [3환경 설정](../../examples/parity-three-environments.yaml)) | 시크릿 실제 사용·감사 이벤트 전달·실제 복구는 `unknown`. 클라우드 관리형 감사, DaemonSet 수집기, Barman 이외 백업 미지원 |

Prometheus의 scrape 검사가 성공해도 **`metrics-alerting` capability는 `unknown`**이다. 아직 알림 경로를 검증하지 않았으므로 전체 기능을 통과시켜서는 안 된다. 미구현 검사를 성공시키는 stub이나 고정 데모 응답은 없다. 테스트의 합성 정책·HTTP 응답은 테스트 파일에만 있다.

Secret 실제 사용은 환경별 `secretUsage`를 명시하고 지원하는 앱 adapter를 배포한 경우에만
검사한다. 등록 클러스터의 같은 namespace service proxy로 challenge가 있는 읽기 전용 검사를
보내며, 메타데이터와 모든 필수 Secret에 연결된 작업이 통과해야 Secret capability가 통과한다.
지원 operation은 새 DB 연결의 `database-query`, 서명된 버킷 존재 조회의 `bucket-access`이다.
설정 없음·proxy 권한 부족·일반 readiness 200·옛 응답·미검사 Secret은 unknown이다.
모든 Secret 키, 객체 쓰기 권한, cloud IAM 최소 권한을 증명하지 않는다.
설정과 rollout 순서는 [ADR 0038](../../docs/adr/0038-verify-application-secret-use.md)을 따른다.

환경별 `cloudIAM` 설정은 AWS identity 정책 시뮬레이션과 GCP Policy Troubleshooter의
실제 정책 조회를 RBAC 결과에 결합한다. 허용·금지 permission/resource를 모두 지정하고,
검사 불가·조건 미평가·일부 정책만 검사한 경우는 unknown이다. AWS 시뮬레이션만으로 전체
유효 권한을 통과시키지 않는다. 검사 대상 principal의 workload 연결과 검사자 권한은 운영
준비가 필요하며 코드가 권한을 부여하지 않는다. 등록된 AWS/GCP 환경에 설정이 없으면
`cloud_policy_not_configured`로 unknown을 남겨 RBAC만으로 통과시키지 않는다.
[ADR 0039](../../docs/adr/0039-cloud-policy-parity.md)와
[설정 예시](../../examples/parity-cloud-iam.yaml)를 참고한다.

## 레지던시: 신뢰 경계

`Config`는 운영자가 리뷰한 서버 정책이다. 환경 ID와 국가, 정확한 클러스터 scope의 dataset 분류, APAC 국가 집합을 가진다. 환경/namespace/CNPG cluster와 database/schema/table을 각각 구분한다. 단순 문자열 `schema.table`을 연결해 식별하지 않는다.

- `ParseConfig`는 알 수 없는 필드·중복 키·지원하지 않는 설정을 거부한다. JSON도 YAML의 부분집합으로 받는다.
- schema version은 `1`. 분류는 `kr-only`, `apac`, `global`. 국가 코드는 정규 ISO 3166-1 alpha-2 대문자이며, 미상 코드·macroregion·별칭은 허용하지 않는다.
- APAC 목록은 조직이 명시한다. 코드가 국가 목록을 추측하지 않는다.
- `global`도 미등록/국가 미상 목적지를 허용하지 않는다.
- 평가기 생성 시 설정을 깊은 복사하고 정렬한다. 호출자의 map/slice 변경으로 정책이 바뀌지 않는다.
- SHA-256 digest는 내장 Rego와 정규화한 **전체 설정**을 묶는다. 목록 순서는 의미 변경이 아니다. 이 digest는 Plan·매니페스트·승인 기록을 묶는 실행 digest가 아니다.
- Rego는 바이너리에 포함되며 외부 정책 업로드는 없다. 평가에서 HTTP/DNS/runtime 정보 builtin은 금지한다.

`preflight.Request`와 `Inventory`는 **신뢰된 서버 코드가 실제 리소스를 해석한 뒤 구성**해야 한다. API 본문이나 AI 출력에서 그대로 받아서는 안 된다. 내부 `files.contentLabels`도 서버 scanner가 검사한 staging에서 계산하는 추가 제약이며, 공개 Plan 입력이 아니다. 클라이언트가 label/country/verify.tables를 반출 허가 증거로 제출하는 경로는 없다.

`Complete=true`는 실제 탐색의 증명이 아니다. 생산 collector가 전체 scope와 모든 DB/스키마의 이동 데이터를 조사하고 완전성을 보장해야 한다. inventory와 catalog 양쪽에 모두 같은 데이터셋이 있어야 허용하며, 새 데이터셋·cataloged 데이터 누락은 거부한다. catalog와 inventory가 동시에 놓친 데이터는 이 코어가 찾아낼 수 없다. 현재 `inventory.Collector`는 실제로 구현되어 Check마다 수집하지만, 승인 시점부터의 freshness/환경 fingerprint를 묶은 실행 증명은 아니다.

### 파일 내용 검사: 활성화와 한계

- `inspectionEnvironment`에 API의 private TEMP 저장소가 실제 위치한 **등록된 KR 환경**을 선언한다. KR은 `apacCountries`에도 포함되어야 한다. 이 필드가 없으면 파일 이전만 `inspection_location_unverified`로 막힌다. 선언 자체가 독립적인 지리 위치 실측은 아니다.
- `files.flags`는 guarded 경로에서 비워야 한다. 실행 파일은 서버의 `rclone`으로 고정하고 rclone config 파일/전역 filter·dry-run 환경 옵션은 사용하지 않는다.
- 기본 검사 대상은 UTF-8 원문 및 제한된 ZIP32 멤버다. 주민번호/휴대전화/이메일/카드 후보를 발견하면 `kr-only`를 추가한다. 패턴 미발견이 개인정보 부재나 법적 반출 허가를 의미하지 않는다. 운영자의 기존 bucket/prefix label은 항상 유지한다.
- 일반 이미지·PDF·바이너리 등 미지원 바이트는 기본 거부한다. 해당 원본 bucket의 `reviewedFiles: [{sha256: <실제로 검토한 64자리 소문자 SHA-256>, label: kr-only|apac|global}]`로 **그 바이트만** 분류할 수 있다. 수동 검토는 지원 텍스트의 탐지 결과나 archive 안전성 검사를 무시하지 못한다.
- 최대 10,000 객체, 객체당 16 MiB, 원본 합계 256 MiB, ZIP decoded 합계 64 MiB/멤버 10,000개/중첩 3단계, 호출당 최대 10분이다. 초과 시 전체 거부하며 샘플 통과로 바꾸지 않는다. 큰 WAD/영상 등은 검토한 해시가 있어도 크기 제한에 걸릴 수 있다.
- 검사 원문과 개별 키/해시/일치 문자열은 기록하지 않는다. 감사 로그 `residencyContent`와 engine 파일 evidence에는 규칙 버전·개수·바이트·manifest digest·label·규칙별 발견 수만 담는다.
- 매 패스 전체 다운로드·재전송·대상 다운로드 비교가 필요하다. fence 지연/요청 비용이 증가할 수 있으며 운영 성능은 미검증이다. staging은 KR 내 private ephemeral volume에 두고 복제/백업/ACL/장애 후 잔여 파일을 통제해야 한다.
- 전송 계약은 현재 객체 키와 **본문 바이트**다. S3/GCS metadata·tags·버전·ACL·Content-Type 의미를 보존해야 하는 앱은 이 초기 staging 경로를 그대로 운영 적용하면 안 된다. 별도 보존 어댑터/검증이 필요하다.
- 정책이 없는 서버, 독립 CLI와 역방향 롤백 면제 경로까지 모두 보호하는 기능은 아니다. DB 행 내용, 난독화/의미 기반 PII, 원본 전체의 원자적 시점, metadata/DNS TOCTOU도 해결하지 않는다. 자세한 대안·보안 경계는 [ADR 0031](../../docs/adr/0031-file-content-residency.md).

### 물리 복제의 범위

현재 migration-engine은 CNPG 물리 복제와 승격을 사용한다. `verify.tables`는 비교 범위이지 복제 필터가 아니다. 이 패키지는 `physical-replication`만 지원하고, 클러스터 전체 분류를 함께 평가한다. 하나라도 대상 국가를 허용하지 않으면 전체 이전을 거부한다. 여러 경로 중 하나라도 불허면 전체 요청이 불허다.

- `kr-only`는 KR만 허용한다.
- `apac`은 구성한 국가 집합만 허용한다.
- `global`은 등록된 국가를 허용한다.

테이블 선택형 logical 이전, 백업/restore 목적지 해석은 지원하지 않는다. 그 요청을 물리 복제라고 바꿔서 검사에 통과시키면 안 된다. 제한된 v1 dataset 이름은 영문/숫자/underscore/dollar/hyphen의 단순 PostgreSQL 식별자만 지원한다. 따옴표 안의 점·공백 등 복잡한 식별자는 지원하지 않고 거부한다.

### 호출

```go
// approvedPolicyBytes와 trustedRequest는 서버 소유 입력이다.
// DSN/비밀값/행 데이터는 어느 입력에도 넣지 않는다.
cfg, err := residency.ParseConfig(approvedPolicyBytes)
if err != nil { return err }
evaluator, err := residency.New(ctx, cfg)
if err != nil { return err }
verdict, err := evaluator.Evaluate(ctx, trustedRequest)
if err != nil || !verdict.Allowed {
    // 차단 사유를 반환. nil error만 보고 허용하지 않는다.
}
// 여기까지는 residency만 검증했다. 엔진을 실행해도 된다는 뜻이 아니다.
```

평가 오류·취소·미초기화 평가기·빈/불완전 inventory·scope 불일치는 허용하지 않는다. 정상적인 정책 거부는 `Allowed=false, err=nil`이고, 평가/입력 오류는 `Allowed=false, err!=nil`이다. 원문 입력/SDK 오류는 판정에 노출하지 않는다. 유효한 리소스 식별자 자체도 보안 정보일 수 있으므로 이후 API 공개에는 별도 접근 제어가 필요하다.

## 동등성

`parity.Evaluate`의 필수 기능은 다음 합집합이다.

```
source.Capabilities ∪ request.Mandatory
```

원본이 보안을 빠뜨렸다는 이유로 대상도 빠뜨리게 하지 않으려면 조직 최소 기준을 `Mandatory`로 제공해야 한다. 이것 역시 서버 정책이며 AI가 고를 수 없다. live source discovery는 후속 작업이다. 발견 실패를 빈 capability 목록으로 바꾸지 말고 `Complete=false`로 전달한다.

- 5개 분류: `identity`, `secret`, `audit`, `metrics-alerting`, `backup-restore`.
- 결과: `pass`, `fail`, `unknown`, `not_applicable`.
- `not_applicable`은 완전한 baseline과 조직 요구에서 둘 다 요구하지 않을 때만 생성한다.
- 필수 probe가 없거나 오류·unknown·다른 target/capability 응답·증거 없는 성공을 반환하면 통과하지 않는다.
- 원본 발견이 불완전하면 probe를 실행하지 않고 전체를 unknown으로 둔다.
- 각 probe는 최대 30초 context를 받는다. 실제 구현은 그 안에서 더 짧은 HTTP/검사 timeout을 적용한다.
- source/target 방향별 분기를 두지 않는다. 앱 ID는 같아야 하며 환경 ID는 달라야 한다.

registry의 Probe는 신뢰된 코드다. 외부 사용자가 작성한 `Result`를 그대로 받는 API가 아니다. 결과의 시각은 수집 시각이며 최종 게이트의 TTL·환경 지문 검증을 대체하지 않는다.

프로브 결과는 이제 5분이 지나거나 5초보다 미래이면 전체 성공으로 사용하지 않는다. 앱의 서버 소유 설정에 `baselines.<source-env>: {revision, reviewedAt, expiresAt}`으로 원본 요구사항 검토를 명시하며 검토 기간은 최대 30일이다. baseline 누락·만료·원본 namespace 불일치는 부분 관측을 유지하되 전체 pass를 금지한다. 기존 audit-only와 복구 면제는 유지한다.

시작 감사 기록에는 Plan ref/revision/reviewedAt, 미확장 Plan 및 첨부 manifest digest, 서버 Policy/parity config digest, 증거 observedAt/expiresAt을 추가한다. 매 새 시작에서 다시 검사하며 이 기록을 다른 계획이나 앱의 실행 허가로 재사용하지 않는다. 실제 환경 전체 fingerprint나 승인 당시 대비 drift 증명은 아니다. 범위와 미지원 항목은 [ADR 0033](../../docs/adr/0033-parity-evidence-and-app-retries.md)에 있다.

### RBAC

`probes.NewRBAC(target, authorizationClient, serviceAccount, rules)`로 생성한다. caller가 client를 해당 환경의 kubeconfig에 묶어야 한다. probe는 target이 달라지면 거부한다.

양성(필요 동작 허용)과 음성(불필요 동작 거부) 규칙이 모두 있어야 한다. `system:serviceaccount:<namespace>:<SA>`와 실제 SA 그룹 3개를 명시해 `SubjectAccessReview`를 요청한다. 검사기 자신에게 SAR 생성 권한이 필요하다. SAR는 실제 get/delete 등의 작업을 실행하지 않는다. 인가 평가 오류·응답 모순·403/접속 실패를 권한 거부 성공으로 오인하지 않는다. provider reason/SDK 오류 원문은 결과에 포함하지 않는다.

이 검사는 Kubernetes RBAC에 한정된다. AWS Pod Identity로 S3를 읽는 앱이라면 별도의 실제 IAM 경로 검사가 필요하다.

### Prometheus

`probes.NewPrometheus(target, endpoint, job, httpClient, maxSampleAge)`로 생성한다. caller가 endpoint/client/job을 앱과 환경에 정확히 묶어야 한다. URL/인증 설정은 서버 소유이며 Plan에서 받지 않는다. 생성 시 HTTP client를 복사하고 redirect를 차단하며 요청 시간을 제한한다.

조회는 `up{namespace="...",job="..."}`와 `timestamp(up{...})` 두 가지다. 같은 평가 시각으로 조회하고 metric name을 제외한 전체 label set으로 타깃을 대응한다. 빈 결과·다른 namespace/job·누락/중복 series·오래되거나 미래인 sample·NaN/잘못된 응답·부분 결과 warning·대용량 body는 통과하지 않는다. 반환된 모든 series가 `1`이어야 한다.

정상 결과는 **해당 selector로 반환된 타깃**의 scrape 증거다. 서비스의 기대 replica 수와의 일치·사용자 요청 성공·알림 전달은 별도 검증이며, 이 검사로 증명하지 않는다. 앱에 `namespace` label이 없으면 임의로 넓은 query로 대체하지 말고 Prometheus 설정/수집 계약을 먼저 맞춘다.

## 테스트와 후속 통합

플랫폼 디렉터리에서:

```sh
go test -count=1 -cover ./internal/fileguard ./internal/preflight/... ./internal/runs
go test -race ./internal/fileguard ./internal/preflight/... ./internal/runs
go vet ./internal/fileguard ./internal/preflight/... ./internal/runs
go test ./...
```

테스트는 실제 Rego 평가와 httptest 서버를 통한 실제 Kubernetes/Prometheus HTTP client를 사용한다. 실제 클러스터/Prometheus/운영 데이터에 접속하지 않으므로 live 검증의 증거는 아니다.

2026-11-06 파일 본문 검사 변경은 Windows Go 1.26.7에서 전체 build/vet, 관련 targeted/race, LF 정규화 복사본의 전체 test를 통과했다. 공식 checksum을 확인한 portable rclone v1.70.3으로 로컬 실제 복사/검증/PII 거부 테스트를 실행했고 `fileguard` coverage는 81.3%였다. 작업 디렉터리의 CRLF apidoc 실패와 base commit에서도 재현한 배포 순서 간헐 실패는 별도 보존했다. MinIO-tagged 테스트는 컴파일만 확인했으며 운영 버킷 이전·무중단 성능 검증은 아니다. 명령/로그/미검증 항목은 workspace `migration-engine/docs/test-records/2026-11-06-file-content-inspection.md`를 참고한다.

후속 작업:
1. source capability 발견·전체 DB inventory·리소스 목적지 해석과 승인된 운영 catalog 준비.
2. 시크릿 사용/감사 전달/실제 복구/알림의 end-to-end 검증을 추가. 리소스 부분 검사는 ADR 0029로 구현했다. 데이터가 이동하는 복구 검사 앞에는 residency 검사 적용.
3. Plan+매니페스트 digest, 정책/환경 fingerprint, 증거 TTL 및 신뢰된 리허설 기록을 저장하고 승인에 연결.
4. 시작/재개/역방향 롤백/자동 롤백 경계를 공통 게이트로 보호. 이미 진행 중인 fence/승격의 복구 안전성을 훼손하지 않기.
5. 최초 replica 생성부터 정책을 적용. 기존 복제본이 있다면 이미 데이터가 이동했을 수 있으므로 신규 실행 차단만으로 완전한 반출 방지를 주장하지 않기.
