# 기존 runtime의 소유권과 fresh readiness

준비된 Ollama 0.40.14와 기존 models 디렉터리를 사용한다. 설치·다운로드·전역 환경설정 변경은 필요 없다. 실행 명령은 [rehearsal.md](rehearsal.md)를 따른다.

1. LIVE mode는 11435가 비어 있는지 먼저 확인한다. 다른 listener가 있으면 중단한다. 자기 `ollama serve`에만 `OLLAMA_HOST=127.0.0.1:11435`, `OLLAMA_MODELS`, `OLLAMA_NUM_PARALLEL=1`, `OLLAMA_MAX_LOADED_MODELS=1`을 전달한다.
2. 소유권은 PID뿐 아니라 Windows kernel process handle·생성 FILETIME·실제 executable로 확인한다. inference 전에 kill-on-close Job Object에 소유 프로세스를 넣어 해당 runner 자식만 정리한다. 이미 종료된 프로세스에는 executable 재조회를 요구하지 않고 소유 job handle을 닫는다. 다른 Ollama/SSH/Go 프로세스를 열거해 종료하지 않는다.
3. GCP의 현재 owner는 SSH로 `source-control.py snapshot`을 실행해 systemd unit·User·ExecStart·KillMode·InvocationID·PID·proc executable·시작시각을 읽는다. unit을 두 번 읽어 도중 교체를 거부한다. 기존 discovery JSON의 PID를 readiness로 재사용하지 않는다.
4. 각 backend에 `/api/version`, `/api/tags`의 전체 manifest `65ec06548149b04c096a120e4a6da9d4017ea809c91734ea5631e89f96ddc57b`를 확인한다. 현재 app의 실제 system prompt와 synthetic public SPACE-1을 사용해 native `/api/chat`을 한 번 생성한다. 12초 미만·done·1~600자·최종 자료 ID와 `/api/ps`의 양수 VRAM을 확인해야 fresh ready다. source와 laptop warm은 순차다. 이 단일 샘플은 서비스 SLO나 전체 품질 보증이 아니다.
5. route POST 직전에 성공한 target warm generation이 있고 관측 나이가 0~60초인지 확인한다. native read-only `/api/version`, `/api/tags`, `/api/ps`를 현재 살아 있는 owned runtime의 PID·생성 FILETIME·executable 재조회 사이에서 확인한다. 실행파일·models의 현재 resolved path, loopback URL, model·version·전체 manifest, 현재 app system prompt와 SPACE-1 fixture/native options의 SHA256가 warm 결과와 모두 같아야 route를 변경한다. missing/stale/future/종료·교체/path·model·fixture 변경은 route POST 전에 실패하며 source route를 유지한다. health/tags 성공만으로 warm을 대신하지 않으며 자동 inference 재시도는 없다. 실패 후 새 실행에서 실제 warm을 다시 수행한다.

정리 시 살아 있는 owned laptop의 identity를 재확인하고 해당 runtime에서만 `keep_alive:0`으로 모델을 unload한다. 소유 job만 종료하고 모든 owned port가 풀렸는지 검사한다. unload 실패도 결과에 기록한다. 강제 종료 후에는 readiness가 만료되며 다음 실행에서 다시 warm해야 한다.

## 별도 승인이 필요한 source 서비스 제어

`source-control.py snapshot`은 read-only다. `stop/start`는 별도 명시 승인 후 기존 GCP VM에서만 사용한다. 이번 rehearsal은 호출하지 않는다. Stop은 정확히 일치하는 owner, LIVE 성공 ledger, 실제 overlap, route=laptop/active.gcp=0, source 완료 ID, operator의 fresh drain 확인을 요구한다. 파일 하나만으로 현재 gateway drain을 증명할 수 없으므로 operator가 현재 같은 gateway instance를 다시 확인해야 한다.

승인된 stop 입력은 stdin의 `{"owner": <fresh snapshot>, "evidence": <live result plus freshDrainConfirmedByOperator:true>}`이고 명령은 `sudo -n python3 source-control.py stop --approved-source-service-operation`이다. 현재 이 명령의 실행은 승인되지 않았다. 재가동도 `start --approved-source-service-operation`으로만 가능하며 exact unit inactive/PID0 검사를 거친다. 서비스 identity와 drift 거부는 단위 시험했고 실제 stop/start는 검증하지 않았다.

rollback 순서: source 경로 복구 → fixed source unit이 실행 중인지 snapshot → 실제 warm 성공 → gateway route=gcp → 진행 중 laptop ID 완료/active.laptop=0 확인. source 서비스를 이미 멈췄다면 별도 승인된 start와 warm이 앞서야 한다. VM 삭제/TTL 변경은 어떤 명령에도 포함하지 않는다.
