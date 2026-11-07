# 0040. 게이트웨이 쓰기 보류의 내구성·대기 상한·부하·계측을 보완한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

사용자가 세션 정합성을 제외한 게이트웨이 보완과 쓰기 지연 최적화를 요청했다.
기존 메서드 기반 읽기/쓰기 분류, 단일 쓰기 대상, 승격 뒤 자동 원복 금지 계약은 유지한다.
테스트의 fake backend는 실제 이전이나 운영 지연 개선률의 증거가 아니다.

## 결정

1. 새 hold는 디스크 저장에 성공해야 200을 반환한다. 실패하면 이번에 만든 hold만 원본
   라우팅으로 해제한다. 기존 hold는 저장 오류로 풀지 않는다. 관리 변경의 저장 구간은
   controlMu로 직렬화하되 drain 대기는 잠금 밖에서 수행해 운영자가 취소할 수 있게 한다.
   release 직전에 현재 route+hold를 저장한다. 이 저장이 실패하면 해제하지 않는다.
   release 이후 저장만 실패한 경우 메모리에서 이미 처리 중인 작업을 원복하지 않으며,
   재시작 시 마지막으로 저장된 target+hold로 보수적으로 복원한다. route/weights의 기존
   메모리 적용·성공 응답 계약은 유지하되 저장 오류를 진단/메트릭에 노출한다. 안전한 이전은
   반드시 hold → route → release 프로토콜을 사용해야 한다. 임의의 무보류 route를
   트랜잭션으로 만든다는 보장은 아니다.
2. 디스크 오류는 /readyz와 앱별 /healthz에 503, persistence_healthy=0으로 표시한다.
   루트 /healthz는 liveness로 남겨 현재 서비스 중인 gateway를 디스크 오류만으로 트래픽에서
   제거하지 않는다. 저장이 다시 성공하면 진단이 회복된다. StateFile 없는 테스트 모드는
   persistence_enabled=0이다. control 변경을 막연히 실패로 돌려 실제 cutover를 원복하지 않는다.
3. 보류 요청의 초기 본문 읽기에 기본 10초, 요청 도착 이후 MaxHold 중 짧은 상한을 건다.
   네트워크 read deadline과 body close를 함께 사용하고 전달 전에 deadline을 제거한다.
   408 body_timeout 또는 503 hold_timeout인 요청은 백엔드로 보내지 않는다. 64KiB 이후의
   큰 본문은 기존처럼 streaming하고 전체 업로드 크기를 이 제한으로 바꾸지 않는다.
4. 앱별 백엔드 쓰기 전달 동시성을 기본 64로 제한한다. 새 쓰기도 같은 제한을 사용한다.
   대기열 요청은 계속 in-flight로 계산해 다음 hold가 이를 누락하지 않는다. 슬롯 대기도
   MaxHold 안에 끝나야 하며, 초과 요청은 503 write_queue_timeout이고 자동 재전송하지 않는다.
   본문 버퍼는 앱 전체 합산 기본 64MiB 예약 한도를 두고 ReadAll 성장분을 보수적으로 예약한다.
   실제 전체 Go heap 상한이나 DB 내부 실행 동시성을 보장하는 값은 아니다.
5. 기존 backend request_duration 의미를 유지하고 end_to_end, hold_wait, write_queue,
   write_pause 지표를 추가한다. write_pause는 hold 시작부터 해제까지라 기존 요청 drain도
   포함한다. 새 HTTP 전체 지연은 gateway handler 관점이며 브라우저의 DNS/TLS 시간을
   포함하지 않는다. 실패/취소/본문 오류 등도 가능한 범위에서 기록하고 읽기 재시도를 중복
   client request로 세지 않는다. 지연 버킷은 5분까지 확장하며 그 이상 API 백분위는 마지막
   유한 구간의 하한 추정치이다. 정확한 장시간 latency 표본이라고 해석하지 않는다.
6. 앱별 AppSpec.healthPath를 저장·복원하고 deploy가 public service의 healthCheck를
   전달한다. 기존 기본값 /healthz는 호환성을 위해 유지한다. 의존 서비스 준비를 검사하려면
   앱이 /readyz 같은 경로를 선언해야 한다. query를 포함한 로컬 HTTP path를 지원하며
   외부 URL/fragment를 거부한다. 2xx만 정상으로 보고 redirect는 따라가지 않는다.
   이전 경로의 늦은 probe 결과가 새 설정을 정상으로 덮어쓰지 않게 한다.

## 검토한 대안

- 모든 저장 오류에 즉시 자동 rollback: 이미 DB/트래픽 전환이 적용됐을 수 있어 제외한다.
- 요청 대기 중 무제한 본문 수신: MaxHold와 메모리 상한을 우회하므로 제외한다.
- release 후 새 요청은 무제한 통과: 기존 대기열을 굶기고 DB 부하를 다시 키우므로 제외한다.
- DB 안정 확인·정합성 검사 생략: 지연 단축을 위해 이전 안전성을 약화시키지 않는다.
- 모든 앱을 임의의 /readyz로 변경: 경로와 dependency 의미는 앱 계약이므로 선언을 따른다.

## 결과

저장 실패·복구, 보류 중 느린 본문, 전체/보류 지연, 긴 지연 구간, 제한된 dispatch와 다음
hold drain, 앱 간 버퍼 예산, readiness path 저장·복원과 redirect를 회귀 검사한다.
Windows에서 관련 gateway/command/deploy 테스트와 vet/build를 수행한다. -race는 현재
CGO 비활성·C 컴파일러 부재로 실행하지 못했다. 실제 EKS/GKE/온프렘 부하 실험, HyperDX
대시보드 반영, 운영 최적 동시성 튜닝과 지연 절감률은 이 로컬 검증으로 입증하지 않는다.
세션, GET 부작용 자동 탐지, read-your-writes 경로 예외, gateway HA는 이번 범위가 아니다.
