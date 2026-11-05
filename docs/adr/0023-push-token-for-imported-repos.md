# 0023. 들여온 오픈소스 사본에만 저장소 하나, 1시간짜리 push 토큰을 발급한다

- 상태: 채택
- 날짜: 2026-11-05

## 상황

팀은 오픈소스 앱을 플랫폼에 배포한다. 순서는 이렇다.

1. `scripts/import-repo.sh`가 공개 저장소를 조직 `2026-Softbank-Hackathon-Charlie`에 private 사본으로 들여오고, 플랫폼 GitHub App(`hopcharlie-platform`, 선택한 저장소만 보는 설치)에 더한다.
2. AI 에이전트(ai-sandbox 저장소의 `hopai`, agent 역할 토큰으로 플랫폼에 인증)가 사본에 Dockerfile과 `platform.yaml`을 더하고 `hopcharlie` 브랜치로 push한다.
3. 운영자가 `charlie app add --branch hopcharlie`로 등록한다.

2단계에는 GitHub 쓰기 자격 증명이 필요하다. 제약은 이렇다.

- 에이전트는 신뢰할 수 없는 오픈소스 코드를 읽으므로 프롬프트 인젝션에 노출된다. 자격 증명이 새어 나가도 닿는 곳이 그 사본 하나이고, 길어야 1시간이어야 한다.
- 오래 사는 비밀을 새로 만들어 보관하고 싶지 않다. 플랫폼에는 이미 App 개인 키가 있다.
- 플랫폼이 캐시해서 쓰는 설치 토큰은 범위가 좁혀져 있지 않다. 설치가 보는 모든 저장소(`gitops` 포함)의 contents에 쓸 수 있으므로 밖으로 내주면 안 된다.

## 결정

### API

- `POST /v1/github/push-tokens` `{repo}`(agent, operator)를 둔다. 201로 `{repo, token, expiresAt}`을 돌려준다.
- 플랫폼은 App JWT로 `POST /app/installations/{id}/access_tokens`를 본문 `{"repositories":["<name>"],"permissions":{"contents":"write"}}`로 불러 새 설치 토큰을 만든다. GitHub이 토큰을 그 저장소 하나와 contents 쓰기로 좁히고 1시간 뒤에 만료시킨다. `expiresAt`은 GitHub이 정한 값을 그대로 돌려준다.
- 부를 때마다 새로 만들고 캐시하지 않는다. 플랫폼 자신이 쓰는 캐시된 설치 토큰은 쓰지도 바꾸지도 않는다. 설치 id를 찾는 코드만 함께 쓴다.
- App 권한은 이미 contents 쓰기(gitops 커밋)이므로 App 설정은 바꾸지 않는다. 좁힌 토큰의 권한은 App 권한 안에서만 정할 수 있다.

### 발급 조건

차례로 확인한다.

1. `repo`가 `owner/name`이 아니면 400 `invalid_request`다.
2. GitHub App이 설정되어 있지 않으면 503 `github_not_configured`다.
3. gitops 저장소(`PLATFORM_GITOPS_REPO`, 기본 `2026-Softbank-Hackathon-Charlie/gitops`, 대소문자 무시)는 토픽이 있어도 언제나 403 `repo_not_imported`이고, 메시지로 gitops 저장소라서 거절한다고 알린다.
4. 플랫폼 설치 토큰으로 `GET /repos/{owner}/{name}`을 읽는다. 404이면 404 `repo_not_found`다(없거나 설치가 볼 수 없음).
5. 토픽 `hopcharlie-imported`가 없으면 403 `repo_not_imported`이고, 메시지로 `scripts/import-repo.sh`로 들여오라고 안내한다.
6. owner가 플랫폼의 조직(`PLATFORM_GITHUB_ORG`)과 다르면(대소문자 무시) 토큰을 만들지 않고 403 `repo_not_imported`다. 토큰 요청의 `repositories`는 설치 계정 안의 이름만 받으므로, 이 확인이 없으면 다른 owner의 같은 이름 저장소에 토픽을 붙여 조직 저장소의 토큰을 받을 수 있다.
7. GitHub 오류는 502 `github_unreachable`이다.

`scripts/import-repo.sh`는 저장소를 만든 바로 뒤에 `gh repo edit "$repo" --add-topic hopcharlie-imported`로 토픽을 붙인다.

### 토큰을 남기지 않는다

- 응답에 `Cache-Control: no-store`를 붙인다.
- 감사 로그에는 `repo`와 `expiresAt`만 남긴다. 감사 항목은 라우트가 기록하는 경로, 상태 코드와 핸들러가 넣은 note뿐이고 응답 본문은 기록하지 않는다. 서버에는 요청이나 응답 본문을 남기는 로깅 미들웨어가 없다.
- GitHub 오류 로그에는 GitHub의 상태 코드와 메시지만 들어가고 토큰은 들어가지 않는다.

### 브랜치 규칙의 한계

에이전트가 `hopcharlie` 브랜치에만 push한다는 규칙은 hopai가 지키고, GitHub은 막지 않는다. 조직이 Free 플랜이고 사본이 private이라서 branch protection과 ruleset을 쓸 수 없고, 설치 토큰의 권한은 브랜치 단위로 좁힐 수 없다. 그래서 토큰은 그 사본의 어느 브랜치(기본 브랜치 포함)에나 쓸 수 있다.

이 한계를 받아들이는 이유는 이렇다.

- 배포는 운영자가 등록한 브랜치만 따라간다(ADR 0011, 0015). 다른 브랜치에 push해도 배포되지 않는다. 등록한 `hopcharlie` 브랜치를 쓰는 것은 원래 에이전트에게 맡긴 일이다.
- 피해 범위가 들여온 사본 하나다. 사본은 upstream에서 다시 받거나 지우고 다시 들여오면 된다. 팀 저장소, gitops, infra에는 닿지 않는다.
- 토큰은 1시간 뒤에 만료된다.

## 검토한 대안

- 저장소마다 deploy key: 오래 사는 SSH 키를 저장소마다 만들어 보관하고 에이전트에게 건네야 한다. 새면 지울 때까지 유효하고, 쓰기 키는 어느 브랜치에나 push한다.
- fine-grained PAT: 사람 계정에 묶이고, 사본을 들여올 때마다 그 사람이 토큰의 저장소 목록에 손으로 더해야 한다.
- 운영자의 classic PAT(`import-repo.sh`의 `INSTALL_TOKEN`): `repo` 범위라서 그 사람이 쓸 수 있는 모든 저장소에 쓴다. 에이전트에게 주면 platform, infra, gitops가 모두 열린다.
- 설치를 "All repositories"로 바꾸기: 들여올 때 설치에 더하는 단계는 없어진다. 대신 플랫폼이 캐시하는 범위 없는 설치 토큰(빌드의 git clone과 gitops 커밋에 쓴다)이 조직의 모든 저장소, infra와 platform의 main까지 쓸 수 있게 된다.

## 결과

- 새 비밀이 생기지 않는다. 이미 있는 App 개인 키로 만든다.
- 이 결정 전에 들여온 사본은 `gh repo edit <org>/<name> --add-topic hopcharlie-imported`로 토픽을 붙여야 토큰을 받는다.
- 발급 여부는 토픽이 정한다. 그래서 팀 저장소(platform, infra, ai-sandbox 등)에는 이 토픽을 붙이지 않는다. gitops만 토픽과 관계없이 거절한다.
- hopai는 agent 토큰으로 이 API를 부르고, 받은 토큰은 git 자격 증명으로만 쓰며 로그와 대화 기록에 남기지 않아야 한다. 만료되면 다시 받는다.
- 감사 로그로 누가 언제 어느 사본의 토큰을 받았는지 볼 수 있다.
