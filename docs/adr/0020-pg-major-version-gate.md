# 0020. 이전 시작 전에 원본과 대상의 PostgreSQL 메이저 버전을 비교한다

- 상태: 채택
- 날짜: 2026-11-04

## 상황

엔진은 CNPG 물리 복제(pg_basebackup과 스트리밍)로 이전한다([migration-engine ADR 0001](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/blob/main/docs/adr/0001-cnpg-distributed-topology.md)). 물리 복제는 같은 메이저 버전 사이에서만 된다. 원본 17.11에서 대상 17.x는 되지만, 대상 18.6은 안 된다.

대상 manifest에 `spec.imageName`이 없으면 CNPG operator의 기본 이미지가 쓰인다. 이 기본값은 operator 버전마다 다르고, 최근 버전은 PostgreSQL 18 계열이다(우리 클러스터의 1.30.1이 정확히 어떤 이미지를 쓰는지는 확인하지 않았다). 그래서 원본이 17인데 이미지를 적지 않은 Plan은 이렇게 실패한다.

1. 시작한다.
2. 엔진이 대상 Cluster를 만든다.
3. pg_basebackup이 실패하고 재시도를 반복한다.
4. `catchupTimeout`(기본 10분)을 기다린다.
5. `target cluster not healthy`로 실패한다.

이 단계에서는 원본 쓰기를 막지 않으므로 원본 서비스는 계속 돈다. 하지만 원인을 늦게, 그리고 흐리게 알려 준다.

AI Plan 검사(ai-sandbox)는 이미지가 CNPG 이미지 형식인지만 검사하고, AI에게 "원본을 참고하라"고 지시할 뿐 버전을 비교하지 않는다. 사람이 직접 쓴 Plan은 그 검사를 거치지도 않는다.

## 결정

1. **모든 새 run이 거치는 `plans.Service.Reserve`에서 메이저 버전을 비교한다.** 순서는 레지던시 → manifest 파일화 → 버전 → 용량이다. 버전이 다르면 용량을 잴 필요가 없다.
2. **원본은 DB에서 읽는다.** 읽기 전용 세션으로 `server_version_num`을 읽는다.
3. **대상은 DB, 그다음 manifest 순서로 읽는다.**
   - 대상 DB에 접속되면(이미 있는 replica) 그 값을 쓴다.
   - 접속되지 않으면 manifest의 `spec.imageCatalogRef.major`를 읽는다.
   - 그것도 없으면 `spec.imageName` 태그의 앞 숫자를 읽는다(`17.11`, `18.6-minimal-trixie`, digest가 붙은 태그 포함).
4. **명백히 다를 때만 막는다.** 409 `version_mismatch`이고, 메시지에 양쪽 버전과 읽은 곳을 적는다. Plan은 `approved`로 돌아간다.
5. **읽지 못하면 `unknown`이고 막지 않는다. 통과로 기록하지도 않는다.** 코드로 이유를 구분한다.
   - `source_unknown`: 원본 DB를 읽지 못함
   - `image_default`: manifest에 이미지가 없어 operator 기본값이 버전을 정함
   - `image_unparsed`: `latest`처럼 태그에 버전이 없음
   - `target_unknown`: 대상 DB도 manifest도 없음

   감사 로그 `pgVersion`에 status, code, 양쪽 버전이 남는다.
6. **기본으로 켠다.** `PLATFORM_VERSION_GATE=off`로 끈다. 레지던시·용량과 달리 판정이 단순하고 오탐 여지가 거의 없다. DB 두 개에 한 번씩만 접속하며, 상한은 10초다.
7. **롤백은 면제한다.** 다른 게이트와 같은 이유다. 롤백하는 두 클러스터는 이미 복제 중이라 메이저 버전이 같다.

## 검토한 대안

- **operator 기본 이미지를 클러스터에서 조회해 `image_default`도 판정한다.** 그러려면 환경마다 operator Deployment를 읽는 권한과 코드가 필요하다. 지금은 unknown으로 두고, 메시지에 `spec.imageName`을 적으라고 안내한다.
- **이미지 없는 manifest를 거부한다.** 더 안전하지만 기존 Plan과 예시를 깨고, 대상이 이미 있어 manifest가 필요 없는 경우까지 막는다.
- **엔진이 prepare에서 검사한다.** 엔진을 직접 쓰는 CLI 경로도 보호되지만, 엔진은 run이 생긴 뒤에 돈다. 시작 요청에 바로 답하려면 플랫폼에서 해야 한다. 엔진 쪽 검사는 별도로 추가할 수 있다.

## 결과

- 메이저가 다른 Plan은 run이 생기기 전에 몇 초 안에 거절된다. 원인이 메시지에 바로 나온다.
- `image_default`는 막지 않으므로, 이미지를 적지 않은 Plan은 여전히 10분 뒤에 실패할 수 있다. 감사 로그의 unknown과 메시지로 그 위험을 알 수 있다.
- 확장(extension)의 버전 차이와 마이너 버전 차이는 검사하지 않는다. 물리 복제는 마이너 버전 차이를 허용한다.
- 원본 DB에 접속하지 못하는 환경(플랫폼에서 DSN에 닿지 않는 경우)에서는 항상 unknown이다.
