# 이전 시 운영 DB 부하 관리

플랫폼 API 프로세스에 `HOPMIGRATE_LOAD_POLICY=/etc/hopmigrate/db-load-policy.json`을 설정하면 실행 생성에 사용하는 migration-engine이 운영자 정책을 읽는다. 클라이언트 Plan이나 요청에서 Prometheus URL, 토큰, 쿼리와 임계값을 지정하지 않는다. 파일은 읽기 전용으로 마운트한다. 환경 변수가 없으면 지표 가드는 비활성이다.

JSON 구조·허용 범위·실제 scrape 시각 쿼리·양쪽 endpoint 등록 방법은 [엔진 운영 안내](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/blob/c508dbf3ba75f61172fa8473f571e5d5c12981b8/docs/db-load-budget.md)를 따른다. source 및 target의 실제 `env/namespace/cluster`가 모두 필요하다. 잘못된 정책이나 등록 누락은 실행 엔진 생성에 실패하며, 지표가 없거나 오래되면 정상으로 처리하지 않는다. 설정 변경은 새로 생성하는 엔진부터 적용된다.

CPU/IO 초과 시 새 작업 대기, 디스크/WAL 위험 시 실패, 검증 배치 크기·휴식·쿼리 제한이 적용된다. 관측 결과는 기존 엔진 metric 이벤트 및 단계 evidence에 포함된다. 별도 API나 정상 수치를 만드는 fallback은 추가하지 않았다.

쓰기 차단 전 양쪽 부하를 다시 확인한다. 차단 중 관측 회복 대기를 넣지 않는다. 승격 요청 이후 실패는 자동 원복 대신 halt한다. 실행 중 CNPG 물리 복제의 속도나 DB CPU를 강제 제한하지는 않으며, 엔진 취소 후에도 기존 복제는 계속될 수 있다. 정책 배포 및 실제 운영 부하 측정은 이 변경에서 수행하지 않았다.
