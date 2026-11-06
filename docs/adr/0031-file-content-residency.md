# ADR 0031: 파일 내용 검사와 검사한 바이트에 묶인 전송

- 날짜: 2026-11-06
- 상태: 로컬 구현·검증 완료 — Windows Go/LF 전체 테스트 및 관련 패키지 race 통과; 운영 이전은 미검증
- 기여: 클라우드 활용·완성도/보안 거버넌스, 데모 (a) 안전한 이전·(b) 정책 거부와 실제 검사 증거

## 문제

버킷 위치, 프리픽스와 운영자 분류를 확인해도 실제 객체 내용이 그 분류와 일치한다는 보장은 없다. `rooms/`를 APAC으로 선언한 채 한국 전용 정보가 포함된 파일을 넣으면 기존 게이트는 발견하지 못했다. 키/메타데이터 샘플링은 내용 검사가 아니다.

검사 후 원격 원본을 다시 복사하면 검사한 버전과 보낸 버전이 달라질 수도 있다. 최초 시작 검사만 추가하면 presync/delta/fence 또는 재개에서 바뀐 파일이 검사 없이 전송된다.

## 결정

`PLATFORM_RESIDENCY_POLICY_FILE`이 설정된 API의 정상 파일 이전 경로에 `internal/fileguard`를 설치한다. 공개 Plan 스키마나 엔진 모듈 버전은 변경하지 않는다. 기존 `engine.Options.Files` 주입점을 사용한다.

1. 기존 전체 DB inventory·버킷 실측·Rego 판정을 먼저 통과해야 한다. 미등록 버킷과 이미 불허인 이동은 파일 본문을 다운로드하지 않는다.
2. 정책의 `inspectionEnvironment`가 API 임시 저장소의 위치를 선언한다. 등록된 **KR 환경**이고 KR이 APAC 집합에도 포함되어야 한다. 없으면 `inspection_location_unverified`로 다운로드 전에 거부한다. 미분류 내용이 KR 전용일 가능성이 있기 때문이다.
3. `rclone lsjson`으로 전송 범위 전체의 객체를 열거한다. 수·크기·경로·중복을 검증한 후 객체마다 `rclone cat`으로 새 임시 디렉터리에 다운로드한다. 쓰기 크기는 목록의 크기로 제한하며 실제 크기가 달라지면 거부한다.
4. 모든 객체의 실제 바이트를 검사하고 SHA-256을 계산한다. 순서가 정규화된 `(키, 바이트 해시, 추가 분류)` 및 검사 규칙 버전으로 manifest digest를 만든다. **원본의 특정 시점에 대한 원자적 스냅샷은 아니며**, 이 패스에서 실제로 내려받은 바이트의 materialized snapshot이다.
5. 내용에서 계산한 분류를 Rego의 carried labels에 **추가**한다. 버킷/프리픽스의 기존 제약을 대체하지 않는다. 대상 및 기존 onward-replica 국가 제한 모두에 적용된다.
6. 새 run 시작 검사는 보고서만 남기고 staging을 제거한다. 실제 Sync는 매번 새 staging을 만들고, 현재 정책·실측을 다시 검사한 뒤 **그 staging만** 대상으로 전송한다. 검사한 파일 집합/해시가 변경되면 거부한다.
7. 업로드는 크기/mtime 단축 판정을 쓰지 않도록 `--ignore-times`를 사용한다. 이어 staging과 대상을 `rclone check --download --one-way`로 비교한다. 최종 엔진 Check도 새 원본 staging을 만들어 검사·비교한다.
8. 정상 시작/재개 모두 같은 어댑터를 주입한다. 어댑터 생성 결과가 nil이면 원래 rclone으로 fallback하지 않고 시작을 거부한다.
9. 기존 역방향 롤백 run의 레지던시 면제는 유지한다. 장애 복구를 새 내용 게이트로 막는 정책 변경은 별도 결정이다. 이 예외 때문에 모든 반출 경로가 보호된다고 주장하지 않는다.

## 검사 범위와 분류

- 확장자가 아니라 실제 바이트로 지원 형식을 판별한다.
- UTF-8 텍스트(허용 제어 문자는 줄바꿈·CR·탭)와 제한된 ZIP32의 내부 데이터를 검사한다.
- 주민번호 형태, 한국 휴대전화 형태, 이메일, 결제카드 후보 형태를 발견하면 보수적으로 `kr-only`를 추가한다. 체크섬/법적 개인정보 판단이 아닌 탐지 규칙이며 오탐이 가능하다.
- 객체 키, ZIP 멤버 이름과 텍스트 주석에서도 같은 패턴을 검사한다. 일치한 실제 문자열은 보고하지 않는다.
- ZIP은 single-disk·연속적인 Store/Deflate 표현만 허용한다. 경로 이탈, 링크, 암호화, 비어 있지 않은 extra metadata, ZIP64, 실행 파일 prefix, 숨긴 gap/trailer, CRC 오류, 초과 중첩/팽창은 거부한다. 디렉터리 엔트리의 숨은 payload도 거부한다. 이 제한으로 일반 ZIP 일부도 거부될 수 있다.
- 이미지/OCR, PDF, WAD, 일반 바이너리 등 미지원 형식은 기본 거부한다. 운영자가 원본 버킷의 `reviewedFiles`에 **정확한 SHA-256과 분류**를 기록한 바이트만 허용한다. ZIP의 미지원 바이너리 멤버도 해당 멤버 바이트의 해시 검토가 필요하다.
- 수동 검토는 지원 텍스트에서 발견한 제약을 지우지 못하며, 손상/암호화/범위 초과 archive를 우회시키지도 않는다. 기존 크기 제한도 유지한다.
- 지원 텍스트에 알려진 패턴이 없으면 추가 분류는 `global`이다. 이는 **개인정보가 없다는 증명도 아니고 법적 반출 승인도 아니다.** 기존 운영자 분류가 계속 필요하다. 난독화·Base64·이름만 있는 개인정보·새로운 패턴·스테가노그래피·의미 기반 민감정보는 탐지하지 않는다.
- DB 행 내용은 이 변경으로 검사하지 않는다. DB 쪽은 기존 전체 catalog/inventory 분류 방식이다.

## 운영 제한

| 항목 | 하드 상한 |
|---|---|
| 객체 수 | 10,000 |
| 객체 하나 | 16 MiB |
| 한 패스의 원본 바이트 | 256 MiB |
| rclone 목록 출력 | 8 MiB |
| ZIP 멤버 누적 수 | 10,000 |
| ZIP 누적 디코딩 바이트 | 64 MiB (중첩 단계의 바이트도 누적) |
| ZIP 중첩 | 3단계 |
| 한 검사/전송 호출 | context 최대 10분, 상위 context가 더 짧으면 그 제한 적용; 취소 후 subprocess pipe 정리에 최대 5초 추가 |

기존 Plan의 rclone flags는 guarded 경로에서 허용하지 않는다. `--size-only` 등을 포함해 명시된 flag가 하나라도 있으면 `file_guard_options_not_supported`다. 실행 파일은 서버 PATH의 `rclone`으로 고정하며, config 파일은 읽지 않는다. 전역 `RCLONE_*` filter/dry-run/link/files-from 옵션은 자식 환경에서 제외하고 `RCLONE_CONFIG_*`, `RCLONE_S3_*`, `RCLONE_GCS_*` remote/backend 설정만 남긴다. 다른 자격증명 환경 변수와 표준 provider 인증 경로는 기존과 같다.

**staging은 민감한 평문 데이터다.** API 계정 전용의 KR 내 TEMP 볼륨을 사용해야 한다. Unix는 디렉터리 0700/파일 0600이며 Windows는 서비스 계정 전용 TEMP ACL이 별도로 필요하다. `inspectionEnvironment`는 운영자 선언이지 호스트/볼륨 지리 위치의 독립 실측이 아니다. 볼륨 복제·백업·core dump·swap도 운영자가 통제해야 한다. 같은 계정/호스트의 악성 프로세스와 동시 변경은 이 경로의 신뢰 경계 밖이다.

정상 종료/실패에는 staging 제거를 시도하고 제거 실패를 고정 오류로 반환한다. 프로세스 강제 종료·호스트 장애에는 plaintext 잔여물이 남을 수 있다. private ephemeral volume과 운영 cleanup 절차가 필요하다. 메모리/임시 디스크/동시 요청의 전체 한도는 배포 수준에서도 제한해야 한다.

## 비용·호환성 트레이드오프

- 매 패스 전체 열거·다운로드·해시·전송·대상 다운로드 비교가 필요하다. 기존 직접 incremental sync보다 네트워크·요청 비용과 fence 시간이 크게 늘 수 있다. 빠르거나 저렴해졌다고 주장하지 않는다. 실측 없이 운영에 적용하지 않는다.
- 캐시는 없다. Bloom filter 양성만으로 복사/검사를 건너뛰지 않는다. 후속 최적화는 exact content-hash 결과와 source version/manifest를 묶은 뒤 검토한다.
- 전송 계약은 **현재 객체의 키와 본문 바이트**다. 로컬 staging 경유는 원본 S3/GCS의 사용자 metadata·tags·버전·ACL·storage class를 보존하는 이전 경로가 아니다. Content-Type/mtime 등은 local→remote rclone 기본 동작에 영향을 받는다. metadata/Content-Type 의미 보존이 필요한 앱은 이 초기 경로의 운영 적용 대상이 아니며 별도 어댑터와 검증이 필요하다.
- 목록 작성과 다운로드 중 원본은 변할 수 있다. 전송된 바이트는 검사한 staging에 묶지만 버킷 전체의 동시 시점을 증명하지 않는다. 최종 fence가 모든 객체 writer(백그라운드 작업·presigned upload 포함)를 멈추는지 별도 검증해야 한다.
- 버킷 DNS·정책/복제 설정과 실제 업로드 사이 TOCTOU, site replication/Storage Transfer 미탐지, 기존 대상 객체/과거 버전은 이 변경으로 해결하지 않는다.
- 정책이 꺼진 서버와 독립 CLI의 직접 엔진 실행에는 이 API 가드가 없다. 기존 경고와 opt-in 모델을 유지한다.

## 증거와 오류

시작 허용/거부 감사 로그에 `residencyContent`로 규칙 버전, 개수, 바이트, manifest digest, 추가 label, 규칙별 탐지 단위 수(본문/키/ZIP 멤버/주석; 고유 객체 수와 다름)를 기록한다. Sync/Check의 engine evidence에도 완료된 검사 보고서를 넣는다. 권한 거부 후 전송은 하지 않으며, 보고서를 만들기 전 실패하면 완전한 검사인 것처럼 보고하지 않는다.

원시 rclone stdout/stderr, 파일명 목록, 개별 파일 해시, 내용, 일치 문자열은 로그/API 오류에 넣지 않는다. SDK/서브프로세스 오류는 고정 코드로 바꾼다. manifest digest도 상관관계 정보이므로 기존 감사 로그 접근 제어를 유지한다. 성공 manifest는 실행 Plan 전체를 승인한 digest나 독립적인 법적 분류 증명이 아니다.

## 대안

- 원격 객체를 검사하고 원격에서 다시 복사: 적은 staging 사용량 대신 검사/복사 버전 불일치가 생겨 제외.
- 키·Content-Type·표본 검사만 추가: 실제 전체 본문을 검사하지 않아 제외.
- Bloom filter만 추가: 분류 기능이 없고 false positive가 복사 누락으로 이어질 수 있어 제외.
- 범용 DLP/OCR 서비스: 형식 지원은 넓지만 외부 서비스에 검사 데이터를 보내는 레지던시·비용·보안 경계가 추가된다. 이번 초기 구현에는 도입하지 않는다.

## 검증 상태

실제 scanner/ZIP parser와 staging/manifest의 단위 테스트, synthetic transport를 사용한 TOCTOU·거부·매 패스 재검사 테스트, 실제 Rego의 추가 제약/복제 대상 테스트, runner 시작/재개/롤백 어댑터 연결 테스트를 추가했다. 실제 로컬 rclone 통합 테스트도 포함하며 바이너리가 없으면 명시적으로 skip한다. 기존 `minio` build-tag 테스트는 실제 본문 읽기와 rclone이 필요하도록 갱신했다.

2026-11-06 Windows Go 1.26.7에서 변경 Go 파일 19개의 gofmt, `go build ./...`, `go vet ./...`, 관련 패키지의 `go test -count=1`과 `go test -race -count=1`을 통과했다. LF 정규화 복사본에서 `go test -count=1 ./...`도 통과했다. 이는 Windows 검증이며 Linux 실행 결과가 아니다. 검증 후에는 문서/기록만 갱신했다.

공식 SHA-256을 확인한 portable rclone v1.70.3으로 `TestLocalRcloneIntegration`을 실제 실행해 복사·다운로드 비교·PII 추가 후 거부/대상 기존 바이트 유지까지 통과했다(skip 아님). `internal/fileguard`의 `-cover -v` 결과는 81.3%다. MinIO-tagged 테스트는 `-run '^$'`로 컴파일만 통과했으며 실제 MinIO 접속은 하지 않았다.

최초 실행 전 도구 차단 기록과 이후 실패도 보존한다. 작업 디렉터리의 전체 테스트는 CRLF 때문에 `tools/apidoc.TestRenderRepoContract`에서 실패했지만 같은 코드의 LF 복사본은 통과했다. 첫 targeted 실행의 `TestListAndGetDeployments` 생성 시각 동률/순서 실패는 미변경 base commit의 200회 반복에서도 재현했다. 이번 변경에서 해당 테스트나 정렬 로직을 수정해 숨기지 않았다. 실제 MinIO/S3/GCS, 운영 API 전체 이전, 성능·staging 배치는 미검증이다. 상세 명령·실패·통과 로그는 workspace `migration-engine/docs/test-records/2026-11-06-file-content-inspection.md`에 남긴다.
