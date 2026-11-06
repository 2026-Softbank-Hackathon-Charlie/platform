# platform #91 구현 및 검증

## 결과

- moimroom: 실제 PostgreSQL 17에서 전체 API와 패키지 race 통과. 별도 앱 LOGIN 계정의 읽기/쓰기와 credential catalog 접근 거부, commit 뒤 실제 TCP 응답 유실, 동일 키 8개 동시 요청, 본문 충돌, 실패 영수증 rollback, 개인정보 비노출 검사 통과.
- moimroom: 같은 격리 DB 인스턴스 안에서 실제 pg_dump/pg_restore 후 독립 oracle의 원래 예약 결과 재전달과 예약/영수증 각각 1행 확인. 복원+앱 확인 3초이며 fixture 백업 뒤 추가 쓰기 0건. 운영 RPO/RTO가 아니다.
- platform: Windows 관련 plans/API/parity/paritygate 테스트, 전체 vet/build 통과.
- platform: Linux Go 1.26.5 CGO race 전체 통과(exit 0). Windows checkout의 텍스트를 격리 Linux 사본에서 LF로 정규화했으며 production 코드/검증 assertion은 바꾸지 않았다. 결과는 platform-linux-final-race.log 및 status.log.
- 실제 클라우드·운영 알림·배포·장애 주입은 수행하지 않았다.

## 실패와 재실행

Windows 전체 테스트는 gateway 소켓 WSAEFAULT와 CRLF API 문서 크기 검사로 실패했다. API 문서 실패는 수정 전 commit 8a23d38의 별도 worktree에서도 동일한 173409-byte chunk로 재현했다(platform-baseline-apidoc.log). Windows에서 한 차례 설정 파싱 실패가 있었고 단독 10회 및 관련 전체 재실행에서 재현되지 않았다. 실패도 보존하며 숨기지 않는다. Linux LF 사본에서는 전체 race와 해당 API/gateway/apidoc 테스트가 모두 통과했다.

첫 WSL race는 Windows 모듈 캐시 접근이 매우 느리고 WSL 서비스 timeout이 발생해 관련 WSL client만 중단했다. 이후 Docker 내부 모듈/빌드 캐시와 p=2로 제한한 별도 runner로 전체 race를 완료했다. platform-race.log는 중단된 시도이며 최종 증거가 아니다.

## 범위

source baseline은 서버 소유의 다섯 필수 capability와 앱별 구성에 대한 운영자 검토 선언이며 자동 발견이 아니다. config/Plan/manifest/Policy digest는 실제 환경 전체 fingerprint가 아니다. 신규 start마다 재검증하지만 승인 시 환경 drift의 완전한 attestation은 아니다. 현재 audit-only와 복구 면제는 유지한다.

cloud IAM·Kubernetes Secret의 실제 로드·감사 전달/장기 보존·실제 Alertmanager receiver·CNPG/Barman restore·gateway를 통한 moimroom 왕복 재시도·cross-environment restore residency·운영 RPO/RTO와 선택적 enforcement는 미지원/미검증이며 unknown을 pass로 바꾸지 않았다. #91 전체 완료 또는 RPO=0을 주장하지 않는다.

로컬 컨테이너와 DB backup은 검증 후 정리했다. existing hop-moimroom 클러스터는 유지했다. runner 스크립트와 원시 로그를 보존했다. 원본 결과는 workspace migration-engine/docs/test-records/2026-11-06-platform-91이며 리뷰용 사본은 platform/moimroom 저장소에 둔다. 문서 인코딩과 로그 공백만 정규화한다.
