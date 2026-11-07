# 0048. 온프렘 등록 코드를 한 번 발급하고 DB에서 원자적으로 소비한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

콘솔에는 온프렘 연결 화면이 있지만 실제 코드 발급 API가 없었다. 화면에서 코드를
만들거나 장비 자기보고만으로 ready를 표시하면 연결 증거가 아니다. 현재 환경은 정적
SSM 파일에서 읽으며 Argo 등록은 별도 권한이 필요하다. 이번 범위는 실제 코드 발급,
인증, 만료, 폐기이며 신규 클러스터 자동 등록은 포함하지 않는다.

## 결정

operator만 POST `/v1/onprem/enrollments`, GET `/{id}`, POST `/{id}/revoke`를 사용한다.
코드는 crypto/rand 32바이트(256bit), URL-safe 인코딩으로 만들고 수명은 15분이다.
DB에는 원문·설치 명령 없이 SHA-256만 저장한다. 같은 actor의 Idempotency-Key
재요청은 동일 등록 상태만 반환한다. 입력이 다른 재사용은 충돌로 거부한다.
원문을 다시 보여 주지 않으며, 새 코드가 필요하면 폐기 후 새 요청 키를 사용한다.

공개 claim은 JSON body의 코드, environment, installK3s를 함께 확인한다. DB의
조건부 UPDATE가 pending·코드 해시·기한을 검사하여 한 요청만 소비한다. revoke도
같은 행을 원자적으로 바꾸므로 늦은 claim이 폐기를 되돌리지 못한다. 최대 64개 활성
발급, 전역/IP별 claim 제한과 16개 동시 실행, 4KiB body, 본문 5초·처리 10초 제한을 둔다.
전달 헤더를 신뢰해 IP나 설치 서버를 정하지 않는다.

발급 응답만 코드·명령을 포함하고 Cache-Control은 no-store다. GET, 재시도, claim,
폐기는 코드와 명령을 반환하지 않는다. 감사에는 등록 ID·환경만 남긴다. 발급/claim의
감사 저장 실패 때는 코드를 반환하지 않고 등록을 폐기한다.

설치 origin은 `PLATFORM_ONPREM_PUBLIC_URL`(기본 `https://api.hopcharlie.dev`)의
검증된 HTTPS 값이며 요청 Host나 X-Forwarded-*로 만들지 않는다. 실제 embedded
`GET /v1/onprem/installer.sh`를 내려받아 전용 임시파일에서 실행한다.

스크립트는 Linux/WSL2에서 기존 k3s 바이너리를 확인한다. 없을 때 명시적인 installK3s
선택과 사용자가 직접 준 root 권한이 모두 있어야 설치한다. sudo를 자동 실행하지 않으며
기존 `/etc/rancher/k3s`나 서비스 설정을 발견하면 덮어쓰지 않는다. WSL2에는 systemd가
필요하다. 공식 upstream install.sh는 커밋과 SHA-256을 고정하고 k3s 버전은
`v1.36.5+k3s1`로 고정한다. 바이너리 다운로드 검사는 upstream 스크립트의 범위다.
[K3s 공식 문서](https://docs.k3s.io/quick-start)를 확인했다.

로컬 사전조건 검사 뒤 먼저 서버에서 코드를 claim한다. 서버가 같은 등록 ID와 claimed를
확인한 뒤에만 다운로드·해시 검사·설치를 한다. 만료·폐기·잘못된 코드로는 설치하지 않는다.
claim 후 다운로드나 설치가 실패하면 grant는 이미 소비됐으며 claimed/unknown으로 남는다.
자동으로 코드를 복구하거나 k3s를 제거하지 않는다. 필요하면 콘솔에서 폐기 후 재발급한다.

claim에는 kubeconfig나 장기 클러스터 자격증명을 보내지 않는다. 코드 JSON은 권한이
제한된 임시파일로 curl에 넘기며 URL·curl 인자에 넣지 않는다. 최초 설치 명령에는
짧은 수명의 코드가 들어가므로 장비의 터미널 기록에도 주의해야 한다.

## 검토한 대안

- 메모리만 사용: 재시작 때 발급·폐기가 사라져 배제했다.
- 원문을 idempotency 응답에 저장: hash-only 원칙을 깨므로 배제했다.
- 재시도마다 새 코드: 응답 유실이 여러 유효 코드를 만들어 배제했다.
- Host 헤더에서 설치 URL 생성: 명령 오염 위험 때문에 배제했다.
- 장비 자기보고로 ready: 관리 서버의 연결·배포 관측이 아니므로 배제했다.

## 결과

발급과 1회 인증은 실제 서버·DB 경로로 동작한다. claim 이후는 claimed이고 5개
검사는 unknown이다. 등록 작업자가 구성되지 않았다는 이유를 표시하며 ready 또는
passed를 만드는 경로는 없다. installer 성공도 클러스터 등록 완료를 뜻하지 않는다.

후속 등록 worker는 별도 최소 권한으로 SSM/Argo/환경 profile·네트워크를 구성하고,
정적 registry의 재시작 또는 안전한 갱신을 연결해야 한다. 서버가 대상 클러스터의 UID,
인증, 실제 image pull과 Hello World 응답을 관측한 뒤에만 ready를 허용해야 한다.
이번 PR은 SSM·Secret·IAM 변경, live installer 실행, 운영 배포를 수행하지 않는다.

검증은 HTTP 랜덤 발급·재시도·권한·만료·폐기·1회 소비, SQL 재연결 보존과 CAS 경합을
포함한다. installer의 문법·격리 검증은 실제 장비 설치나 신규 환경 등록의 증거가 아니다.
