# 보호 경로와 증거 범위

현재 실행 도구는 **로컬 시험**이다. 실제 `moimroom` AI handler → 검증된 TLS → 실제 platform gateway → GCP SSH forward / laptop loopback을 사용한다. 브라우저에 gateway/admin token을 전달하지 않는다. 앱의 store는 synthetic public SPACE-1이며 DB·예약·실제 배포 ingress 검증에 해당하지 않는다.

1. `rehearsal.py`는 두 모듈의 전용 `TestAIRehearsalHost`만 실행한다. 기본 `go test`에서는 `AI_REHEARSAL_HOST=1` 없으면 skip한다. 두 host 모두 `httptest`의 ephemeral loopback listener와 180초 상한을 사용한다.
2. gateway와 app host는 노트북에 있고 source VM 경로에 의존하지 않는다. API→gateway는 실행 중 생성된 private test certificate를 trust root로 검증한다. 미신뢰 root 연결 실패·잘못된 bearer 401·다른 inference 경로 404를 실제 검사한다. 테스트 전용 private key는 파일로 저장하지 않는다.
3. 두 random bearer는 Python 메모리와 자식 환경에만 전달한다. credential/private key/raw prompt/model output은 결과 파일에 저장하지 않는다. 운영 증거에는 ID·선택 backend·시각·status·PID·executable·생성시각만 남긴다.
4. LIVE source 경로는 기존 SSH key, `BatchMode=yes`, `StrictHostKeyChecking=yes`, `ExitOnForwardFailure=yes`를 사용한다. listener는 `127.0.0.1`뿐이다. source IP는 그 실행의 read-only VM 조회 결과를 명시적으로 전달한다. 신규 key 등록·known_hosts 자동 신뢰·공용 포트·IAM 변경은 하지 않는다.
5. 실행 후 `operations-{fake,live}-results.json.network`는 `scope:local-test`, `deploymentVerified:false`, `runtimeImageDigest:null`이다. 이 결과로 배포용 `network-contract.schema.json`을 채우거나 통과했다고 주장하면 안 된다. 배포용 image digest/고정 resource/TLS/보호 ingress는 별도 실제 발견이 필요하다.

기존 VM 예산 KRW200000 및 native DELETE 2026-11-08 23:59 KST는 그대로다. source systemd 서비스 종료는 이 도구의 동작이 아니다.
