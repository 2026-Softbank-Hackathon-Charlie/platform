"""Run the disposable migration fixture without publishing raw logs or headers."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


SCENARIOS = ("lost-response", "canary-fault", "observe-rollback", "migrate")
STAGES = ("build", "kind cluster", "source pg-src", "platform store",
          "test apps, gateway, platform API", "second app by host name")


def summarize(raw, exit_code):
    scenarios = []
    current = None
    total_pass_markers = 0
    failures = 0
    last_stage = "not_started"
    for line in raw.decode("utf-8", errors="replace").splitlines():
        for stage in STAGES:
            if line.startswith("== " + stage):
                last_stage = stage
        match = re.fullmatch(r"== scenario ([a-z-]+) \(under load\)", line)
        if match and match[1] in SCENARIOS:
            current = {"name": match[1], "passed": False, "fault_injected": False}
            scenarios.append(current)
            last_stage = match[1]
        if line == "== E2E passed":
            total_pass_markers += 1
            if current is not None:
                current["passed"] = True
        if line.startswith("FAIL:"):
            failures += 1
        if current is None:
            continue
        if "FAULT injected on the target app:" in line:
            current["fault_injected"] = True
        match = re.match(r"client writes  ok=(\d+) failed=(\d+)(?:;|$)", line)
        if match:
            current.update(writes_ok=int(match[1]), writes_failed=int(match[2]))
        match = re.fullmatch(r"write retries  first attempt failed=(\d+), completed by a retry=(\d+), retry attempts=(\d+)", line)
        if match:
            current.update(first_attempt_failed=int(match[1]), recovered_writes=int(match[2]), retry_attempts=int(match[3]))
        match = re.match(r"run\s+\S+ status=(succeeded|rolled_back|failed|halted)\b", line)
        if match:
            current["run_status"] = match[1]
        match = re.fullmatch(r"verify\s+verified=(true|false) missingRows=(\d+) missingWrites=(\d+) checkedWrites=(\d+)", line)
        if match:
            current.update(verified=match[1] == "true", missing_rows=int(match[2]), missing_writes=int(match[3]), checked_writes=int(match[4]))
        match = re.fullmatch(r"recheck\s+(\d+) of (\d+) (?:recorded|acknowledged) writes are on the (?:new primary|source \(primary again\))", line)
        if match:
            current.update(rechecked_writes=int(match[1]), acknowledged_writes=int(match[2]))
    passed = (exit_code == 0 and failures == 0 and total_pass_markers == 5
              and [s["name"] for s in scenarios] == list(SCENARIOS)
              and all(s["passed"] for s in scenarios))
    return {"schema_version": 1, "pipeline_passed": passed,
            "exit_code": exit_code, "last_stage": last_stage,
            "scenarios": scenarios, "failure_assertions": failures,
            "private_output_sha256": hashlib.sha256(raw).hexdigest(),
            "unverified": ["cross-cluster or public-cloud networking", "rclone and MinIO file migration",
                           "moimroom application migration", "production rollout", "opt-in gateway HA"]}


def main():
    parser = argparse.ArgumentParser()
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--root", type=Path)
    source.add_argument("--log", type=Path, help="parse a local test fixture only")
    parser.add_argument("--exit-code", type=int, default=0)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    started = datetime.now(timezone.utc).isoformat()
    if args.root:
        environment = os.environ.copy()
        environment.pop("KEEP", None)
        environment["SCENARIOS"] = " ".join(SCENARIOS)
        # Never pipe child output to Actions: failure tails can contain DSNs.
        with tempfile.TemporaryFile() as private_log:
            result = subprocess.run(["bash", "test/e2e/run.sh"], cwd=args.root,
                                    env=environment, stdout=private_log,
                                    stderr=subprocess.STDOUT, check=False)
            private_log.seek(0)
            raw = private_log.read()
        exit_code = result.returncode
    else:
        raw = args.log.read_bytes()
        exit_code = args.exit_code
    summary = summarize(raw, exit_code)
    summary.update(started_at=started, completed_at=datetime.now(timezone.utc).isoformat())
    for key in ("TESTED_SHA", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        value = os.environ.get(key, "")
        if re.fullmatch(r"[0-9a-f]{40}" if key == "TESTED_SHA" else r"[0-9]+", value):
            summary[key.lower()] = value
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary))
    return 0 if summary["pipeline_passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
