"""Exercise run() through its actual route side-effect boundary."""
import copy
import datetime
import importlib.util
import pathlib
import ssl
import tempfile
import types
import unittest
import urllib.error
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location("rehearsal", pathlib.Path(__file__).with_name("rehearsal.py"))
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)


class RouteReached(RuntimeError):
    pass


class ReadinessRoute(unittest.TestCase):
    def test_current_probe_reads_native_binding_and_live_owner_twice(self):
        owner = {"pid": 12, "created": "123", "executable": "ollama.exe"}
        paths = {"url": "http://127.0.0.1:11435", "models": "models"}
        native = [(200, {"version": "0.40.14"}, {}), (200, {"models": [{"name": r.MODEL, "digest": r.MANIFEST}]}, {}), (200, {"models": [{"name": r.MODEL, "size_vram": 1}]}, {})]
        for name, responses, owners in [("valid", native, [owner, owner]), ("replaced-during-read", native, [owner, dict(owner, created="new")]), ("bad-version", [(200, {"version": "other"}, {})] + native[1:], [owner, owner]), ("bad-full-manifest", [native[0], (200, {"models": [{"name": r.MODEL, "digest": r.MANIFEST[:12]}]}, {}), native[2]], [owner, owner]), ("not-resident", native[:2] + [(200, {"models": []}, {})], [owner, owner])]:
            with self.subTest(name=name), patch.object(r, "request", side_effect=responses) as read, patch.object(r, "fixture_sha256", return_value="fixture"):
                identity = Mock(side_effect=owners)
                if name == "valid":
                    observed = r.current_runtime(paths["url"], pathlib.Path("unused"), identity, lambda: paths)
                    self.assertEqual(observed["fixtureSha256"], "fixture")
                    self.assertEqual(identity.call_count, 2)
                    self.assertEqual([call.args[0].split("/")[-1] for call in read.call_args_list], ["version", "tags", "ps"])
                    self.assertTrue(all(call.kwargs.get("method", "GET") == "GET" for call in read.call_args_list))
                else:
                    with self.assertRaises(ValueError):
                        r.current_runtime(paths["url"], pathlib.Path("unused"), identity, lambda: paths)

    def test_owned_live_identity_rejects_stopped_process_even_when_handle_identity_remains(self):
        child = object.__new__(r.OwnedProcess)
        child.p = Mock()
        child.owner = {"pid": 12, "created": "123", "executable": "ollama.exe"}
        child.identity = Mock(return_value=child.owner)
        for polls in ([0], [None, 0], [None, None]):
            child.p.poll.side_effect = polls
            if polls == [None, None]:
                self.assertEqual(child.live_identity(), child.owner)
            else:
                with self.assertRaises(ValueError):
                    child.live_identity()

    def test_fixture_hash_binds_actual_app_prompt_and_native_options(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / "internal/api/ai_brief.go"
            source.parent.mkdir(parents=True)
            source.write_text('const aiSystem = "original"', encoding="utf-8")
            first = r.fixture_sha256(root)
            source.write_text('const aiSystem = "changed"', encoding="utf-8")
            self.assertNotEqual(first, r.fixture_sha256(root))

    def test_actual_run_rejects_invalid_target_before_route(self):
        now = datetime.datetime.now(datetime.timezone.utc)
        owner = {"pid": 12, "created": "123", "executable": "ollama.exe"}
        current = {"owner": owner, "paths": {"url": "http://127.0.0.1:11435", "executable": "ollama.exe", "models": "models"}, "model": r.MODEL, "manifest": r.MANIFEST, "version": "0.40.14", "fixtureSha256": "fixture", "gpuResident": True}
        good = dict(current, ready=True, warmGeneration=True, observedUtc=now.isoformat(), generationSeconds=.1, outputRunes=100)
        cases = [("fresh", good, current, 1), ("sub-clock-tick-generation", dict(good, generationSeconds=0), current, 1), ("missing", None, current, 0)]
        for name, changes in [
            ("not-ready", {"ready": False}), ("health-only", {"warmGeneration": False}),
            ("expired", {"observedUtc": (now - datetime.timedelta(seconds=61)).isoformat()}),
            ("future", {"observedUtc": (now + datetime.timedelta(seconds=30)).isoformat()}),
            ("missing-time", {"observedUtc": None}), ("missing-owner", {"owner": {}}),
        ]:
            cases.append((name, dict(good, **changes), current, 0))
        for field, value in [("owner", dict(owner, pid=13)), ("owner", dict(owner, created="replaced")), ("owner", dict(owner, executable="other.exe")), ("paths", dict(current["paths"], models="other")), ("paths", dict(current["paths"], url="http://127.0.0.1:9999")), ("model", "other"), ("manifest", "0" * 64), ("version", "other"), ("fixtureSha256", "other"), ("gpuResident", False)]:
            cases.append(("current-" + field + "-" + str(value), good, dict(current, **{field: value}), 0))
        cases.append(("stopped", good, ValueError("owned runtime stopped"), 0))
        for name, proof, observed, expected_calls in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                for relative in ("platform/internal/gateway/ai_demo.go", "app/internal/api/ai_brief.go"):
                    path = root / relative
                    path.parent.mkdir(parents=True)
                    path.write_text("fixture", encoding="utf-8")
                args = types.SimpleNamespace(platform=str(root / "platform"), app=str(root / "app"), output=str(root / "output"), mode="fake")
                fake = Mock(urls={"gcp": "http://127.0.0.1:1234", "laptop": "http://127.0.0.1:11435"}, servers=[])
                host = Mock(owner=owner)
                host.p.poll.return_value = 0
                host.ready.side_effect = [{"url": "https://127.0.0.1:1236", "admin": "http://127.0.0.1:1237", "rootPEM": "", "pid": 22}, {"url": "http://127.0.0.1:1238", "pid": 23}]
                routes = []
                def request(url, method="GET", data=None, token=None, request_id=None, context=None, timeout=14):
                    if url.endswith("/v1/route"):
                        routes.append(data)
                        raise RouteReached("actual route POST reached")
                    if url.endswith("/v1/ai/requests"):
                        return (401, {}, {}) if token == "wrong" else (200, {"active": {"gcp": 1}, "requests": [{"requestId": "old-001"}]}, {})
                    if url.endswith("/api/tags"):
                        return 404, {}, {}
                    if url.endswith("/api/chat"):
                        if token == "wrong":
                            return 401, {}, {}
                        raise urllib.error.URLError(ssl.SSLCertVerificationError("untrusted"))
                    return (429, {}, {}) if request_id == "capacity-002" else (200, {}, {})
                probe = Mock(side_effect=observed) if isinstance(observed, Exception) else Mock(return_value=copy.deepcopy(observed))
                with patch.object(r, "build_hosts", return_value=["gateway", "app"]), patch.object(r, "OwnedProcess", return_value=host), patch.object(r, "FakeUpstreams", return_value=fake), patch.object(r, "warm", return_value=copy.deepcopy(proof)), patch.object(r, "current_runtime", probe, create=True), patch.object(r, "request", side_effect=request), patch.object(r, "released", return_value=True), patch.object(r.ssl, "create_default_context", return_value=object()):
                    with self.assertRaises((RouteReached, ValueError)):
                        r.run(args)
                self.assertEqual(len(routes), expected_calls)


if __name__ == "__main__":
    unittest.main()
