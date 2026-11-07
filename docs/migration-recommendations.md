# 개요의 AI 이전 추천

기존 샌드박스와 같은 `hopai` 모델을 읽기 전용 추천 모드로 실행한다. 개요 진입/명시 새로고침에서만 작업을 시작하고 15분 동안 공유한다. `/v1/migration-recommendations/status`는 결과만 읽는다. 자동 polling이나 실제 이전 실행은 없다.

## 연결

1. `hopai recommend`가 포함된 검증된 이미지를 고정한다.
2. init과 main의 `KIND=recommend` 분기를 검증한 Job template에 `hopcharlie.dev/recommendations: v1` annotation을 둔다. 추천 init은 기존 AI 키·플랫폼 callback token만 읽고 kubeconfig를 읽지 않는다.
3. API의 기존 `PLATFORM_AIRUN_JOB_TEMPLATE`, 앱·환경 목록과 게이트웨이 연결을 사용한다. 구버전 template은 `not_configured`로 응답한다.
4. 비용 비교가 필요하면 검토한 아래 스키마의 운영자 파일을 `PLATFORM_RECOMMENDATION_COST_FILE`로 지정한다. 파일과 클러스터 설정 연결은 기존 운영 승인 범위를 따른다. 파일을 저장소에 넣을 필요는 없다.

파일 형식은 `{"version":1,"quotes":[]}`다. quotes의 각 항목은 다음 필드를 갖는다. 수치는 예시값을 자동 적용하지 않고 운영자가 검토한 실제 견적을 넣는다.

| 필드 | 의미 |
| --- | --- |
| app, environment, region | 등록 앱·환경과 정확한 지역. 온프렘의 지역이 없으면 빈 문자열 |
| workloadRevision | 현재 등록된 앱 `specCommit` |
| monthlyUsd, periodHours | 해당 운영시간으로 산정한 앱 전체 월 견적. 기간은 1~744시간이며 양쪽이 같아야 함 |
| observedAt | RFC3339 검토 시각. 24시간을 지나면 비교하지 않음 |
| includesStorage, includesDataTransfer, includesOperations | 스토리지·반출·운영비 포함 여부 |
| includesPower, includesAmortization | 온프렘에서 필수인 전력·장비 상각 포함 여부 |
| commitmentReviewed, capacityReviewed, residencyReviewed | 약정·용량·데이터 정책을 검토했는지 여부 |

공개 AWS 단가는 지역·Linux m7i.large 컴퓨트 참고값이며 앱 전체 청구액이 아니다. GCP 인증 가격 카탈로그가 없으면 지역별 단가를 만들지 않는다. 반도체 수급 전망도 참고 근거이며 클라우드 인상률로 사용하지 않는다. 비용 파일이 없더라도 등록된 이전 경로·최신 준비 상태·기존 데이터 국가 제한을 비교한 추가 확인 후보를 표시하고 이전 조건 화면으로 연결할 수 있다.

## 한계와 검증

추천은 승인이나 데이터 반출 허가가 아니다. 실제 이전은 최신 원본·대상, 데이터·권한·용량 검증과 사람 승인을 다시 거친다. Ready 노드가 있다는 사실을 여유 메모리로 해석하지 않는다.

현재 단일 API 메모리 캐시를 사용한다. 재시작 뒤 기존 callback을 복원하지 않으며 남아 있는 Job의 생성 시각으로 같은 15분 구간의 재실행 비용을 차단한다. 다중 API의 분산 비용 잠금과 실제 청구 수집은 별도 범위다. 코드 검사는 provider fake/임시 K8s client이며 운영 AI 호출·실제 비용 절감을 증명하지 않는다.
