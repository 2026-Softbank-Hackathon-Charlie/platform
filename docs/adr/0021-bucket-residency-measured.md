# 0021. 버킷 레지던시는 선언이 아니라 측정으로 판정한다

- 상태: 제안
- 날짜: 2026-11-04

## 상황

레지던시 게이트([ADR 0013](0013-residency-start-gate.md))는 DB를 서버가 직접 조사한다. 원본의 모든 테이블을 시스템 카탈로그에서 읽고, 카탈로그와 한 줄이라도 다르면 거부한다. 그런데 Plan의 `files`(rclone 버킷 복사)는 문자열만 비교했다.

1. **위치는 선언뿐이었다.** 정책의 `{bucket: "s3:photos", environment: eks}`는 운영자가 적은 값이다. `s3:`이 실제로 어느 엔드포인트, 어느 리전을 가리키는지는 서버 환경 변수 `RCLONE_CONFIG_S3_*`가 정한다. 둘이 어긋나도(오타, 다른 리전, 다른 MinIO) 게이트는 통과했다. 온프렘 MinIO는 사설 주소라 클라우드 API로 물어볼 수도 없다.
2. **내용을 몰랐다.** 버킷 하나에 라벨 하나였다. `apac` 버킷에 누가 `kyc/`를 만들어 신분증 사진을 넣어도 알 수 없었다.
3. **목적지 이후를 보지 않았다.** 대상 버킷의 복제 규칙(S3 CRR, MinIO bucket replication)이나 MinIO ILM tier가 데이터를 다른 나라로 다시 옮겨도 몰랐다. GCS 멀티리전 버킷은 한 나라에 있지 않다.

## 결정

DB와 같은 원칙을 적용한다. **서버가 복사에 쓸 바로 그 rclone 원격으로 버킷을 측정하고, 정책은 측정값으로만 판정한다.**

### 측정 (`internal/preflight/bucketprobe`)

- rclone과 같은 환경 변수(`RCLONE_CONFIG_<원격>_*`, 없으면 `RCLONE_S3_*`, `RCLONE_GCS_*`)에서 원격 종류, 엔드포인트, 자격 증명을 읽는다. 원격의 `TYPE`이 환경에 없으면 측정하지 않고 거부한다. rclone은 설정 파일에서 찾을 수 있지만 probe는 그 파일을 보지 않으므로 추측하지 않는다.
- **hosting**: 클라우드가 위치를 보증하는 경우(엔드포인트를 비운 AWS S3, `*.amazonaws.com`, GCS)는 `provider`다. 그 밖(MinIO, `provider = AWS`인데 다른 엔드포인트 등)은 `self`다. self-hosted 저장소가 알려 주는 region은 그 서버의 설정값일 뿐 위치의 증거가 아니다.
- 위치(S3 `GetBucketLocation`, GCS `location`/`locationType`), 엔드포인트 호스트가 가리키는 IP, 원본의 1단계 프리픽스, 대상의 활성 복제 대상 버킷, self-hosted의 lifecycle 전환(tier) 여부를 읽는다. 메타데이터만 읽고 객체 내용은 읽지 않는다.
- 실패는 고정 코드로만 알린다(`bucket_probe_incomplete`, `bucket_probe:<code>`). 엔드포인트, 자격 증명, 저장소 오류 문구는 결과에 넣지 않는다.
- 정책에 등록되지 않은 버킷에는 접속하지 않는다.

### 판정 (Rego)

| 코드 | 조건 |
|---|---|
| `bucket_location_unverified` | 확인 방법이 없다. provider인데 `expect.locations`가 없거나, self인데 `expect.endpointCIDRs`가 없다 |
| `bucket_location_mismatch` | 측정한 위치가 `expect.locations`에 없다 |
| `bucket_multi_region` | provider 버킷이 dual-/multi-region이다 |
| `bucket_endpoint_outside_network` | 엔드포인트 IP 중 하나라도 `expect.endpointCIDRs` 밖이다 |
| `bucket_tiering_not_allowed` | self-hosted 버킷의 lifecycle이 객체를 다른 tier로 옮긴다 |
| `unclassified_prefix`, `prefix_catalog_mismatch` | 프리픽스 카탈로그가 있을 때, 목록에 새 프리픽스가 있거나 카탈로그의 프리픽스가 없다. DB의 `unclassified_dataset`, `inventory_catalog_mismatch`와 같다 |
| `prefix_country_not_allowed` | 그 프리픽스의 라벨이 대상 국가를 허용하지 않는다 |
| `bucket_replication_unregistered`, `bucket_replica_country_not_allowed` | 대상 버킷이 복제하는 버킷이 정책에 없거나, 원본이 가져가는 라벨이 그 나라를 허용하지 않는다. 복제 규칙은 원격이 아니라 버킷 이름을 적으므로 같은 이름의 등록 버킷이 여럿이면 모두 허용해야 통과한다 |

`expect`가 없는 버킷은 거부한다. 선언만 있는 상태를 통과시키지 않는다. 그래서 **기존 정책 파일은 버킷마다 `expect`를 추가해야 파일 복사가 다시 통과한다.** 서버 시작은 막지 않는다.

### 분류 돕기 (AI는 제안만)

`POST /v1/residency/buckets/inspect`(agent, operator)는 등록된 버킷을 측정해 정책 카탈로그와 나란히 보여 준다. 프리픽스를 주면 그 아래 첫 객체 50개를 다음 수치로만 요약한다.

- content type, 확장자, 사용자 메타데이터 **이름**의 개수
- 키 이름이 고정 패턴(이메일, 주민번호 형태, 휴대폰 번호, 카드 번호, 날짜, UUID, 비ASCII)에 맞는 개수

키, 메타데이터 값, 내용은 응답에 없다. AI 에이전트(ai-sandbox)는 이 요약으로 미분류 프리픽스의 라벨을 제안한다. 판정에는 쓰지 않는다. 사람이 검토해 정책 파일을 고치고 다시 배포해야 정책이 바뀐다.

## 결과

- 온프렘 MinIO의 원격 설정이 클라우드 주소로 바뀌면 시작 전에 막힌다.
- 버킷 안에 새 프리픽스가 생기면, 새 테이블이 생겼을 때처럼 분류할 때까지 막힌다. 안전한 쪽으로 실패하지만 운영자가 카탈로그를 고쳐야 한다.
- 감사 로그에 `residencyBuckets`(버킷별 hosting, 위치, 엔드포인트 IP, 프리픽스, 복제 대상, tier 여부)가 남는다.

## 알려진 한계

- probe가 이름을 해석한 뒤 rclone이 다시 해석한다. 그 사이 DNS가 바뀌면 다른 곳에 접속할 수 있다. `endpointCIDRs`는 운영자가 DNS를 통제하는 이름이나 IP 리터럴과 함께 써야 한다.
- CIDR이 증명하는 것은 "그 네트워크 안"이다. 그 네트워크가 어느 나라에 있는지는 운영자의 선언이다(예: Tailscale `100.64.0.0/10`은 tailnet 노드라는 뜻일 뿐이다).
- 설정 파일(`rclone.conf`)에만 있는 원격 옵션은 probe가 보지 못한다. 플랫폼 이미지에는 rclone 설정 파일을 두지 않는다.
- MinIO site replication(클러스터 단위)은 madmin 관리 API가 필요해 읽지 않는다. GCS에는 버킷 간 복제 기능이 없지만, Storage Transfer Service 작업은 보이지 않는다.
- 프리픽스는 1단계까지만 분류한다. 키 패턴 개수는 단서일 뿐이고, 개인정보가 없다는 증명이 아니다.
