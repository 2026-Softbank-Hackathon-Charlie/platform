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

- `POST /v1/github/push-tokens` `{repo}`(agent, operator)를 둔다. 201로 `{repo, token, expiresAt}`을 돌려준다. `repo`는 GitHub이 알려 준 지금 이름이다.
- 플랫폼은 App JWT로 `POST /app/installations/{id}/access_tokens`를 본문 `{"repository_ids":[<id>],"permissions":{"contents":"write"}}`로 불러 새 설치 토큰을 만든다. GitHub이 토큰을 그 저장소 하나와 contents 쓰기로 좁히고 1시간 뒤에 만료시킨다. `expiresAt`은 GitHub이 정한 값을 그대로 돌려준다.
- 부를 때마다 새로 만들고 캐시하지 않는다. 플랫폼 자신이 쓰는 캐시된 설치 토큰은 쓰지도 바꾸지도 않는다. 설치 id를 찾는 코드만 함께 쓴다.
- App 권한은 이미 contents 쓰기(gitops 커밋)이므로 App 설정은 바꾸지 않는다. 좁힌 토큰의 권한은 App 권한 안에서만 정할 수 있다.

### 저장소를 id로 묶는다

- 플랫폼 설치 토큰으로 `GET /repos/{owner}/{name}`을 읽어 `id`, `full_name`, `owner.login`, `topics`를 얻는다. GitHub은 이름이 바뀌거나 다른 계정으로 옮긴 저장소도 따라가서 지금 이름으로 답한다.
- 조직 확인과 gitops 확인은 요청한 이름에 한 번(GitHub을 부르기 전), GitHub이 답한 지금 이름에 한 번 더 한다.
- 토큰은 이름이 아니라 그 `id`로 만든다(`repository_ids`). 확인한 저장소와 토큰이 닿는 저장소가 이름 바꾸기 사이에 달라질 수 없다.

### GitHub의 답을 확인한다

- 토큰을 만든 답의 `repository_selection`, `repositories[]`(id, full_name), `permissions`를 읽는다. `repository_selection`이 `selected`이고, 저장소가 정확히 하나이며 그 id가 요청한 id이고, 권한이 `contents: write`와 GitHub이 늘 붙이는 `metadata: read` 안에 있어야 한다.
- 하나라도 다르면 그 토큰을 그 토큰 자신으로 인증한 `DELETE /installation/token`으로 폐기하고 502 `github_unreachable`로 답한다. 메시지는 GitHub이 더 넓은 토큰으로 답했다고 알리고, 자세한 내용은 서버 로그에 남는다.

### 발급 조건

차례로 확인한다.

1. `repo`가 `owner/name`이 아니거나 이름이 `.`, `..`이거나 `.git`으로 끝나면 400 `invalid_request`다.
2. GitHub App이 설정되어 있지 않으면 503 `github_not_configured`다.
3. 요청한 이름의 owner가 플랫폼의 조직(`PLATFORM_GITHUB_ORG`)과 다르면(대소문자 무시) GitHub을 부르지 않고 403 `repo_not_imported`이고, 메시지로 조직 밖의 저장소라고 알린다. 다른 owner의 저장소는 토픽을 붙였어도 GitHub에 묻지 않는다.
4. 요청한 이름이 gitops 저장소이면 GitHub을 부르지 않고 403 `repo_not_imported`이고, 메시지로 gitops 저장소라서 거절한다고 알린다. 토픽이 있어도 거절한다.
5. 저장소를 읽는다. 404이면 404 `repo_not_found`다(없거나 설치가 볼 수 없음).
6. GitHub이 답한 지금 이름으로 3과 4를 다시 확인한다. 조직 밖으로 옮긴 저장소와 gitops로 이름을 바꾼 저장소가 여기서 걸린다.
7. 토픽 `hopcharlie-imported`가 없으면 403 `repo_not_imported`이고, 메시지로 `scripts/import-repo.sh`로 들여오라고 안내한다.
8. 토큰을 만든다. GitHub이 422로 거절하면(설치가 그 저장소를 포함하지 않음) 404 `repo_not_found`이고, 메시지로 저장소를 설치에 더하라고 안내한다. 답의 범위가 다르면 위와 같이 폐기하고 502다. 그 밖의 GitHub 오류는 502 `github_unreachable`이고 메시지는 토큰을 만들지 못했다고 알린다.
9. 감사 로그를 쓴다(아래). 쓰지 못하면 503 `audit_unavailable`이다.

`scripts/import-repo.sh`는 모든 단계(저장소 만들기, Actions 끄기, push, 설치에 더하기)가 끝난 뒤 마지막으로 `gh repo edit "$repo" --add-topic hopcharlie-imported`로 토픽을 붙인다. 그래서 토픽이 있다는 것은 들여오기가 끝났다는 뜻이다. 설치에 더하지 못해 멈추면 손으로 더한 뒤 토픽을 붙이라고 안내한다.

### gitops 확인의 범위

- gitops 확인은 설정한 `PLATFORM_GITOPS_REPO`(기본 `2026-Softbank-Hackathon-Charlie/gitops`, 대소문자 무시) 하나와만 비교한다.
- `PLATFORM_GITOPS_MODE=git`이면 이 값은 배포에 쓰이지 않고 기본값으로 남는다. 그때의 gitops 저장소나 다른 조직의 저장소는 토픽 규칙(그런 저장소에 토픽을 붙이지 않는다)과 3의 조직 확인에 기댄다.

### 토픽을 붙일 수 있는 사람

- 저장소의 토픽은 그 저장소의 관리자(admin)면 누구나 붙일 수 있다. 토픽은 서명된 표시가 아니라 조직 안의 약속이다.
- 잘못 붙인 토픽이 닿을 수 있는 범위는 설치가 선택한 저장소로 제한된다. 토큰은 설치가 포함하지 않은 저장소에는 만들어지지 않는다(422, 8). 그래서 설치에 들어 있는 저장소 목록이 실수로 토픽을 붙였을 때의 피해 범위이고, 설치를 "All repositories"로 바꾸지 않는 이유가 하나 더 생긴다.

### 토큰을 남기지 않는다

- 응답에 `Cache-Control: no-store`를 붙인다.
- 감사 로그에는 `repo`(지금 이름, 다르면 요청한 이름은 `requested`), `expiresAt`, 요청한 사람(actor)만 남기고 토큰은 남기지 않는다. 감사 항목은 경로, 상태 코드와 핸들러가 넣은 note뿐이고 응답 본문은 기록하지 않는다. 서버에는 요청이나 응답 본문을 남기는 로깅 미들웨어가 없다.
- 감사 로그는 응답보다 먼저 쓴다(ADR 0019의 시크릿 값 보기와 같다). 쓰지 못하면 토큰을 폐기하고 돌려주지 않으며 503 `audit_unavailable`로 답한다. 폐기가 실패하면 토큰 없이 로그에만 남기고, 토큰은 1시간 안에 만료된다. 먼저 쓴 감사 항목이 있으면 라우트는 같은 요청을 다시 기록하지 않는다. 역할 때문에 거절한 401, 403과 201이 아닌 다른 응답은 지금처럼 라우트가 기록한다.
- GitHub 오류 로그에는 GitHub의 상태 코드와 메시지만 들어가고 토큰은 들어가지 않는다.
- 에이전트 활동 로그(`internal/agentlog`)도 문자열 안의 GitHub 토큰(`gho_`, `ghp_`, `ghs_`, `ghu_`, `ghr_`, `github_pat_`)을 지운다. 에이전트가 토큰을 실수로 도구 출력에 남겨도 저장되지 않게 한다.

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
- 설치를 "All repositories"로 바꾸기: 들여올 때 설치에 더하는 단계는 없어진다. 대신 플랫폼이 캐시하는 범위 없는 설치 토큰(빌드의 git clone과 gitops 커밋에 쓴다)이 조직의 모든 저장소, infra와 platform의 main까지 쓸 수 있게 된다. 토픽을 잘못 붙였을 때의 피해 범위도 조직 전체가 된다.
- 토큰을 이름(`repositories: [name]`)으로 만들기: 처음 구현이 이렇게 했다. 확인한 뒤 토큰을 만들기 전에 이름이 바뀌면 다른 저장소에 토큰이 갈 수 있어서 id로 바꿨다.

## 결과

- 새 비밀이 생기지 않는다. 이미 있는 App 개인 키로 만든다.
- 이 결정 전에 들여온 사본은 `gh repo edit <org>/<name> --add-topic hopcharlie-imported`로 토픽을 붙여야 토큰을 받는다.
- 발급 여부는 토픽이 정한다. 그래서 팀 저장소(platform, infra, ai-sandbox 등)에는 이 토픽을 붙이지 않는다. 설정한 gitops만 토픽과 관계없이 거절한다.
- hopai는 agent 토큰으로 이 API를 부르고, 받은 토큰은 git 자격 증명으로만 쓰며 로그와 대화 기록에 남기지 않아야 한다. 만료되면 다시 받는다.
- 감사 로그로 누가 언제 어느 사본의 토큰을 받았는지 볼 수 있고, 감사 로그를 쓸 수 없는 동안에는 토큰이 나가지 않는다.
