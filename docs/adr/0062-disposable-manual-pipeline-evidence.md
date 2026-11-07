# 0062. 실제 이전 파이프라인은 수동 격리 runner에서 실행하고 구조화 증거만 보관한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

기존 CI는 실제 PostgreSQL 저장소와 kind 등록 경계를 검증하지만 `test/e2e/run.sh` 전체 이전을 호출하지 않는다. 이 스크립트는 API→Plan→프로세스 내부 엔진→CNPG 원본·대상 DB→HTTP 게이트웨이를 부하 중 실행한다. 실패 로그에는 임시 인증정보가 섞일 수 있다.

## 결정

`workflow_dispatch` 전용 GitHub hosted runner에서 main 이력의 정확한 SHA를 checkout하여 기존 네 시나리오를 모두 실행한다. lost-response, canary-fault, observe-rollback, migrate의 기존 판정을 그대로 유지하며 assertion이나 엔진 안전 경계를 완화하지 않는다. workflow 자체도 main에서만 실행한다. 권한은 contents 읽기와 기존 private module 읽기 토큰이다.

원문 stdout·stderr와 헤더는 외부 증거로 보관하지 않는다. 구조화된 시나리오 이름, 상태, 숫자, 종료 코드, UTC 시각, 원문 해시만 allowlist로 추출한다. 네 시나리오의 성공 marker와 최종 marker가 모두 있고 실제 종료 코드가 0일 때만 전체 성공이다. always 단계는 정확한 fixture kind 라벨과 저장소 이름·라벨을 사용하여 정리하고 남은 컨테이너 수를 기록한다.

증거 읽기는 최대 32 MiB로 제한한다. 초과하면 truncated·미검증으로 실패 처리하고 해시 범위를 앞부분으로 명시한다. 정리 단계는 checkout 전 실패에도 JSON을 남기고 Docker 조회 실패는 남은 자원 0으로 오인하지 않는다. 삭제에는 소유 라벨을 요구하며 최종 저장소 부재 검사는 라벨 없이 정확한 이름으로 수행한다.

## 검토한 대안

- 모든 PR에 전체 이전 CI 추가: 일상 CI 비용과 기존 필수 검사 범위를 불필요하게 늘린다.
- 원문 로그 artifact: 임시 DSN·토큰 노출 가능성이 있어 채택하지 않는다.
- 로컬 Docker나 운영 Kubernetes 재사용: 개발 환경 불안정성과 운영 영향 경계를 피하기 위해 사용하지 않는다.

## 결과

일회용 kind 내부의 실제 DB 이전과 실패 시나리오를 재현할 수 있다. 성공 여부는 해당 실행 결과로만 판단한다. 이것은 퍼블릭 클라우드, 클러스터 사이 네트워크, moimroom 앱, opt-in 게이트웨이 HA 또는 운영 적용 증거가 아니다. 기존 moimroom E2E는 별도 앱·차트·리허설 승인 호환성을 검토해야 한다. 실행 실패는 마지막 고정 단계와 원문 해시로 식별하며 원문 오류 문자열을 공개하지 않는다.

같은 수동 workflow의 독립 `minio-files` job은 정확히 같은 tested SHA에서 실제 MinIO 두 컨테이너와 rclone으로 fileguard 경계를 검증한다. loopback 임의 포트, 공개된 합성 fixture 자격, digest 고정 이미지, run별 정확한 컨테이너 이름·소유 라벨만 사용한다. 테스트가 기본 skip이면 성공이 아니다. 순방향·역방향 복사, 캐시 재사용, 동일 크기 원본 변경, 대상 변조 감지·복구, 객체 추가·삭제, 권한·메타데이터 재평가, 취소, 버킷 정리의 필수 열 가지 완료 marker와 최종 marker, 종료 코드 0을 모두 요구한다. 실제 rclone 버전을 기록하고 컨테이너 정리를 별도로 확인한다. 두 job이 통과해도 DB와 파일을 한 run에서 함께 이전했음을 증명하지 않는다.

수동 입력 `suites`는 `all`(기본), `migration`, `minio`만 허용한다. 테스트·workflow만 추가된 뒤 파일 검증을 실행할 때 이미 검증 중인 DB 이전 전체를 다시 실행할 필요가 없도록 job을 선택한다. 선택하지 않은 job은 미실행으로 기록하며 선택 실행을 두 job 전체 성공으로 표현하지 않는다.
