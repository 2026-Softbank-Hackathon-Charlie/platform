---
name: api-doc
description: platform의 api/openapi.yaml을 팀 Notion "API 명세서" 페이지에 반영한다. "API 명세서 갱신해줘", "openapi 바뀐 거 Notion에 올려줘", "API 문서 최신화" 같은 요청이나 /api-doc에 쓴다. Notion MCP가 연결되어 있어야 한다.
---

# API 명세서 갱신

`tools/apidoc`(Go)가 openapi.yaml을 Notion 마크다운으로 바꾸고, 이 스킬은 그 결과를 Notion MCP로 올린다.

| 파일 | 내용 |
|---|---|
| `tools/apidoc/main.go` | 생성기 |
| `tools/apidoc/apidoc.yaml` | Notion 페이지 ID, 배포 서버 주소, 태그 순서, 엔드포인트와 스키마의 한국어 설명 |
| `tools/apidoc/template.md` | 페이지 뼈대. 개요, 다이어그램, SSE 이벤트, 오류 코드처럼 사람이 쓴 절과 `{{...}}` 생성 위치가 있다 |
| `tools/apidoc/out/` | 생성 결과. git에 넣지 않는다 |

## 순서

1. 저장소 루트에서 `git fetch origin`을 하고 `go run ./tools/apidoc`를 실행한다. origin/main의 openapi.yaml을 문서로 만든다. 다른 브랜치는 `-rev <브랜치>`로 미리 볼 수 있지만, 사용자가 따로 요청하지 않으면 팀 페이지에는 origin/main 기준만 올린다.
2. 출력의 `missing Korean text`가 none이 아니면 나열된 키를 apidoc.yaml에 한국어로 채우고 1을 다시 실행한다. `warning:` 줄이 나오면 해당 글자를 백틱으로 감싼다.
3. Notion 페이지(apidoc.yaml의 `notionPage`)를 fetch해서 두 가지를 확인한다.
   - 하위 페이지, 데이터베이스, 생성기가 만들지 않은 문단이 있는지 본다. 있으면 덮어쓰기 전에 사용자에게 묻는다. 올리면 사라진다.
   - 맨 위 표의 기준 커밋과 페이지 최신화 시각을 읽는다. 기준 커밋이 1의 출력과 같고, 그 최신화 시각 뒤로 `git log origin/main -- tools/apidoc`에 커밋이 없고, 이번에 apidoc.yaml이나 template.md를 고치지 않았으면 올릴 필요가 없다고 보고하고 끝낸다.
4. `git diff <이전 기준 커밋> <새 기준 커밋> -- api/openapi.yaml`을 읽고, template.md에서 사람이 쓴 절에 영향이 있으면 고친 뒤 1을 다시 실행한다. 엔드포인트 표, 스키마 표, enum 값 표는 생성되므로 고치지 않는다.
   - 전체 흐름 시퀀스 다이어그램: 새 엔드포인트가 이전 흐름의 한 단계가 되는지 본다.
   - 상태 다이어그램: PlanStatus, RunStatus, RunPhase의 값이나 전이 조건이 바뀌었는지 본다. 전이는 `internal/plans`, `internal/runs` 코드로 확인한다.
   - 스키마 관계도: 응답 타입이 새 타입을 포함하는지 본다.
   - SSE 이벤트 표: RunEvent 설명에 있는 kind 목록과 data 키를 본다.
   - 오류 코드 표: `components.responses` 설명과 엔드포인트 설명에 있는 code를 본다.
5. `tools/apidoc/out/chunk-01.md`부터 번호 순서대로 Notion update-page 도구로 올린다. 파일 내용을 읽어 그대로 넘기고 손으로 바꾸지 않는다.
   - chunk-01: `command: replace_content`, `new_str`에 파일 내용.
   - chunk-02부터: `command: insert_content`, `position: {"type": "end"}`, `content`에 파일 내용.
   - 모든 호출에 `allow_async: false`를 준다. 중간에 실패하면 원인을 고치고 chunk-01부터 다시 올린다.
6. apidoc.yaml이나 template.md를 고쳤으면 origin/main에서 브랜치를 새로 만들어 PR로 올린다. 다른 사람의 작업 트리를 건드리지 않도록 필요하면 `git worktree`를 쓴다.
7. 사용자에게 기준 커밋, 배포 서버 비교 결과(출력의 `deployed server`), 새로 채운 설명, PR 링크를 알린다. 배포 서버가 `DIFFERENT`면 페이지와 실제 서버의 계약이 다르다는 점을 따로 알린다.

## apidoc.yaml 작성 규칙

- 문장은 "~한다"로 끝낸다. 이모지, 화살표, 가운뎃점은 쓰지 않는다.
- openapi.yaml의 영어 설명을 옮긴다. 계약에 없는 동작은 지어내지 않고, 모호하면 `internal/` 코드로 확인한다.
- `<name>`, `${VAR}`, `$`처럼 Notion이 태그나 수식으로 읽는 글자는 백틱으로 감싼다.
- 키 규칙: 엔드포인트는 `operations.<operationId>`, 필드는 `fields.<Schema>.<field>`(중첩 필드는 `<Schema>.<parent>.<child>`), 파라미터는 `params.<name>`(엔드포인트마다 뜻이 다르면 `<name>@<operationId>`), enum 값은 `enums.<Schema>.<값>`이다.
- `role`은 openapi.yaml 설명의 Role 문구를 따른다. 인증이 없으면 `인증 없음`, 읽기 전용이면 `모든 역할`이다.
- 새 스키마는 `schemaGroups`의 알맞은 그룹에, 새 태그는 `tags`에 넣는다. 넣지 않으면 '기타' 그룹이나 태그 이름 그대로 나온다.
