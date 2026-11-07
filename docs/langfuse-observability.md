# 앱별 Langfuse 조회 연결

앱 관측의 Langfuse 화면은 실제 generation 표본을 읽는다. Langfuse 설치나 HyperDX의 AI HTTP span만으로 generation·토큰·비용이 생기지는 않는다. 계측이 없는 값은 미측정으로 남는다.

서버에서 `PLATFORM_LANGFUSE_CONFIG`를 아래 형태의 운영자 관리 파일 경로로 설정한다. 예시는 배포 설정이 아니며 키 값 자체를 넣지 않는다.

```json
{
  "baseUrl": "http://langfuse-web.langfuse.svc.cluster.local:3000",
  "uiBaseUrl": "https://your-langfuse-console.example",
  "apps": {
    "moimroom-mig": {
      "projectId": "your-existing-project-id",
      "metadataKey": "hopcharlie.app",
      "publicKeyFile": "/var/run/langfuse/project-public-key",
      "secretKeyFile": "/var/run/langfuse/project-secret-key"
    }
  }
}
```

각 generation의 지정 metadata 키 값이 앱명과 정확히 일치해야 한다. 이 예시에서는 `hopcharlie.app=moimroom-mig`다. trace의 metadata에만 값이 있고 generation에 전파되지 않으면 조회 범위로 인정하지 않는다. 앱과 다른 프로젝트를 연결하거나 범위가 없는 관측을 추정으로 포함하지 않는다.

파일은 서버만 읽는다. `uiBaseUrl`은 선택 사항이며 없으면 외부 trace 링크를 만들지 않는다. project key는 읽기 권한이 확인된 기존 키를 mounted file로 공급한다. 이 변경은 Secret·키·IAM·프로젝트 생성이나 앱 계측을 자동 수행하지 않는다. 새 설정 적용과 계측은 실제 대상과 권한 범위를 확인한 운영 작업으로 진행해야 한다.

조회는 8초·최대 8MiB·최대 100개 generation으로 제한한다. 표본의 토큰·비용 합계와 평균 지연은 전체 기간의 전체 요청/과금 집계가 아니다. 한 행이라도 지표가 없으면 해당 요약은 null이다. `unobserved`는 이 범위의 표본을 관측하지 못한 상태이며 AI를 사용하지 않는다는 증거가 아니다. Langfuse 수집 지연도 있을 수 있으므로 기간과 수집 시각을 함께 확인한다.

[ADR0055](adr/0055-app-scoped-llm-observability.md)에 경계와 검증 범위를 기록했다.
