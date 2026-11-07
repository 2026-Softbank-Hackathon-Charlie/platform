# 0044. 등록 앱 DB를 최대 세 환경의 복제 그룹으로 확장한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

`moimroom-mig`는 GKE·EKS에 등록되어 있으나 노트북 k3s `onprem`으로 이전해야 한다. ADR 0030의 두 환경 가정은 카탈로그, DB 경로, 공통 CA 봉인, 차트와 엔진의 미래 복제 source 검사에 걸쳐 있다. 숫자 제한만 늘리면 독립 primary가 생성되거나 대상의 WAL 복제가 잘못 연결될 수 있다.

## 결정

원래 `PrimaryEnv`와 `PeerEnv`는 생성 당시의 앵커로 보존한다. 현재 역할은 CNPG와 실제 PostgreSQL recovery 상태로 판정한다. 등록 앱의 `Environments`를 현재 멤버 목록으로 사용하며 최대 세 환경이 동일한 기존 앱 비밀번호와 CA를 공유한다. 두 환경의 기존 GitOps 출력은 유지하고 세 환경에서만 `dbTopology.members`를 추가한다. 추가 환경에도 운영자가 설정한 DB 호스트 템플릿, 스토리지와 sealed-secrets 인증서가 필요하다.

현재 primary는 EKS이므로 먼저 기존 경로로 EKS→GKE 이전을 검증한다. 새 onprem은 GKE를 source/primary로 부트스트랩한다. 환경 추가 API는 GKE의 건강 상태와 명시적 topology primary를 새로 관측하지 못하면 차단한다. 이 검사는 시점 관측이며 장기적인 역할 고정이나 DB 수준 writer fencing을 의미하지 않는다.

차트는 각 다른 멤버의 egress Service·NetworkPolicy·externalCluster를 만든다. 세 멤버 Cluster에 레이블을 붙이고 Argo CD가 이들의 externalClusters 목록을 반영하도록 한다. replica 역할 필드는 계속 엔진 소유다. 운영 적용 시 기존 GKE/EKS 양쪽에도 세 externalCluster가 반영됐는지 확인한 후 실행한다.

세 멤버 등록 앱의 초안만 `allowSourceRewire:true`를 기본으로 만든다. 플랫폼은 제출·실행 시 멤버 수와 환경/context/namespace/cluster/DSN 호스트를 다시 확인한다. 엔진 ADR 0018에 따라 승인된 실행의 prepare 단계에서 현재 primary의 미래 replica.source만 UID/resourceVersion 조건으로 선택한다. 대상은 이미 source의 직접 standby여야 한다. 대상 재연결, primary 역할 변경, promotion 이후 무조건 자동 롤백은 허용하지 않는다. workload admission 정책의 제한적 허용이 선행되어야 한다.

파일 이전 기능이 켜져 있고 등록 앱에 s3 리소스가 있다면 원본/대상 bucket 누락으로 사진을 조용히 제외하지 않고 초안 생성을 거절한다. 운영자가 `files:false`를 명시한 수동 복사 경로는 유지한다. MinIO 드라이버·이미지 선택은 ADR 0045를 따른다.

## 검토한 대안

- 앱을 삭제·재등록: 기존 DB와 사진을 지울 수 있어 채택하지 않는다.
- 같은 저장소를 새 앱 이름으로 gke+onprem에 등록: 기한 초과 시 기존 앱을 보존하는 시연 대안이다. 새 앱의 데이터 복제·검증은 별도로 수행해야 한다.
- target을 자동 재연결하여 모든 멤버 간 직접 이전: WAL 연속성·토폴로지 회복 검증 범위가 커 이번 변경에서 제외한다.
- 환경 제거까지 일반화: 멤버별 backup generation과 나머지 환경의 복제 연결 정리가 추가로 필요하다. 세 멤버 그룹의 환경 제거는 변경 전에 명시적으로 거절한다. 기존 두 멤버 제거 동작은 유지한다.

## 결과

지원 시연 경로는 EKS→GKE, GKE→onprem이다. 이후 onprem→GKE를 거쳐 GKE→EKS로 돌아갈 수 있다. GKE를 거치는 cascading EKS를 onprem에서 직접 승격하는 경로는 engine이 거절한다. "세 환경 등록"은 모든 방향의 자동 토폴로지 재구성을 의미하지 않는다.

infra #131과 platform #111은 이미 main에 병합되어 병합 자체가 남은 작업은 아니다. 실제 운영 이미지·차트 적용은 별도 확인해야 한다. infra #137, 노트북 디스크/메모리, k3s·CNPG·MinIO·Tailscale·이미지 pull·앱 AI 경로, 원본→대상 실데이터/사진과 롤백 시연은 이 로컬 변경만으로 검증되지 않는다. 로컬 디스크 부족으로 전체 Go 검증은 CI에서 수행하며, 단위 테스트와 Helm 렌더는 실제 이전 성공의 증거가 아니다. 운영 이미지 선택·라이선스 합의와 인프라 적용은 담당자와 분리한다.
