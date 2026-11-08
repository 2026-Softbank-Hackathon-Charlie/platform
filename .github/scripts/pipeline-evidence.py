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
MAX_OUTPUT_BYTES = 32 * 1024 * 1024
MINIO_SCENARIOS = ("forward_copy", "reverse_copy_cache_reuse", "same_size_source_change",
                   "target_corruption_detected", "target_corruption_repaired", "object_add_delete",
                   "authorization_rechecked", "metadata_rechecked", "cancelled_no_upload",
                   "fixture_bucket_cleanup")


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


def summarize_minio(raw, exit_code):
    scenarios = []
    final_markers = 0
    for line in raw.decode("utf-8", errors="replace").splitlines():
        match = re.fullmatch(r"FILEGUARD_MINIO_SCENARIO ([a-z_]+) passed", line)
        if match and match[1] in MINIO_SCENARIOS:
            scenarios.append({"name": match[1], "passed": True})
        if line == "FILEGUARD_MINIO_RESULT passed":
            final_markers += 1
    passed = (exit_code == 0 and final_markers == 1
              and [s["name"] for s in scenarios] == list(MINIO_SCENARIOS))
    return {"schema_version": 1, "pipeline_passed": passed,
            "exit_code": exit_code, "scenarios": scenarios,
            "private_output_sha256": hashlib.sha256(raw).hexdigest(),
            "unverified": ["database and file migration in a single run",
                           "moimroom application migration", "public-cloud endpoints",
                           "production rollout"]}


def main():
    parser = argparse.ArgumentParser()
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--root", type=Path)
    source.add_argument("--log", type=Path, help="parse a local test fixture only")
    parser.add_argument("--exit-code", type=int, default=0)
    parser.add_argument("--suite", choices=("migration", "minio"), default="migration")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    started = datetime.now(timezone.utc).isoformat()
    if args.root:
        environment = os.environ.copy()
        environment.pop("KEEP", None)
        environment["SCENARIOS"] = " ".join(SCENARIOS)
        # Never pipe child output to Actions: failure tails can contain DSNs.
        with tempfile.TemporaryFile() as private_log:
            command = (["bash", "test/e2e/run.sh"] if args.suite == "migration" else
                       ["go", "test", "-race", "-count=1", "-v", "-timeout", "10m",
                        "-run", "^TestDisposableMinIOFileguard$", "./internal/fileguard"])
            result = subprocess.run(command, cwd=args.root,
                                    env=environment, stdout=private_log,
                                    stderr=subprocess.STDOUT, check=False)
            private_log.seek(0)
            raw = private_log.read(MAX_OUTPUT_BYTES + 1)
        exit_code = result.returncode
    else:
        with args.log.open("rb") as private_log:
            raw = private_log.read(MAX_OUTPUT_BYTES + 1)
        exit_code = args.exit_code
    truncated = len(raw) > MAX_OUTPUT_BYTES
    summarize_suite = summarize if args.suite == "migration" else summarize_minio
    summary = summarize_suite(raw[:MAX_OUTPUT_BYTES], exit_code)
    summary["suite"] = args.suite
    if args.suite == "minio":
        version = os.environ.get("RCLONE_TESTED_VERSION", "")
        if re.fullmatch(r"[0-9a-zA-Z.+-]+", version):
            summary["rclone_version"] = version
    summary["output_truncated"] = truncated
    summary["private_output_hash_scope"] = "first_32_MiB" if truncated else "complete"
    if truncated:
        summary["pipeline_passed"] = False
        summary["unverified"].append("output exceeded 32 MiB evidence limit")
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
