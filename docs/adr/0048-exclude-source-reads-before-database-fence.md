# 0048. DB 쓰기 차단 전에 source 읽기를 lease로 제외하고 drain한다

- 상태: 채택
- 날짜: 2026-11-07

## 상황

source DB의 앱 연결을 차단하려면 게이트웨이 읽기도 source에서 빠져야 한다.
읽기 가중치가 target 100%여도 기존 write backend fallback과 미리 선택한 retry는
source에 연결할 수 있다. 기존 전체 읽기 카운터는 backend 선택 잠금 해제 후
증가했으며 어느 backend를 사용할지도 구분하지 않았다.

## 결정

인증된 앱별 관리자 API에 `POST /v1/reads/exclude`와 `include`를 추가한다.
본문은 `{backend, lease}`이며 lease는 실행별 고유한 소유 식별자다. 새 exclude는
쓰기 Hold와 in-flight 쓰기 0을 요구하고, source 제외를 저장한 뒤 기존 source
읽기를 drain해야 200을 반환한다. 저장·drain 실패에도 현재 제외를 유지한다.

읽기 primary·미리 선택한 retry의 예약과 제외는 같은 router 잠금으로 직렬화한다.
가중치·fallback·retry 모두 제외한 backend를 선택하지 않는다. 기존에 예약했지만
아직 전달하지 않은 retry도 추적하며, retry 직전 제외를 다시 검사한다. health probe도
같은 source별 카운터로 추적하고 제외 이후 새 probe를 보내지 않는다.

include는 owner lease를 비교하고, 해제한 스냅샷을 먼저 저장한 후 메모리에서
해제한다. 이 두 단계 동안 saveMu를 유지해 동시 background save가 예전 제외를
파일에 다시 쓰지 못하게 한다. 다른 owner는 거부하고, 이미 제외가 없으면 성공한다.

GET state는 선택적 capability와 backend별 lease·예약 수를 제공한다. 영속 상태 파일이
없으면 capability는 false이며, 필드가 없는 구버전을 지원으로 추정하면 안 된다.
활성 lease 중 PUT·DELETE, 제외 backend로 Route·양수 읽기 가중치, 그 backend가
writer인 상태의 Release를 거부한다. 다른 target으로의 Route는 허용한다.

## 검토한 대안

전체 in-flight 0만 기다리면 계속되는 target 트래픽 때문에 source drain이 끝나지
않으며, 선택과 카운트 등록 사이의 요청도 놓친다. 가중치만 바꾸면 fallback·retry를
막지 못한다. 시간 만료로 자동 include하면 SQL HBA가 아직 닫혀 있거나 역할 확인이
끝나지 않은 source로 읽기를 다시 보내므로 명시적 owner include만 허용한다.

## 결과

증명 대상은 현재 단일 gateway 인스턴스다. HA 전체 drain이나 HTTP 밖 DB 쓰기
차단 자체를 증명하지 않는다. DB fence 실행기는 먼저 지원·lease·source별 0과 쓰기
Hold를 확인하고, include 전에 DB 역할과 HBA 복원을 확인해야 한다. 활성 lease가
있을 때 이를 모르는 gateway 버전으로 다운그레이드하면 안 된다.

이미 선택한 retry는 사용하지 않더라도 요청 종료까지 예약하므로 긴 스트림은 drain을
지연시킬 수 있다. timeout은 제외 상태를 유지하며 운영자가 확인할 수 있게 한다.
운영 SQL·트래픽 변경은 이번 코드·테스트 변경에 포함하지 않는다.
