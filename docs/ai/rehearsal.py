"""Bounded loopback rehearsal using the real app handler and real gateway. Stdlib only."""
import argparse
import base64
import concurrent.futures
import ctypes
from ctypes import wintypes
import datetime
import hashlib
import http.server
import ipaddress
import json
import os
import pathlib
import queue
import re
import secrets
import socket
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

MODEL = "qwen2.5:1.5b-instruct-q4_K_M"
MANIFEST = "65ec06548149b04c096a120e4a6da9d4017ea809c91734ea5631e89f96ddc57b"
GUIDE = "요약: 조용한 모임 공간입니다.\n시설: 화이트보드가 있습니다.\n주의: 실내에서는 조용히 이용해 주세요.\n[SPACE-1]"


def loopback(value):
    u = urllib.parse.urlsplit(value)
    if u.scheme != "http" or u.hostname != "127.0.0.1" or not u.port or u.username or u.path or u.query or u.fragment or "?" in value:
        raise ValueError("exact HTTP loopback base URL required")
    return value


def check_owner(expected, current):
    if expected != current or any(not expected.get(k) for k in ("pid", "created", "executable")):
        raise ValueError("process identity changed; cleanup forbidden")


def assert_drain(start_owner, current_owner, state, old_id):
    check_owner(start_owner, current_owner)
    old = next((x for x in state["requests"] if x["requestId"] == old_id), {})
    if state["route"] != "laptop" or state["active"].get("gcp") != 0 or old.get("backend") != "gcp" or old.get("status") != 200 or old.get("outcome") != "complete" or not old.get("finishedAt"):
        raise ValueError("same-instance successful old request drain not proven")


def overlaps(a, b):
    dt = lambda s: datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
    return max(dt(a["startedAt"]), dt(b["startedAt"])) < min(dt(a["finishedAt"]), dt(b["finishedAt"]))


def request(url, method="GET", data=None, token=None, request_id=None, context=None, timeout=14):
    headers = {}
    if token:
        headers["Authorization"] = "Bearer " + token
    if request_id:
        headers["X-Request-Id"] = request_id
        headers["X-Hop-Backend"] = "client-forged"
    if data is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(data, ensure_ascii=False).encode()
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=context) as response:
            body = response.read(65537)
            if len(body) > 65536:
                raise ValueError("oversize response")
            return response.status, json.loads(body) if body else {}, dict(response.headers)
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read(65536)), dict(error.headers)


def wait_until(fn, seconds=15):
    until = time.monotonic() + seconds
    while True:
        try:
            value = fn()
            if value:
                return value
        except (OSError, urllib.error.URLError):
            pass
        if time.monotonic() >= until:
            raise TimeoutError("bounded readiness wait expired")
        time.sleep(.01)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def released(port):
    with socket.socket() as s:
        return s.connect_ex(("127.0.0.1", port)) != 0


class OwnedProcess:
    """Windows kernel handles bind ownership to the created process, not a reused PID.

    Assign a kill-on-close job before inference can spawn Ollama runner children.
    """
    def __init__(self, args, env=None, cwd=None):
        if os.name != "nt":
            raise ValueError("this owned native-runtime driver requires Windows")
        self.p = subprocess.Popen(args, env=env, cwd=cwd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8", errors="replace", creationflags=subprocess.CREATE_NO_WINDOW)
        self.k = ctypes.WinDLL("kernel32", use_last_error=True)
        self.k.CreateJobObjectW.restype = wintypes.HANDLE
        self.k.CreateJobObjectW.argtypes = [ctypes.c_void_p, wintypes.LPCWSTR]
        self.k.SetInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
        self.k.AssignProcessToJobObject.argtypes = [wintypes.HANDLE, wintypes.HANDLE]
        self.k.CloseHandle.argtypes = [wintypes.HANDLE]
        self.k.GetProcessTimes.argtypes = [wintypes.HANDLE] + [ctypes.c_void_p] * 4
        self.k.QueryFullProcessImageNameW.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.LPWSTR, ctypes.POINTER(wintypes.DWORD)]
        self.job = self.k.CreateJobObjectW(None, None)
        # JOBOBJECT_EXTENDED_LIMIT_INFORMATION is 144 bytes on 64-bit Windows;
        # LimitFlags is at offset 16, including in the 32-bit layout.
        info = ctypes.create_string_buffer(144)
        ctypes.c_uint32.from_buffer(info, 16).value = 0x2000
        if not self.job or not self.k.SetInformationJobObject(self.job, 9, info, len(info)) or not self.k.AssignProcessToJobObject(self.job, wintypes.HANDLE(int(self.p._handle))):
            self.p.terminate()
            self.p.wait(timeout=5)
            if self.job:
                self.k.CloseHandle(self.job)
            raise OSError(ctypes.get_last_error(), "owned job setup failed")
        self.owner = self.identity()
        self.lines = queue.Queue()
        def read():
            for line in self.p.stdout:
                # Only explicit readiness metadata leaves the child; no prompts/tokens.
                if line.startswith("AI_HOST "):
                    self.lines.put(json.loads(line[8:]))
        self.reader = threading.Thread(target=read, daemon=True)
        self.reader.start()

    def identity(self):
        created, exited, kernel, user = (ctypes.c_uint64() for _ in range(4))
        handle = wintypes.HANDLE(int(self.p._handle))
        path, size = ctypes.create_unicode_buffer(32768), wintypes.DWORD(32768)
        if not self.k.GetProcessTimes(handle, ctypes.byref(created), ctypes.byref(exited), ctypes.byref(kernel), ctypes.byref(user)) or not self.k.QueryFullProcessImageNameW(handle, 0, path, ctypes.byref(size)):
            raise OSError("process identity unavailable")
        return {"pid": self.p.pid, "created": str(created.value), "executable": path.value}

    def ready(self):
        return self.lines.get(timeout=20)

    def live_identity(self):
        if self.p.poll() is not None:
            raise ValueError("owned runtime stopped; warm again before routing")
        current = self.identity()
        check_owner(self.owner, current)
        if self.p.poll() is not None:
            raise ValueError("owned runtime stopped during identity probe")
        return current

    def close(self):
        if self.job:
            # A reaped process cannot be queried for its executable. The still-
            # owned kernel job is safe to close; it never targets a reused PID.
            if self.p.poll() is None:
                check_owner(self.owner, self.identity())
            self.k.CloseHandle(self.job)
            self.job = None
        self.p.wait(timeout=8)
        self.reader.join(timeout=2)
        self.p.stdout.close()


class FakeUpstreams:
    def __init__(self):
        self.release = threading.Event()
        self.hits = {"gcp": {}, "laptop": {}}
        self.servers = []
        self.bad_auth = False
        self.started = str(time.monotonic_ns())
        self.threads = []
        for backend in ("gcp", "laptop"):
            outer = self
            def handler(name):
                class Handler(http.server.BaseHTTPRequestHandler):
                    def log_message(self, *_):
                        pass
                    def do_GET(self):
                        bodies = {"/api/version": {"version": "0.40.14"}, "/api/tags": {"models": [{"name": MODEL, "digest": MANIFEST}]}, "/api/ps": {"models": [{"name": MODEL, "size_vram": 1}]}}
                        self.send_response(200 if self.path in bodies else 404)
                        self.send_header("Content-Type", "application/json")
                        self.end_headers()
                        self.wfile.write(json.dumps(bodies.get(self.path, {})).encode())
                    def do_POST(self):
                        rid = self.headers.get("X-Request-Id", "")
                        outer.hits[name][rid] = outer.hits[name].get(rid, 0) + 1
                        outer.bad_auth |= bool(self.headers.get("Authorization"))
                        payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                        if payload["model"] != MODEL or "[SPACE-1]" not in payload["messages"][1]["content"]:
                            self.send_error(400)
                            return
                        if name == "gcp" and rid == "old-001":
                            outer.release.wait(10)
                        status = 502 if rid == "no-retry-004" else 200
                        self.send_response(status)
                        self.send_header("Content-Type", "application/json")
                        self.send_header("X-Hop-Backend", "forged-upstream")
                        self.end_headers()
                        self.wfile.write(json.dumps({"message": {"content": GUIDE}, "done": True}).encode())
                return Handler
            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler(backend))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            self.threads.append(thread)
            self.servers.append(server)
        self.urls = dict(zip(("gcp", "laptop"), ("http://127.0.0.1:" + str(s.server_port) for s in self.servers)))

    def identity(self):
        if not all(t.is_alive() for t in self.threads) or any(s.fileno() < 0 for s in self.servers):
            raise ValueError("controlled fake runtime stopped")
        return {"pid": os.getpid(), "created": self.started, "executable": str(pathlib.Path(sys.executable).resolve())}

    def close(self):
        self.release.set()
        for s in self.servers:
            s.shutdown()
            s.server_close()


def build_hosts(platform, app, output):
    paths = []
    for root, package in ((platform, "gateway"), (app, "api")):
        target = root / "bin" / ("ai-rehearsal-" + package + ".exe")
        target.parent.mkdir(exist_ok=True)
        env = dict(os.environ, GOCACHE=str(platform / "bin" / "ai-go-cache"))
        command = ["go", "test", "-buildvcs=false", "-c", "-o", str(target), "./internal/" + package]
        result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True, timeout=120)
        (output / ("operations-build-" + package + ".log")).write_text(result.stdout + result.stderr, encoding="utf-8")
        if result.returncode:
            raise RuntimeError("host build failed: " + package)
        paths.append(target)
    return paths


def warm_payload(app):
    code = (app / "internal/api/ai_brief.go").read_text(encoding="utf-8")
    system = json.loads(re.search(r'const aiSystem = (".*")', code).group(1))
    data = {"description": "조용한 모임 공간입니다.", "amenities": ["화이트보드"], "rules": ["실내에서는 조용히 이용해 주세요."]}
    public = json.dumps(data, ensure_ascii=False, separators=(",", ":"))
    user = "[SPACE-1]\n공개 자료: " + public + "\n\n위 자료의 안내 네 줄을 출력하세요. 마지막 줄: [SPACE-1]"
    payload = {"model": MODEL, "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}], "stream": False, "keep_alive": "60s", "options": {"num_ctx": 2048, "num_predict": 128, "temperature": 0, "seed": 42}}
    assert len(system) + len(user) <= 2000
    return payload


def fixture_sha256(app):
    return hashlib.sha256(json.dumps(warm_payload(app), ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def current_runtime(url, app, owner_probe, paths_probe):
    # Fresh native reads are bracketed by current, alive kernel ownership reads.
    owner, paths = owner_probe(), paths_probe()
    version_status, version, _ = request(url + "/api/version", timeout=2)
    tags_status, tags, _ = request(url + "/api/tags", timeout=2)
    ps_status, ps, _ = request(url + "/api/ps", timeout=2)
    if version_status != 200 or version.get("version") != "0.40.14":
        raise ValueError("current runtime version mismatch")
    if tags_status != 200 or not any(x.get("name") == MODEL and x.get("digest") == MANIFEST for x in tags.get("models", [])):
        raise ValueError("current full model manifest mismatch")
    if ps_status != 200 or not any(x.get("name") == MODEL and x.get("size_vram", 0) > 0 for x in ps.get("models", [])):
        raise ValueError("current GPU model residency missing")
    check_owner(owner, owner_probe())
    if paths != paths_probe():
        raise ValueError("runtime path changed during probe")
    return {"owner": owner, "paths": paths, "model": MODEL, "manifest": MANIFEST, "version": version["version"], "fixtureSha256": fixture_sha256(app), "gpuResident": True}


def activate_laptop(proof, probe, admin, admin_token):
    if not isinstance(proof, dict) or proof.get("ready") is not True or proof.get("warmGeneration") is not True or not proof.get("gpuResident") or not 0 <= proof.get("generationSeconds", -1) < 12 or not 0 < proof.get("outputRunes", 0) <= 600:
        raise ValueError("successful target warm generation required before routing")
    try:
        observed = datetime.datetime.fromisoformat(proof["observedUtc"])
        if observed.tzinfo is None:
            raise ValueError("timezone missing")
    except (KeyError, TypeError, ValueError) as error:
        raise ValueError("valid target warm timestamp required") from error
    current = probe()
    check_owner(proof.get("owner", {}), current.get("owner", {}))
    for key in ("paths", "model", "manifest", "version", "fixtureSha256", "gpuResident"):
        if not proof.get(key) or proof[key] != current.get(key):
            raise ValueError("target readiness binding changed: " + key)
    age = (datetime.datetime.now(datetime.timezone.utc) - observed).total_seconds()
    if not 0 <= age <= 60:
        raise ValueError("target warm expired or future-dated; warm again before routing")
    assert request(admin + "/v1/route", "POST", {"backend": "laptop"}, admin_token)[0] == 200
    return dict(current, warmAgeSeconds=round(age, 3))


def warm(url, app, owner, paths):
    assert request(url + "/api/version")[1]["version"] == "0.40.14", "version mismatch"
    tags = request(url + "/api/tags")[1]["models"]
    assert any(x["name"] == MODEL and x["digest"] == MANIFEST for x in tags), "manifest mismatch"
    payload = warm_payload(app)
    start = time.monotonic()
    status, response, _ = request(url + "/api/chat", "POST", payload, timeout=12)
    elapsed = time.monotonic() - start
    text = response.get("message", {}).get("content", "").strip()
    assert status == 200 and response.get("done") is True and 0 < len(text) <= 600 and text.endswith("[SPACE-1]") and elapsed < 12, "actual warm generation/budget failed"
    ps = request(url + "/api/ps")[1]["models"]
    assert any(x.get("name") == MODEL and x.get("size_vram", 0) > 0 for x in ps), "GPU model residency not observed"
    return {"ready": True, "warmGeneration": True, "observedUtc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "owner": owner, "paths": paths, "fixtureSha256": hashlib.sha256(json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()).hexdigest(), "model": MODEL, "manifest": MANIFEST, "version": "0.40.14", "generationSeconds": elapsed, "outputRunes": len(text), "gpuResident": True, "scope": "synthetic public SPACE-1; actual app prompt; no raw prompt/output logging"}


def run(args):
    platform, app, output = (pathlib.Path(x).resolve() for x in (args.platform, args.app, args.output))
    output.mkdir(parents=True, exist_ok=True)
    binaries = build_hosts(platform, app, output)
    owned, ports, hosts, fake = [], [], [], None
    result = {"mode": args.mode, "scope": "local loopback real app handler + TLS real gateway; synthetic public store, no DB/booking/UI/deployment proof", "sourceServiceStopped": False, "cleanup": {}}
    admin_token, token = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    env = dict(os.environ, AI_REHEARSAL_HOST="1", AI_GATEWAY_TOKEN=token, GATEWAY_ADMIN_TOKEN=admin_token)
    runtime = forward = None
    laptop_url = "http://127.0.0.1:11435"
    try:
        if args.mode == "fake":
            fake = FakeUpstreams()
            upstreams = fake.urls
            ports.extend(s.server_port for s in fake.servers)
            target_paths = lambda: {"url": upstreams["laptop"], "runtime": "controlled-fake"}
            target_owner = fake.identity
            result["readiness"] = {"laptop": warm(upstreams["laptop"], app, target_owner(), target_paths())}
        else:
            if not args.ollama or not args.models or not args.source_host or not args.ssh_key:
                raise ValueError("live mode requires --ollama --models --source-host --ssh-key")
            ipaddress.ip_address(args.source_host)
            exe, models = pathlib.Path(args.ollama).resolve(strict=True), pathlib.Path(args.models).resolve(strict=True)
            if not released(11435):
                raise ValueError("laptop port occupied; will not adopt or kill its owner")
            runtime = OwnedProcess([str(exe), "serve"], dict(os.environ, OLLAMA_HOST="127.0.0.1:11435", OLLAMA_MODELS=str(models), OLLAMA_NUM_PARALLEL="1", OLLAMA_MAX_LOADED_MODELS="1"))
            owned.append(runtime)
            ports.append(11435)
            wait_until(lambda: request(laptop_url + "/api/version", timeout=1)[0] == 200)
            ssh = ["ssh", "-i", str(pathlib.Path(args.ssh_key).resolve(strict=True)), "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=8", "-o", "ExitOnForwardFailure=yes"]
            port = free_port()
            forward = OwnedProcess(ssh + ["-N", "-L", f"127.0.0.1:{port}:127.0.0.1:11434", "gpu_poc@" + args.source_host])
            owned.append(forward)
            ports.append(port)
            source_url = "http://127.0.0.1:" + str(port)
            wait_until(lambda: request(source_url + "/api/version", timeout=1)[0] == 200)
            # Read-only source identity uses the exact fixed service snapshot implementation.
            import shlex
            source_code = base64.b64encode((platform / "docs/ai/source-control.py").read_bytes()).decode()
            remote = "sudo -n python3 -c " + shlex.quote("import base64;exec(base64.b64decode('" + source_code + "'))") + " snapshot"
            snapshot = subprocess.run(ssh + ["gpu_poc@" + args.source_host, remote], capture_output=True, text=True, timeout=15)
            if snapshot.returncode:
                raise RuntimeError("read-only source owner snapshot failed: " + snapshot.stderr[-1200:])
            source_owner = json.loads(snapshot.stdout)
            upstreams = {"gcp": source_url, "laptop": laptop_url}
            target_paths = lambda: {"url": laptop_url, "executable": str(pathlib.Path(args.ollama).resolve(strict=True)), "models": str(pathlib.Path(args.models).resolve(strict=True))}
            target_owner = runtime.live_identity
            result["readiness"] = {"gcp": warm(source_url, app, source_owner, {"url": source_url}), "laptop": warm(laptop_url, app, target_owner(), target_paths())}
        for name, value in upstreams.items():
            env["AI_UPSTREAM_" + name] = loopback(value)
        gateway = OwnedProcess([str(binaries[0]), "-test.run=^TestAIRehearsalHost$", "-test.v", "-test.timeout=175s"], env)
        owned.append(gateway)
        gm = gateway.ready()
        hosts.append(gm)
        gateway_owner = gateway.owner
        context = ssl.create_default_context(cadata=base64.b64decode(gm["rootPEM"]).decode())
        env.update(AI_ENDPOINT=gm["url"] + "/api/chat", AI_REHEARSAL_ROOT=gm["rootPEM"])
        application = OwnedProcess([str(binaries[1]), "-test.run=^TestAIRehearsalHost$", "-test.v", "-test.timeout=175s"], env)
        owned.append(application)
        am = application.ready()
        hosts.append(am)
        for metadata in hosts:
            for key in ("url", "admin"):
                if key in metadata:
                    ports.append(urllib.parse.urlsplit(metadata[key]).port)
        admin = gm["admin"]
        state = lambda: request(admin + "/v1/ai/requests", token=admin_token)[1]
        app_call = lambda rid: request(am["url"] + "/api/spaces/1/ai-brief", "POST", request_id=rid)
        assert request(gm["url"] + "/api/chat", "POST", {}, "wrong", "auth-probe", context)[0] == 401
        assert request(admin + "/v1/ai/requests", token="wrong")[0] == 401
        assert request(gm["url"] + "/api/tags", token=token, context=context)[0] == 404
        try:
            request(gm["url"] + "/api/chat", context=ssl.create_default_context(), timeout=2)
            raise AssertionError("untrusted root accepted")
        except urllib.error.URLError as e:
            assert isinstance(e.reason, ssl.SSLCertVerificationError)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            old = pool.submit(app_call, "old-001")
            before = wait_until(lambda: (s if s["active"].get("gcp") == 1 and any(x["requestId"] == "old-001" for x in s["requests"]) else None) if (s := state()) else None, 5)
            if fake:
                assert app_call("capacity-002")[0] == 429
            result["routeReadiness"] = activate_laptop(result.get("readiness", {}).get("laptop"), lambda: current_runtime(upstreams["laptop"], app, target_owner, target_paths), admin, admin_token)
            later = pool.submit(app_call, "new-003")
            new_reply = later.result(timeout=14)
            if fake:
                assert not old.done(), "fake source was not held across target completion"
                fake.release.set()
            old_reply = old.result(timeout=14)
        summaries = []
        for rid, backend, reply in (("old-001", "gcp", old_reply), ("new-003", "laptop", new_reply)):
            status, body, headers = reply
            assert status == 200 and body["backend"] == backend and body["requestId"] == rid and body["text"].strip().endswith("[SPACE-1]"), "request-bound app result failed"
            summaries.append({"requestId": rid, "backend": body["backend"], "status": status, "durationMs": body["durationMs"], "outputRunes": len(body["text"])})
        final = wait_until(lambda: s if (s := state())["active"].get("gcp") == 0 else None, 3)
        assert_drain(gateway_owner, gateway.identity(), final, "old-001")
        records = {x["requestId"]: x for x in final["requests"]}
        actual_overlap = overlaps(records["old-001"], records["new-003"])
        if fake:
            assert actual_overlap
            assert app_call("no-retry-004")[0] == 502
            assert fake.hits == {"gcp": {"old-001": 1}, "laptop": {"": 1, "new-003": 1, "no-retry-004": 1}} and not fake.bad_auth, "retry, capacity, or auth boundary failed"
            result["fakeUpstreamHits"] = fake.hits
        else:
            forward.close()
            owned.remove(forward)
            assert released(urllib.parse.urlsplit(upstreams["gcp"]).port), "source forward still listening"
        after = app_call("isolated-005")
        assert after[0] == 200 and after[1]["backend"] == "laptop"
        result.update(status="PASS", actualOverlap=actual_overlap, observedBeforeSwitch=before, drain=final, gatewayOwner=gateway_owner, appResults=summaries, sameAppURL=am["url"] + "/api/spaces/1/ai-brief", tlsVerified=True, sourcePathIsolation=(args.mode == "live"), afterIsolation={"status": after[0], "backend": after[1]["backend"], "requestId": after[1]["requestId"]})
        result["network"] = {"scope": "local-test", "ingressAccessVerified": True, "inferenceUrl": gm["url"] + "/api/chat", "adminUrl": admin, "sourceIndependent": True, "appUrl": am["url"], "backends": upstreams, "runtimeImageDigest": None, "deploymentVerified": False}
    except Exception as error:
        result.update(status="FAIL", errorType=type(error).__name__, error=str(error))
        raise
    finally:
        if fake:
            fake.close()
        if runtime and runtime.p.poll() is None:
            try:
                check_owner(runtime.owner, runtime.identity())
                request(laptop_url + "/api/generate", "POST", {"model": MODEL, "keep_alive": 0}, timeout=5)
                result["cleanup"]["ownedModelUnload"] = True
            except Exception:
                result["cleanup"]["ownedModelUnload"] = False
        errors = []
        for process in reversed(owned):
            try:
                # Graceful shutdown for test hosts, followed by owned job cleanup.
                meta = next((m for m in hosts if m["pid"] == process.p.pid), None)
                if meta and process.p.poll() is None:
                    check_owner(process.owner, process.identity())
                    request(meta.get("admin", meta["url"]) + "/__rehearsal_stop", "POST", token=admin_token, timeout=2)
                    process.p.wait(timeout=5)
                process.close()
            except Exception as error:
                errors.append(type(error).__name__ + ": " + str(error))
        result["cleanup"].update(errors=errors, ports={str(port): released(port) for port in ports}, processesExited=all(p.p.poll() is not None for p in owned))
        if errors or not all(result["cleanup"]["ports"].values()) or not result["cleanup"]["processesExited"]:
            result["status"] = "FAIL"
        result["sourceHashes"] = {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest() for root in (platform, app) for path in ([root / "internal/gateway/ai_demo.go"] if root == platform else [root / "internal/api/ai_brief.go"])}
        (output / ("operations-" + args.mode + "-results.json")).write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    if result["status"] != "PASS":
        raise RuntimeError("rehearsal cleanup verification failed")
    print(json.dumps({"status": result["status"], "mode": args.mode, "actualOverlap": result["actualOverlap"], "sourcePathIsolation": result["sourcePathIsolation"], "results": str(output / ("operations-" + args.mode + "-results.json"))}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("fake", "live"), default="fake")
    for name in ("platform", "app", "output"):
        parser.add_argument("--" + name, required=True)
    for name in ("ollama", "models", "source-host", "ssh-key"):
        parser.add_argument("--" + name)
    run(parser.parse_args())
