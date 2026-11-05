# 0026. 게이트웨이는 지금처럼 한 대로 두고, 여러 대로 늘리는 방법과 재시작을 줄이는 방법은 기록만 한다

- 상태: 채택
- 날짜: 2026-11-05

## 상황

ADR 0004와 0012는 게이트웨이가 단일 장애점이라고 적었다. 모든 앱의 사용자 요청이 게이트웨이 한 대를 지난다. 관리 클러스터의 공용 ALB는 `app.hopcharlie.dev`와 `*.hopcharlie.dev`를 둘 다 Service `hopcharlie-gateway`(포트 8000)로 보내고, 그 뒤에는 pod 하나뿐이다. 그래서 게이트웨이가 재시작하면 모든 앱이 함께 끊긴다.

코드와 차트에서 확인한 지금 구조는 이렇다.

- infra 차트 `helm/charts/hopcharlie-platform/templates/gateway.yaml`: `replicas: 1`, `strategy: Recreate`, PVC `hopcharlie-gateway-data`(gp3, ReadWriteOnce, 1Gi)를 `/data`에 붙이고 `-state-file /data/gateway-state.json`으로 띄운다. 이미지는 API와 같은 `images.platform`이다.
- 파일에 남는 것(`internal/gateway/persist.go`의 `persistedState`): 앱마다 쓰기 환경(`writeBackend`), 읽기 비율(`readWeights`), 쓰기 보류 여부(`writesHeld`, `heldSince`), 관리 API로 등록한 앱의 환경 주소. 바뀔 때마다 임시 파일에 쓰고 fsync한 뒤 rename한다.
- 프로세스 메모리에만 있는 것(`internal/gateway/gateway.go`의 `router`): 보류 세대(`gen`), 보류 중에 기다리는 쓰기 수와 그 요청들, 처리 중인 쓰기 수(`inflightWrites`), 처리 중인 읽기 수, 1초 단위 통계. hold가 200을 돌려주는 조건(처리 중인 쓰기가 0)도 이 메모리 값으로 판단한다.
- 종료할 때(`cmd/gateway/main.go`의 `shutdown`) 보류 중에 기다리던 쓰기에는 503 `shutting_down`을 돌려주고, 공개 서버와 관리 서버를 각각 최대 10초 동안 정리한 뒤 끝난다. 보류 여부는 파일에 남으므로 새 pod는 보류를 이어 간다.

그래서 replicas만 2로 올리면 안 된다. 엔진의 hold는 한 pod의 처리 중인 쓰기만 기다리고 그 pod의 새 쓰기만 붙잡는다. 다른 pod로 들어온 쓰기는 보류 중에도 옛 원본으로 가서, 전환 중에 쓰기가 두 DB로 나뉜다. ReadWriteOnce 볼륨의 상태 파일 하나를 두 프로세스가 덮어쓰는 문제도 생긴다.

## 재시작 시간 측정

게이트웨이를 일부러 재시작하지 않고, 이미 일어난 재시작의 기록으로 쟀다. 2026-11-02부터 11-05까지 게이트웨이 pod는 21개였고, 처음 생성을 빼면 재시작이 20번이다(11-03 09:43 UTC부터 11-05 10:47 UTC까지).

| 구간 | 값(20번) | 출처 |
| --- | --- | --- |
| ALB 대상 해제부터 새 대상 등록까지 | 8~16초, 중앙값 11초 | aws-load-balancer-controller 로그의 `deRegistered targets`, `registered targets` 시각(대상 그룹 `k8s-charliep-hopcharl-5e226528f9`, `app.hopcharlie.dev`) |
| 같은 구간, `*.hopcharlie.dev` 대상 그룹 | 9~16초, 중앙값 12초(13번, 11-04 06:40 UTC부터) | 같은 로그(`k8s-charliep-hopcharl-0ab752acec`) |
| 새 pod 생성부터 Ready까지 | 6~15초, 중앙값 10초 | Prometheus의 `kube_pod_created`, `kube_pod_status_ready_time`(kube-state-metrics, 보관 1주) |
| 새 등록부터 ALB가 healthy로 보기까지 | 15~30초로 추정(측정 아님) | 대상 그룹 헬스 체크 설정: 간격 15초, 시간 제한 5초, healthy 기준 2번 |

지금 pod(11-05 10:47 UTC)의 단계별 시각은 이렇다. 생성 10:47:14, 볼륨과 샌드박스 준비 10:47:19(5초), init 컨테이너 `fetch-settings`(SSM에서 설정 읽기) 10:47:19~10:47:23(4초), 컨테이너 시작과 Ready 10:47:25, ALB 등록 10:47:27.

읽은 내용은 이렇다.

- 옛 pod는 빨리 내려간다. Recreate는 옛 pod가 모두 끝난 뒤에 새 ReplicaSet을 만드는데, 20번 모두 옛 대상 해제와 새 pod 생성이 같은 초 또는 1초 차이였다. 처리 중인 요청이 길면 최대 10초씩 더 걸릴 수 있다.
- 볼륨을 옮긴 적은 없다. PV `pvc-615cad2d-...`의 nodeAffinity가 `ap-northeast-2c`이고, 그 존의 노드는 노드 그룹 `charlie-dev-management-ap-northeast-2c`(min=max=1)의 `ip-10-40-27-132` 하나다. 21개 pod가 모두 이 노드에서 떴으므로 EBS 분리와 연결이 없었다. 이 노드가 바뀌면 새 노드가 뜨고 볼륨이 다시 붙을 때까지 게이트웨이가 Pending이다. 이 시간은 재지 않았고 분 단위로 본다.
- 사용자가 느끼는 끊김은 측정한 8~16초에 ALB 헬스 체크 15~30초를 더해 약 25~45초로 본다. CloudWatch의 1분 단위 `HealthyHostCount`(Minimum)는 이 기간에 0으로 내려가지 않아서 1분보다 짧은 끊김을 가려내지 못했다. ALB 접근 로그는 꺼져 있다. ALB 단위 `HTTPCode_ELB_503_Count`는 재시작한 분마다 올라갔지만 같은 이미지로 함께 재시작한 API(대상 해제부터 등록까지 37~414초, 중앙값 60초)의 몫과 나눌 수 없다.
- 재시작은 모두 플랫폼 이미지 교체에서 왔다. 20번 모두 infra의 `chore: 플랫폼 이미지를 ...로 교체` 커밋 뒤 1~5분 안에 일어났고(Argo CD `charlie-management-hopcharlie-platform`, 자동 동기화), 기록이 남은 마지막 4번은 동기화 시작 뒤 1~3초 안에 게이트웨이 대상이 해제됐다. 게이트웨이 코드가 바뀌지 않은 커밋도 API와 이미지를 같이 쓰므로 게이트웨이를 재시작시켰다. 콘솔 이미지 교체와 그 밖의 설정 커밋은 게이트웨이를 재시작시키지 않았다. 21개 pod 모두 컨테이너 재시작 수가 0이라 장애나 OOM으로 인한 재시작은 없었다(`kube_pod_container_status_restarts_total`).

## 결정

게이트웨이는 지금처럼 한 대(`replicas: 1`, Recreate, 상태 파일)로 둔다. 아래 두 방법은 기록만 하고 이번 일정에는 구현하지 않는다.

### 대안 A: 상태를 플랫폼 DB로 옮기고 모든 복제본에 hold와 release를 건다

- 게이트웨이의 상태(앱마다 쓰기 환경, 읽기 비율, 보류 여부, 앱의 환경 주소)를 상태 파일 대신 플랫폼 DB에 둔다. 복제본은 시작할 때 DB에서 읽고, 다 읽기 전에는 Ready가 되지 않는다. 그래서 보류 중에 새로 뜬 복제본도 보류 상태로 시작한다.
- 엔진은 headless Service(예: `hopcharlie-gateway-pods`)의 DNS로 모든 복제본 주소를 찾고, 보류 여부를 DB에 먼저 기록한 뒤 각 복제본의 `/apps/{app}/v1/writes/hold`를 부른다. 모든 복제본이 200(자기 처리 중인 쓰기가 0)으로 답했을 때만 전환을 이어 간다. 하나라도 실패하면 hold한 복제본에 release를 부르고 hold를 실패로 돌려준다. route와 release도 모든 복제본에 부르고 모두 답해야 끝난다.
- 카나리 판정이 읽는 `/v1/state?window=` 통계는 복제본마다 따로 있으므로 플랫폼(`internal/runs`)이 복제본의 값을 더해야 한다.
- 복제본이 둘 이상이면 RollingUpdate(`maxUnavailable: 0`)와 ALB pod readiness gate로 재시작 중에도 healthy 대상이 남는다. 상태 파일용 ReadWriteOnce PVC는 없앤다.
- 바꿔야 하는 곳: 게이트웨이(`internal/gateway`의 상태 저장과 시작), 이전 엔진(복제본 찾기와 모두 응답 확인), 플랫폼 runs(통계 합치기), infra 차트(headless Service, replicas, 전략, PVC 제거, DB 접속), kind E2E(복제본 둘에서 hold 중 쓰기 누락 0건).

### 대안 B: 한 대로 두고 재시작을 줄인다

- 게이트웨이 이미지를 API와 나눈다(차트에 `images.gateway`, 기본값은 `images.platform`). 지난 20번의 재시작은 모두 플랫폼 이미지 교체에서 왔으므로, 게이트웨이 코드가 바뀔 때만 재시작하게 하는 것이 효과가 가장 크다.
- ALB 헬스 체크 간격을 15초에서 5초로 줄인다(게이트웨이 Service의 `alb.ingress.kubernetes.io/healthcheck-interval-seconds`, healthy 기준 2번 유지). 추정 15~30초인 구간이 5~10초가 된다.
- 설정을 SSM에서 읽는 init 컨테이너(4초)는 그대로 둔다. 줄어드는 시간에 비해 설정 전달 방식을 바꾸는 범위가 크다.
- 노드 그룹 `ap-northeast-2c`의 노드를 바꾸지 않는다. 바꿔야 하면 그 존에 노드가 하나 더 뜬 뒤에 옛 노드를 비운다.

### 지금 한 대로 두는 이유

- 대안 A는 이전 엔진의 hold 의미(누락 0건, ADR 0004)를 바꾼다. 게이트웨이, 엔진, 플랫폼, 차트를 함께 바꾸고 복제본 둘에서 E2E로 다시 검증해야 하는데, 제출(11-07 10:00 KST)까지 남은 시간으로는 검증까지 끝낼 수 없다. 검증하지 않은 hold로 시연하는 것보다 25~45초의 재시작 끊김을 피하는 운영 규칙이 안전하다.
- 재시작은 언제 일어나는지 우리가 정한다. 지난 20번은 모두 이미지 교체였고, 노드 장애나 OOM으로 인한 재시작은 없었다. 시연 당일에 이미지를 바꾸지 않고 노드를 건드리지 않으면 계획된 재시작은 없다.
- 한 대일 때의 hold, 상태 파일, 종료 동작은 `internal/gateway`와 `cmd/gateway`의 테스트, kind E2E(`test/e2e`)로 이미 검증했다.
- 대안 B의 첫 두 항목은 게이트웨이 코드를 바꾸지 않고 infra 차트와 이미지 고정 방식만 바꾸면 되므로 시연 뒤에 먼저 한다.

## 시연 당일 운영 메모

- 노드를 정리하지 않는다. drain, 노드 그룹 업데이트(AMI 포함), 인스턴스 교체, 노드 수 변경을 하지 않는다. 게이트웨이 볼륨은 `ap-northeast-2c`에 묶여 있고 그 존의 노드는 하나라서, 그 노드가 빠지면 새 노드와 볼륨 연결이 끝날 때까지 모든 앱이 끊긴다. API(`hopcharlie-api`)도 같은 노드에 있다.
- 플랫폼 이미지를 바꾸지 않는다. platform main에 병합하면 CI가 이미지를 게시하고 infra main의 `clusters/aws-dev-management/values/hopcharlie-platform.yaml`에 `images.platform`을 바로 고정하므로, platform PR 병합과 `images.platform`을 바꾸는 infra 커밋을 시연이 끝날 때까지 미룬다. 바꾸면 Argo CD가 몇 초 안에 동기화하고 API와 게이트웨이가 함께 재시작한다. 모든 앱이 약 25~45초 끊기고, 이전이 진행 중이면 보류 중에 기다리던 쓰기가 503을 받는다. 콘솔 이미지 교체는 게이트웨이에 영향이 없다.
- 꼭 바꿔야 하면 진행 중인 이전이 없을 때, 시연 순서 사이에 바꾸고 `kubectl -n charlie-platform get pod -l app.kubernetes.io/component=gateway`로 새 pod가 Ready인지 본 뒤 1분 더 기다린다(ALB 헬스 체크).
- 시연 전에 게이트웨이 pod의 RESTARTS가 0이고 AGE가 마지막 이미지 교체 시각과 맞는지 확인한다.

## 결과

- 게이트웨이는 여전히 모든 앱의 단일 장애점이다. 재시작마다 모든 앱이 약 25~45초 끊긴다(측정 8~16초와 추정 헬스 체크 시간의 합).
- 발표에서는 이 한계와 대안 A의 설계(상태를 DB로, headless Service로 모든 복제본에 hold, 모두 답할 때만 진행)를 함께 설명한다.
- 시연 뒤에 대안 B의 이미지 분리와 헬스 체크 간격부터 infra 차트에 반영하고, 대안 A는 엔진과 함께 설계를 다시 검토한다.
