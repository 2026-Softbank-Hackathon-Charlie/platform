# 0045. MinIO 파일 원격을 지원하고 수동 이미지 변형과 운영 이미지 선택을 분리한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

`moimroom-mig`를 GKE에서 `onprem`으로 옮길 때 사진도 이전해야 한다. 기존 `EnableFiles`는 AWS와 GCS만 원격과 버킷 접두사에 등록하여 MinIO 환경의 사진 경로가 빠진다. 원격이 없으면 요청의 기본 파일 선택에서 사진을 제외할 수 있다(ADR 0032).

ADR 0016은 MinIO 관리 클라이언트의 AGPL-3.0 의존성을 `-tags minio` 빌드로 격리한다. 기본 API가 MinIO storage 설정을 거부하는 것은 의도한 보호다. 한편 main의 기본 이미지 발행은 infra main의 `images.platform`과 `helm/images.lock.json`을 함께 갱신하므로, 운영에서 MinIO 이미지를 선택한 뒤에도 기본 이미지가 덮어써 API가 시작하지 못할 위험이 있다.

## 결정

### 파일 원격

- `PLATFORM_APP_FILES=1`이면 MinIO도 `<환경>:<bucketPrefix><instance>`를 만든다. rclone S3 backend의 `provider=Minio`, `endpoint`, `region`, `force_path_style=true`, `env_auth=false`를 사용한다. AWS/GCS 원격의 인증 방식은 유지한다.
- `adminAccessKeyEnv`, `adminSecretKeyEnv`는 운영자 관리 `PLATFORM_ENVS_FILE`의 환경변수 이름이다. `EnableFiles`가 해당 값을 조회하고 `RCLONE_CONFIG_<ENV>_ACCESS_KEY_ID`와 `SECRET_ACCESS_KEY`로 프로세스 안에서 전달한다. 전역 `AWS_*`를 바꾸지 않고 API·계획·반환 metadata·로그에 키를 넣지 않는다. `RemoteEnv` 자체는 키 값을 반환하지 않는다.
- 자동 구성할 MinIO endpoint·접두사·키 참조 및 빈 키를 검사하고, 모든 원격을 사전 검사한 뒤 환경변수 설정을 시작한다. 오류에 endpoint 원문이나 키 값을 포함하지 않는다. 설정 함수의 오류도 값이 섞일 수 있어 그대로 전파하지 않는다. 설정 함수 자체가 실패하면 이미 설정한 일부 변수의 복구까지 보장하지 않으므로 서버 시작을 실패시켜야 한다.
- `_TYPE`가 이미 설정된 원격은 운영자 제공 구성으로 보고 덮어쓰거나 별도 키를 조회하지 않는다. 이 경우에도 버킷 접두사는 필요하다. 실제 연결과 기존 원격의 인증·보안 검증은 운영자가 책임진다.
- endpoint는 HTTP(S) origin으로 제한하고 사용자 정보·경로·query·fragment는 거부한다. 온프렘 내부 주소와 HTTP는 허용한다. 이는 운영자가 관리하는 설정이며 일반 이전 API가 임의 endpoint를 지정할 수 없다. 목적지 IP·DNS·리다이렉트 접근 제한과 TLS 적용은 네트워크 및 배포 정책의 책임이다.
- `no_check_bucket=true`는 rclone이 버킷을 생성하거나 존재 확인하는 동작을 생략하기 위한 기존 정책이다. 버킷은 저장소 드라이버가 준비하며, 객체 복사와 엔진의 `rclone check --one-way` 검증은 유지한다. 체크섬 검증 해제 옵션은 넣지 않는다. 옵션 설명은 [rclone 공식 S3 문서](https://rclone.org/s3/)를 따른다.

### 이미지 발행과 선택

- CI 성공 후 자동 실행은 계속 **기본 이미지**만 빌드한다. MinIO 관리 드라이버를 포함하지 않으며 기본 설정의 MinIO 차단을 해제하지 않는다.
- 기존 발행 workflow의 수동 `flavor=minio` 선택만 `-tags minio` 바이너리와 `<7자리 SHA>-minio@sha256:...` 이미지를 발행한다. 기본 태그는 기존 `<7자리 SHA>`다. 두 변형은 별도 concurrency group으로 서로 취소하지 않는다.
- 수동 MinIO 발행은 **artifact만 발행**하고 infra 토큰을 사용하는 핀 변경 단계를 실행하지 않는다. 운영 이미지 선택과 MinIO storage·Secret 연결은 팀의 ADR 0016 관련 합의 후 별도로 검토한 infra 변경으로 수행한다. workflow 선택 자체가 라이선스 검토 완료나 운영 승인을 증명하지 않는다.
- 자동 핀 도구는 현재 이미지와 신규 이미지의 flavor를 비교한다. 다르면 해당 시도에서 GitHub 읽기 이후 tree·commit·ref 쓰기를 하지 않고 `infra pin skipped`로 보고한다. 현재 이미지와 lock의 digest 일치는 여전히 먼저 검사한다. 동시 infra 변경으로 재시도할 때도 flavor를 다시 확인한다. 앞선 시도에서 ref 경합으로 버려진 Git 객체는 존재할 수 있으므로 summary는 핀 적용이 없었다고만 보고한다. 잘못된 변형·SHA·digest·저장소는 계속 거부한다.
- 기본→MinIO 및 MinIO→기본 선택은 자동 핀의 역할에서 제외한다. helper는 같은 flavor끼리의 핀 형식을 지원하지만, 현재 workflow에서 MinIO 자동 핀은 호출하지 않는다.

## 검토한 대안

- 기본 이미지에 항상 MinIO 포함: ADR 0016의 명시적 의존성 격리와 팀의 라이선스 선택을 없애므로 채택하지 않는다.
- main마다 두 이미지를 모두 발행하고 선택된 flavor를 자동 갱신: MinIO artifact 발행 자체가 자동화되고 운영 적용 경로도 확대된다. 팀의 배포·라이선스 정책이 정해진 뒤 별도 결정한다.
- MinIO 태그만 추가하고 기존 자동 핀 유지: 다음 기본 발행이 운영 이미지를 되돌릴 수 있어 안전하지 않다.
- 사진 수동 복사만 사용: 비상 대안은 가능하지만 계획·검증 경로에서 파일이 빠지는 기존 누락을 해결하지 못한다.

## 결과

- MinIO의 정상 설정에서 사진 버킷이 등록되며, 자동 구성의 불완전 설정은 서버 시작 단계에서 실패한다. `PLATFORM_APP_FILES=0` 또는 API의 명시적 `files=false`는 계속 파일 이전을 끈다. 저장소 없는 다른 환경의 기본 파일 생략 정책은 이 변경 범위 밖이다.
- MinIO를 운영에서 선택한 동안 기본 main 발행의 자동 갱신은 **의도적으로 멈춘다**. 플랫폼 담당자는 보안 패치마다 CI가 통과한 main의 MinIO 이미지를 수동 발행하고, digest와 storage 설정 호환성을 검토한 infra 변경으로 갱신해야 한다. 담당자·빈도·팀 합의는 배포 전 정해야 한다. 자동으로 최신화된다고 표시하면 안 된다.
- 원격은 기존 플랫폼 MinIO 관리 키를 사용하므로 앱 한 버킷에만 제한된 복사용 계정은 아니다. 플랫폼 프로세스의 자격증명 경계 안에 머무르며, 추후 전용 복사 계정 도입은 별도 권한 설계다.
- 실제 endpoint·Secret 환경변수 이름·접두사는 인프라 담당자가 공유해야 한다. MinIO 연결, 버킷 준비, GKE→onprem 사진 복사와 검증, 운영 이미지 배포는 로컬 단위 테스트로 증명되지 않는다. 운영 설정을 활성화하거나 실제 키를 조회하지 않았다.
- `rclone check`에서 양쪽 저장소에 공통 해시가 없거나 멀티파트 객체의 ETag가 콘텐츠 해시가 아니면 완전한 바이트 일치 증명은 별도 검증이 필요하다. 이 변경은 검증 방식을 강화했다고 주장하지 않는다. 실제 시연 전 테스트 사진의 개수·내용 및 검증 결과를 확인하고 필요한 경우 download 검증을 별도로 선택해야 한다.
