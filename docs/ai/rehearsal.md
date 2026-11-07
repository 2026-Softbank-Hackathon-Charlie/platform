# 한 명령 로컬 전환 시험

workspace root의 PowerShell에서 실행한다. Python 표준 라이브러리와 이미 설치된 Go/SSH만 사용한다. 제품 코드·빌드 설정·dependencies는 변경하지 않는다. Go test binary는 기존 ignored `bin/`, Go cache는 platform `bin/ai-go-cache`를 사용한다.

```powershell
python .worktrees/gpu-ai-platform/docs/ai/rehearsal.py --mode fake --platform .worktrees/gpu-ai-platform --app .worktrees/gpu-ai-moimroom --output artifacts/gpu-ai-demo-plan-2026-11-06
```

fake mode는 실제 app handler와 TLS gateway를 실행한다. 두 HTTP fake native upstream 중 source 응답만 channel로 붙잡아 실제 old/new overlap을 보장한다. target fake generation과 simulated version/tags/VRAM metadata를 사용해 실제 readiness guard도 통과한다. 동일 app URL·requestId·gateway가 고정한 backend·selected-backend 429·backend 실패 시 POST 재시도 없음·bearer 제거를 검사한다. fake의 VRAM/owner는 simulation이며 GPU 실행 증거가 아니다.

LIVE 실행 전 기존 VM을 read-only 조회해 RUNNING/현재 IP와 native DELETE가 유지되는지 확인한다. 다음 IP는 그 결과로 대입한다. SSH known host/key가 없거나 GPU warm이 실패하면 중단한다.

```powershell
python .worktrees/gpu-ai-platform/docs/ai/rehearsal.py --mode live --platform .worktrees/gpu-ai-platform --app .worktrees/gpu-ai-moimroom --output artifacts/gpu-ai-demo-plan-2026-11-06 --ollama artifacts/gpu-experiment-runtime/ollama-0.40.14-windows/ollama.exe --models artifacts/gpu-experiment-runtime/models --source-host <CURRENT_VM_IP> --ssh-key C:/Users/ADMIN/.ssh/google_compute_engine
```

1. 새 laptop runtime과 소유 SSH source loopback forward를 시작하고 두 runtime을 실제 app prompt로 순차 warm한다. protected-path 인증/TLS 검사를 통과한다.
2. old-001을 같은 app URL로 시작한다. 같은 실행 gateway ledger에서 `route=gcp`, `active.gcp=1`, old ID 선택을 실제 관측한다. route POST 직전 target의 성공 warm이 60초 이내이고 현재 살아 있는 owned runtime·경로·전체 model manifest/version·app prompt/fixture binding이 같아야 route=laptop으로 바꾼다. 불일치는 route 변경 전에 안전하게 실패한다. 세부 조건은 [runtime-runbook.md](runtime-runbook.md)를 따른다. new-003은 같은 URL을 사용한다.
3. 두 앱 응답의 backend·ID·성공을 확인하고, 동일 process identity와 같은 ledger의 old 완료/route=laptop/active.gcp=0을 검사한다. 실제 ledger 시각이 교차해야 `actualOverlap:true`다. 시간이 교차하지 않았다면 live 기능이 성공해도 무중단 overlap 증거로 쓰면 안 된다.
4. source 서비스는 그대로 둔 채 **소유한 SSH forward만** 닫는다. source port 해제를 확인하고 isolated-005가 laptop에서 성공하는지 확인한다. 이는 source-path isolation이며 source-service shutdown 증거가 아니다.
5. `finally`에서 자기 runtime의 모델만 unload하고 소유 프로세스/job을 정리한다. token·raw prompt/output은 저장하지 않는다. 결과는 지정한 `--output` 아래 `operations-live-results.json` 또는 `operations-fake-results.json`; build 로그는 `operations-build-{gateway,api}.log`다. 이전 증거를 보존하려면 새로운 output 하위 폴더를 지정한다. `routeReadiness`는 route 직전 current probe와 warm age를 기록한다. 실패도 구조화해 남기며 cleanup 확인에 실패하면 최종 FAIL이다.

focused self-check:

```powershell
python -m unittest discover -s .worktrees/gpu-ai-platform/docs/ai -p test_*.py -v
```

기본 host가 skip하는지는 빌드된 각 test binary를 `-test.run=^TestAIRehearsalHost$ -test.v`로 실행하면 확인할 수 있다. gate env를 설정하지 않아야 한다. UI build·이미 승인된 gateway/API 회귀 결과·알려진 Windows GCC32 race 제약은 소스가 그대로이면 기존 증거를 재사용한다. 이 시험은 public deployment, production DB/booking, UI browser, arbitrary fault zero-loss, GPU-memory migration, 전체 데이터 on-prem 상태를 증명하지 않는다.
