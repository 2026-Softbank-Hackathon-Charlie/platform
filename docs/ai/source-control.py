"""Identity-bound control of the pre-existing GCP Ollama unit. No installation.

snapshot is read-only. stop/start require a separate explicit operation approval.
Rehearsal never invokes stop/start. VM lifetime and budget are untouched.
"""
import argparse
import datetime
import json
import os
import pathlib
import subprocess
import sys

UNIT = "hopcharlie-ollama.service"
EXE = "/opt/hopcharlie-gpu-poc/runtime/bin/ollama"


def properties():
    raw = subprocess.check_output(["systemctl", "show", UNIT, "--property=MainPID,ExecStart,User,ActiveState,InvocationID,KillMode"], text=True, timeout=5)
    p = dict(line.split("=", 1) for line in raw.splitlines())
    if p.get("User") != "hopcharlie-ollama" or "path=" + EXE + " ;" not in p.get("ExecStart", "") or "argv[]=" + EXE + " serve ;" not in p.get("ExecStart", "") or p.get("KillMode") != "control-group":
        raise ValueError("existing unit identity/configuration changed")
    return p


def snapshot():
    p = properties()
    pid = int(p["MainPID"])
    if p["ActiveState"] != "active" or pid <= 0 or os.readlink(f"/proc/{pid}/exe") != EXE:
        raise ValueError("fixed source runtime not active or executable changed")
    tail = pathlib.Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
    ticks = int(tail[19])
    boot = next(int(line.split()[1]) for line in pathlib.Path("/proc/stat").read_text().splitlines() if line.startswith("btime "))
    started = datetime.datetime.fromtimestamp(boot + ticks / os.sysconf("SC_CLK_TCK"), datetime.timezone.utc).isoformat()
    # Re-read unit identity to reject replacement during /proc inspection.
    after = properties()
    if any(p.get(k) != after.get(k) for k in ("MainPID", "InvocationID", "ActiveState")):
        raise ValueError("source owner changed during snapshot")
    return {"pid": pid, "startedUtc": started, "executable": EXE, "unit": UNIT, "invocationId": p["InvocationID"]}


def stop(expected, evidence, approved=False):
    if not approved:
        raise ValueError("explicit source-service operation approval required")
    if snapshot() != expected:
        raise ValueError("source owner changed; stop forbidden")
    state = evidence.get("drain", {})
    owner = evidence.get("gatewayOwner", {})
    records = state.get("requests", [])
    old = [x for x in records if x.get("backend") == "gcp"]
    successful_app_ids = {x.get("requestId") for x in evidence.get("appResults", []) if x.get("backend") == "gcp" and x.get("status") == 200}
    if evidence.get("status") != "PASS" or evidence.get("mode") != "live" or evidence.get("actualOverlap") is not True or not owner.get("created") or state.get("route") != "laptop" or state.get("active", {}).get("gcp") != 0 or not old or any(not x.get("requestId") or x["requestId"] not in successful_app_ids or x.get("status") != 200 or x.get("outcome") != "complete" or not x.get("finishedAt") for x in old):
        raise ValueError("live same-instance successful source drain evidence required")
    # This operation must be run immediately after freshly checking live ledger;
    # a persisted rehearsal file alone cannot authorize shutting down a service.
    if evidence.get("freshDrainConfirmedByOperator") is not True:
        raise ValueError("fresh live drain confirmation required")
    subprocess.run(["systemctl", "stop", UNIT], check=True, timeout=20)
    after = properties()
    if after["ActiveState"] != "inactive" or int(after["MainPID"]) != 0:
        raise ValueError("source stop not proven")
    return {"unit": UNIT, "status": "stopped", "previousOwner": expected}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("snapshot", "stop", "start"))
    parser.add_argument("--approved-source-service-operation", action="store_true")
    args = parser.parse_args()
    if args.action == "snapshot":
        value = snapshot()
    elif args.action == "stop":
        data = json.load(sys.stdin)
        value = stop(data["owner"], data["evidence"], args.approved_source_service_operation)
    else:
        if not args.approved_source_service_operation:
            raise ValueError("explicit source-service operation approval required")
        p = properties()
        if p["ActiveState"] != "inactive" or int(p["MainPID"]) != 0:
            raise ValueError("start requires this fixed unit to be inactive")
        subprocess.run(["systemctl", "start", UNIT], check=True, timeout=20)
        value = snapshot()
    print(json.dumps(value))


if __name__ == "__main__":
    main()
