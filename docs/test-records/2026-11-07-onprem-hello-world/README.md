# 2026-11-07 온프렘 Hello World 배포 성공

**결과: 성공.** 공개 주소 <https://hello-onprem.hopcharlie.dev/>에서
실제 온프렘 k3s Pod가 반환한 HTTP 200과 `Hello World!`를 20회 연속 확인했다.
임시 경로를 정리한 뒤에도 공개 주소의 정상 응답을 다시 확인했다.

관측 시각, 각 요청의 상태·본문·응답 Pod, Kubernetes 상태와 배포 결과는
[evidence.json](evidence.json)에 기록했다. 자격증명 값은 포함하지 않는다.

## 확인한 경로

```text
공개 HTTPS → AWS 트래픽 게이트웨이
  → 관리 EKS의 app-onprem-hello-onprem Service
  → Tailscale 웹 프록시
  → 온프렘 k3s의 hello-onprem-web Service → Hello World Pod
```

localhost 포트포워딩을 통과한 결과와 구분해 공개 URL에서 검증했다.

| 항목 | 실제 관측값 |
| --- | --- |
| 앱 / 네임스페이스 | `hello-onprem` |
| 대상 환경 / Argo 클러스터 | `onprem` / `charlie-dev-onprem-workload` |
| 온프렘 노드 | `desktop-c3itrvv` |
| 응답 Pod | `hello-onprem-web-67f9cbcf69-vzhdm` |
| Argo Application | `hello-onprem-onprem`, `Synced / Healthy` |
| 최종 플랫폼 배포 | `dep_4929ba7c820e`, `healthy` |
| 배포 완료 | 2026-11-07 21:11:22 KST (`12:11:22.335691Z`) |
| 소스 | `moimroom-mig`, `demo/hello-onprem` |
| 소스 커밋 | `c74dfe45885ddc6b17145205e82ec432a1bed0cc` |
| 최종 이미지 digest | `sha256:9e3af4a5bb6745acd3ee7dabf46a17bac0acb9dbc5486e9d3bd4cc97312d11f1` |
| 이미지 / 실행 일치 | 플랫폼 배포의 digest와 실제 Pod `imageID` 일치 |
| 공개 GET `/` | 20/20 HTTP 200, 본문 `Hello World!` |

배포 이미지는 기존 AWS CodeBuild에서 빌드하고 ECR에 저장했다.
앱의 `platform.yaml`에는 웹 서비스 1개만 있고 DB·S3 리소스는 없다.
기본 브랜치의 기존 `moimroom-mig` 앱은 이 테스트 앱으로 바꾸지 않았다.

## 발견한 차단 조건과 해결

1. Argo CD의 온프렘 등록은 `moimroom`만 감시하고 `clusterResources=false`였다.
   새 Namespace를 관리할 수 없어 비교 단계에서 실패했다. 승인 후 등록 대상에
   `hello-onprem`을 더하고 Namespace 관리를 허용했다. 기존 토큰과 클러스터 RBAC는 유지했다.
2. 온프렘 노드에는 ECR 인증이 없어 `no basic auth credentials`로 이미지 다운로드가 실패했다.
   승인 후 테스트 네임스페이스에 ECR pull Secret을 만들고 기본 ServiceAccount에 연결했다.
3. 초기 배포 `dep_f530d7bdc88c`는 Pod가 Ready여도 `ClusterIP`의 로드밸런서 주소를
   찾으려다 실패했다. 이 실패 기록은 성공으로 덮어쓰지 않았다.
4. 운영에 반영된 [infra #145](https://github.com/2026-Softbank-Hackathon-Charlie/infra/pull/145)의
   웹 전용 ProxyClass·NetworkPolicy·onprem backend template을 사용하고,
   관리 EKS의 `app-onprem-hello-onprem` 연결을 구성했다.
   앱별 게이트웨이 경로와 최종 배포 상태, 실제 공개 응답까지 확인했다.
5. 초기 임시 프록시는 기본 거부 정책에 막혀 DNS에 접근하지 못했다.
   웹 전용 정책이 적용되는 최종 경로로 통일하고 사용하지 않는 테스트 Service·정책 4개를 제거했다.

수동 설정과 병행된 다른 작업자의 GitOps 변경을 포함한 결과다.
이 기록은 모든 연결 설정을 플랫폼이 자동으로 설치했다는 증거가 아니다.

## 검증 범위와 남은 조건

- 확인: 실제 소스 빌드, ECR 이미지, Argo 배포, 온프렘 Pod Ready,
  이미지 digest 일치, 게이트웨이를 통한 공개 HTTPS 응답.
- 미검증: DB 이전·쓰기 누락·세션 정합성·MinIO 사진 복사·GPU·장애 복구·장시간 부하.
- 일회용 코드로 신규 장비를 자동 등록하는 온보딩 성공을 검증한 것은 아니다.
  이미 등록된 온프렘 클러스터를 사용했다.
- 테스트 ECR pull 인증은 발급 당시 기준 12시간 유효하다.
  이후 새 Pod가 이미지를 다시 받아야 한다면 갱신 또는 정식 credential provider가 필요하다.
- 성공 관측은 기록 시점의 결과다. 이후 장비 종료나 연결 변경까지 가용성을 보장하지 않는다.

## 정리

테스트 앱은 공개 확인을 위해 유지한다. 삭제할 때는 콘솔의 `hello-onprem` 앱 삭제를
사용하고, 관리 EKS의 수동 Service `app-onprem-hello-onprem`도 따로 정리한다.
이름과 `app.kubernetes.io/managed-by=hello-onprem-verification` 소유 표식을 확인한 뒤
해당 테스트 리소스만 제거한다. 다른 앱·온프렘 클러스터 자체는 삭제하지 않는다.
