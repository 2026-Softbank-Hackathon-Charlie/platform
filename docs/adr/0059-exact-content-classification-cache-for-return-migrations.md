# 0059. 정상 재이전에서 전체 바이트를 다시 읽고 정확한 내용 분류만 제한된 LRU로 재사용한다

- 상태: 채택
- 날짜: 2026-11-08

## 상황

앱을 A에서 B로 성공적으로 옮긴 뒤 별도의 정상 이전으로 B에서 A로 돌아갈 때 같은 파일의 DLP 정규식 검사와 ZIP 압축 해제를 반복했다. ADR 0057은 한 스냅샷의 승인 전후 중복 검사만 줄였으며, 새로운 이전·단계에서는 내용 분류가 다시 수행됐다.

과거 성공한 run, manifest digest 또는 mtime/크기가 같다는 사실은 현재 소스 바이트, 새 파일·삭제, 정책, 권한과 대상 상태의 동등성을 증명하지 못한다. Bloom filter의 양성 응답에는 false positive도 있다.

## 결정

- 각 residency Gate가 메모리에 최대 4,096개 객체 분류를 보관하는 정확한 map+LRU를 소유한다. raw bytes, 파일명, 원격 inventory, 이전 허용 결정, 동적 IAM/RBAC·readiness 결과는 저장하지 않는다. 프로세스 재시작·eviction은 단순 cache miss가 된다. 캐시를 영구 저장하는 디스크·Secret·인프라 자원은 추가하지 않는다.
- **매 작업마다 소스 전체를 새로 내려받고 모든 객체의 전체 SHA256을 다시 계산한다.** 정확한 content digest와 분류 정책이 일치할 때만 내용 분류를 재사용한다. 현재 경로의 DLP 검사와 새로운 전체 manifest 생성은 계속 수행한다.
- 키는 RulesVersion·검사 한도·전체 reviewedFiles map·binary label과 Gate의 정책 digest, 양쪽 앱 namespace/cluster/context/kubeconfig/환경/버킷, gateway 앱 경로에 바인딩한다. 두 endpoint는 정규화해 방향만 뒤집힌 정상 재이전에서도 같은 범위임을 확인한다. 실행 ID는 다르더라도 바이트와 범위가 같으면 재사용하며, 서로 다른 환경 쌍·앱 경로·정책은 재사용하지 않는다.
- ZIP의 분류에는 내부 DLP finding과 압축 해제 바이트·member 비용도 보관한다. hit마다 **현재 검사 전체의 누적 한도에 동일한 비용을 적용**한다. 반복된 압축 객체가 ZIP 한도를 우회할 수 없다. 실패한 객체 검사 결과는 저장하지 않는다. 성공한 객체 분류는 이후 이전 전체가 실패해도 허용 결정이 아니므로 재사용할 수 있다.
- 정책 before/authorize, 현재 DB·스토리지 위치·prefix 확인, 승인 후 staging 전체 SHA256 바인딩, `--ignore-times` 전체 업로드와 대상 `check --download`는 유지한다. 캐시 hit는 이전 승인이나 누락 0의 근거가 아니다.
- 기존 파일 evidence 문자열에 per-operation `inspectionCache.hits/misses`만 추가한다. 내용, object key, 개별 hash나 다른 앱의 global 통계는 내보내지 않는다.

## 검토한 대안

- Bloom filter: 존재 가능성의 힌트에는 쓸 수 있지만 통과 근거로 쓸 수 없다. 현재 한도에서 정확한 map이 작고 단순하므로 도입하지 않았다.
- 과거 성공 run의 manifest·행 수·허용 판정 재사용: 현재 바이트와 동적 권한·대상 상태를 증명하지 못하므로 제외했다. 기존 보고서만으로 객체별 신뢰할 분류를 재구성하지 않는다.
- ETag/mtime/크기로 다운로드 생략: 동일 크기 변조, provider별 ETag 의미와 multipart upload를 구분하지 못하므로 제외했다.
- 동등성 probe의 pass를 TTL 동안 재사용: IAM/RBAC·시크릿 사용·대상 상태가 시간에 따라 바뀌므로 제외했다. 기존 Gate의 불변 정책 컴파일·정규화는 이미 재사용하며, 동적 증거는 계속 새로 수집한다.
- 분류를 무제한 또는 영구 보관: 단기 재이전의 이득에 비해 자원·민감한 fingerprint 관리가 늘어나므로 제한된 메모리 LRU를 선택했다.

## 결과

정확한 바이트 hit, 동일 크기 변조 miss, review/binary/scope 변경, 현재 파일명 DLP, archive finding·누적 한도, LRU eviction·동시 접근·실패 결과 미저장을 회귀 테스트한다. 실제 로컬 rclone으로 정방향 성공 후 새로운 Syncer의 정상 역방향 이전, 원본 변조·새 파일·삭제, warm hit 상태의 새 정책 거부를 검사한다.

benchmark는 동일 ZIP을 매번 전체 읽고 manifest를 만드는 조건에서 최초 DLP와 정확한 분류 재사용의 CPU 비용을 비교한다. 원격 전송·실제 운영 이전 시간을 줄였다는 실측은 아니다. 다수의 작은 원격 객체에서는 네트워크 왕복이 여전히 지배적일 수 있다. 캐시가 4,096개를 넘는 자료나 정책이 다른 역방향에서는 이득이 작거나 없으며 검증 강도는 유지된다. 기존 `PromoteRequested` 이후 자동 DB 롤백 금지와 일반 이전·명시적 rollback 경로의 구분은 변경하지 않는다.
