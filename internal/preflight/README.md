# Preflight 코어

**레지던시는 서버에 `PLATFORM_RESIDENCY_POLICY_FILE`이 있을 때 새 run 시작(`plans.Service.Reserve`)을 막는다. 재개는 검사하지 않고 롤백은 면제다. 동등성은 아직 어느 실행 경로에도 연결되지 않았다.**

코어(`preflight`, `residency`, `parity`, `probes`)는 엔진·plans·runs·store에 의존하지 않는다. `inventory`는 원본 DB의 시스템 카탈로그만 읽는 수집기이고, `gate`는 엔진 Plan을 레지던시 요청으로 바꾸는 접착 코드다. 설계 근거는 [ADR 0011](../../docs/adr/0011-preflight-core.md)(코어)과 [ADR 0013](../../docs/adr/0013-residency-start-gate.md)(시작 게이트)에 있다.

## 시작 게이트 흐름

1. 운영자가 `POST /v1/migrations` 또는 롤백을 요청한다.
2. `Reserve`가 Plan을 다시 해석하고 비밀번호를 확장한다.
3. 롤백 Plan이면 검사하지 않고 넘어간다(`rollback_exempt`).
4. `files`가 있으면 원본·대상 버킷을 함께 판정 요청에 넣는다. 정책의 `buckets`에 등록되지 않은 버킷은 `unregistered_bucket`, 원본 버킷 분류가 대상 버킷 국가를 허용하지 않으면 `bucket_country_not_allowed`다. 버킷 위치는 운영자가 선언한 값이고 클라우드에서 조회하지 않는다.
5. `inventory.Collector`가 확장된 `source.dsn`으로 모든 DB를 순회해 테이블·파티션·구체화 뷰 목록을 만든다. 목록이 불완전하면 거부한다(`inventory_incomplete`, `inventory:<code>`).
6. 내장 Rego가 catalog와 대조한다.
7. 거부되면 Plan은 `approved`로 돌아가고 409 `residency_denied`를 반환한다. 사유와 policy digest는 감사 로그에 남는다.

## 구현 범위

| 부분 | 구현 | 아직 하지 않는 것 |
|---|---|---|
| `residency` | 실제 내장 OPA/Rego, catalog/환경 설정 검증, 전체 클러스터 국가 제약 판정, 정책 digest | 실행 Plan 해시, 결과 저장, 승인 화면 미리보기 |
| `inventory` | 실제 PostgreSQL 카탈로그 조회로 모든 DB의 테이블·파티션·구체화 뷰 수집, 접속 불가 DB·대용량 객체·미지원 식별자·timeout은 불완전 | DSN이 그 CNPG 클러스터를 가리키는지의 독립 검증, 이미 존재하는 replica 감지 |
| `gate` | Plan → 물리 복제 + 버킷 복사 요청 변환, 시작 경로 연결 | 버킷 실제 리전 조회, 경로별 버킷 분류, parity 연결 |
| `parity` | 원본 요구 + 조직 필수 capability의 합집합, 5종 결과와 필수 검사 fail-closed | 실제 원본 dependency 자동 발견, 운영 API/UI 연결 |
| `probes.RBAC` | 실제 Kubernetes client로 앱 SA의 허용/금지 SubjectAccessReview | 클라우드 IAM 권한, 리소스 동작 자체의 성공 |
| `probes.Prometheus` | 실제 HTTP 조회로 앱 job의 모든 반환 타깃 `up==1`, `timestamp(up)` 기반 최근 scrape 확인 | 알림 규칙 발화·Alertmanager 도착 검증 |
| 감사·Secret·백업 복구 | 필수이면 미연결 상태를 `unknown`으로 보고 실패 처리 | 검사 요청 생성/로그 검색, 테스트 Job, 임시 CNPG 복구 |

Prometheus의 scrape 검사가 성공해도 **`metrics-alerting` capability는 `unknown`**이다. 아직 알림 경로를 검증하지 않았으므로 전체 기능을 통과시켜서는 안 된다. 미구현 검사를 성공시키는 stub이나 고정 데모 응답은 없다. 테스트의 합성 정책·HTTP 응답은 테스트 파일에만 있다.

## 레지던시: 신뢰 경계

`Config`는 운영자가 리뷰한 서버 정책이다. 환경 ID와 국가, 정확한 클러스터 scope의 dataset 분류, APAC 국가 집합을 가진다. 환경/namespace/CNPG cluster와 database/schema/table을 각각 구분한다. 단순 문자열 `schema.table`을 연결해 식별하지 않는다.

- `ParseConfig`는 알 수 없는 필드·중복 키·지원하지 않는 설정을 거부한다. JSON도 YAML의 부분집합으로 받는다.
- schema version은 `1`. 분류는 `kr-only`, `apac`, `global`. 국가 코드는 정규 ISO 3166-1 alpha-2 대문자이며, 미상 코드·macroregion·별칭은 허용하지 않는다.
- APAC 목록은 조직이 명시한다. 코드가 국가 목록을 추측하지 않는다.
- `global`도 미등록/국가 미상 목적지를 허용하지 않는다.
- 평가기 생성 시 설정을 깊은 복사하고 정렬한다. 호출자의 map/slice 변경으로 정책이 바뀌지 않는다.
- SHA-256 digest는 내장 Rego와 정규화한 **전체 설정**을 묶는다. 목록 순서는 의미 변경이 아니다. 이 digest는 Plan·매니페스트·승인 기록을 묶는 실행 digest가 아니다.
- Rego는 바이너리에 포함되며 외부 정책 업로드는 없다. 평가에서 HTTP/DNS/runtime 정보 builtin은 금지한다.

`preflight.Request`와 `Inventory`는 **신뢰된 서버 코드가 실제 리소스를 해석한 뒤 구성**해야 한다. API 본문이나 AI 출력에서 그대로 받아서는 안 된다. 입력 타입에 호출자가 주는 label/country/verify.tables는 없다.

`Complete=true`는 실제 탐색의 증명이 아니다. 생산 collector가 전체 scope와 모든 DB/스키마의 이동 데이터를 조사하고 완전성을 보장해야 한다. inventory와 catalog 양쪽에 모두 같은 데이터셋이 있어야 허용하며, 새 데이터셋·cataloged 데이터 누락은 거부한다. catalog와 inventory가 동시에 놓친 데이터는 이 코어가 찾아낼 수 없다. 현재는 collector도 freshness 검증도 구현하지 않았다.

### 물리 복제의 범위

현재 migration-engine은 CNPG 물리 복제와 승격을 사용한다. `verify.tables`는 비교 범위이지 복제 필터가 아니다. 이 패키지는 `physical-replication`만 지원하고, 클러스터 전체 분류를 함께 평가한다. 하나라도 대상 국가를 허용하지 않으면 전체 이전을 거부한다. 여러 경로 중 하나라도 불허면 전체 요청이 불허다.

- `kr-only`는 KR만 허용한다.
- `apac`은 구성한 국가 집합만 허용한다.
- `global`은 등록된 국가를 허용한다.

테이블 선택형 logical 이전, 백업/restore 목적지 해석, 오브젝트 버킷 위치 확인은 지원하지 않는다. 그 요청을 물리 복제라고 바꿔서 검사에 통과시키면 안 된다. 제한된 v1 dataset 이름은 영문/숫자/underscore/dollar/hyphen의 단순 PostgreSQL 식별자만 지원한다. 따옴표 안의 점·공백 등 복잡한 식별자는 지원하지 않고 거부한다.

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
go test -count=1 -cover ./internal/preflight/...
go test -race ./internal/preflight/...
go vet ./internal/preflight/...
go test ./...
```

테스트는 실제 Rego 평가와 httptest 서버를 통한 실제 Kubernetes/Prometheus HTTP client를 사용한다. 실제 클러스터/Prometheus/운영 데이터에 접속하지 않으므로 live 검증의 증거는 아니다.

후속 작업:
1. source capability 발견·전체 DB inventory·리소스 목적지 해석과 승인된 운영 catalog 준비.
2. 감사/Secret/백업 복구/알림 검사를 실제 구현. 데이터가 이동하는 복구 검사 앞에는 residency 검사 적용.
3. Plan+매니페스트 digest, 정책/환경 fingerprint, 증거 TTL 및 신뢰된 리허설 기록을 저장하고 승인에 연결.
4. 시작/재개/역방향 롤백/자동 롤백 경계를 공통 게이트로 보호. 이미 진행 중인 fence/승격의 복구 안전성을 훼손하지 않기.
5. 최초 replica 생성부터 정책을 적용. 기존 복제본이 있다면 이미 데이터가 이동했을 수 있으므로 신규 실행 차단만으로 완전한 반출 방지를 주장하지 않기.
