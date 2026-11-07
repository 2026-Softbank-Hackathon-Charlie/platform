# 온프레미스 설치와 실제 연결 완성

목표: 명령 한 번으로 코드 인증, k3s 설치(선택), 장비 전용 연결 서비스 설치, 실제 클러스터 등록, 서버 주도 5개 검사를 수행한다. 재시작 및 자격증명 갱신 후에도 연결을 유지한다. 기존 환경을 자동 교체하지 않는다.

## 연결 계약

- 기존 POST `/v1/onprem/enrollments/{id}/claim`은 등록 세션을 최초 1회 발급한다. 일회용 코드는 재사용하지 않는다.
- 세션은 등록 ID와 환경에 묶으며 토큰의 해시만 DB에 보관한다. 코드/연결 토큰/장비 접속 토큰은 로그·브라우저 저장소·감사 본문에 넣지 않는다.
- POST `/{id}/connection`은 세션 Bearer로만 허용한다. 요청은 clusterUID, CAData, 짧은 수명의 serviceAccountToken을 보낸다. API 주소나 임의 명령을 입력으로 받지 않는다.
- hostname은 `hop-k3s-<등록 ID의 enr_ 접두사를 뺀 값>`으로 서버가 정한다. 전용 태그의 device 소유권을 확인한 뒤 접속한다. CA와 실제 kube-system UID를 직접 확인하고 이후 UID 변경은 거부한다.
- 기존 이름은 덮어쓰지 않는다. 같은 등록 ID와 clusterUID에 한해 재개한다.
- 연결 서비스는 장비 내부의 제한된 SA TokenRequest를 주기적으로 갱신하고 같은 connection 경로로 갱신한다. 단기 토큰을 한 번 보낸 뒤 종료하지 않는다.
- 상태 조회는 기존 `{enrollment: ...}`를 유지한다. 검사 항목은 k3s/network/permissions/image_pull/hello_world이며 서버가 실제 관측한 결과와 시각만 저장한다.
- ready는 5개 검사가 모두 통과하고 Argo·환경 registry 등록이 완료된 경우에만 가능하다. 기본 연결 검사는 DB·MinIO·GPU 이전 동등성을 증명하지 않는다. 이 capability는 별도 검사·기존 거절 경로를 유지한다.

## 구현 순서와 소유권

1. store: 연결 세션 해시·만료·UID·검사 결과·단계·CAS·복구 자료를 영속화하고 revoke/등록 경쟁을 검사한다.
2. installer: 최초 bootstrap과 로컬 연결 서비스, 최소 권한 SA, 짧은 토큰 갱신, 비밀 파일 모드0600, 재실행과 실패 안내를 구현한다.
3. registrar: 전용 tailnet 발급·device 확인, 제한된 API egress, TLS/UID pin, Argo Secret 및 환경 설정의 CreateOnce/갱신, 실제 검사, 재시작 복구를 구현한다.
4. API/runtime: claim과 connection 인증, 설정 미완료의 발급 전 거절, 기존 환경 충돌, 안전한 동적 registry/소비자 연결을 구현한다.
5. infra/frontend: 등록 작업자 권한·전용 OAuth 설정·네트워크 정책과 실제 단계/검사·충돌 안내를 연결한다.
6. 원격 Go/실제 PG 검사, disposable k3s/kind 통합 시험, 독립 보안 검토를 완료한다. 운영 설정 변경 및 신규 OAuth/Secret 발급은 구체적 변경안을 준비한 다음 기존 승인 범위를 확인한다.

## 검증

실제 새 클러스터에서 claim→설치/연결 서비스→서버의 API/TLS/UID 확인→Argo 등록→env 목록 노출→이미지 다운로드와 HTTP 응답→ready를 시험한다. 다른 환경 덮어쓰기, 다른 등록의 토큰, UID 변경, 만료·폐기, 서버 재시작, 단기 접속 토큰 만료 뒤 갱신 실패는 반드시 실패/미확인으로 보인다. mock 결과로 운영 연결 성공을 주장하지 않는다.
