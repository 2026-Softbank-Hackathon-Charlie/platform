import importlib.util
import pathlib
import os
import sys
import unittest
from unittest.mock import patch

HERE = pathlib.Path(__file__).parent
spec = importlib.util.spec_from_file_location("rehearsal", HERE / "rehearsal.py")
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)


class Boundaries(unittest.TestCase):
    def test_loopback_only(self):
        self.assertEqual(r.loopback("http://127.0.0.1:12345"), "http://127.0.0.1:12345")
        for bad in ["http://0.0.0.0:3", "http://example.com:3", "http://127.0.0.1:3/path", "http://u@127.0.0.1:3", "http://127.0.0.1:3?x=y"]:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                r.loopback(bad)

    def test_drain_needs_same_instance_and_successful_old_id(self):
        owner = {"pid": 12, "created": "123", "executable": "gateway.exe"}
        state = {"route": "laptop", "active": {"gcp": 0}, "requests": [{"requestId": "old", "backend": "gcp", "status": 200, "outcome": "complete", "finishedAt": "2026-11-07T00:00:02Z"}]}
        r.assert_drain(owner, dict(owner), state, "old")
        for changed in [dict(owner, pid=13), dict(owner, created="other")]:
            with self.assertRaises(ValueError):
                r.assert_drain(owner, changed, state, "old")
        for changed in [dict(state, requests=[]), dict(state, active={"gcp": 1}), dict(state, route="gcp")]:
            with self.assertRaises(ValueError):
                r.assert_drain(owner, owner, changed, "old")
        state["requests"][0]["outcome"] = "failed"
        with self.assertRaises(ValueError):
            r.assert_drain(owner, owner, state, "old")

    def test_overlap_requires_actual_time_intersection(self):
        old = {"startedAt": "2026-11-07T00:00:00Z", "finishedAt": "2026-11-07T00:00:02Z"}
        self.assertTrue(r.overlaps(old, {"startedAt": "2026-11-07T00:00:01Z", "finishedAt": "2026-11-07T00:00:03Z"}))
        self.assertFalse(r.overlaps(old, {"startedAt": "2026-11-07T00:00:02Z", "finishedAt": "2026-11-07T00:00:03Z"}))

    def test_owner_checks_all_fields(self):
        good = {"pid": 12, "created": "123", "executable": "ollama.exe"}
        r.check_owner(good, dict(good))
        for key in good:
            with self.assertRaises(ValueError):
                r.check_owner(good, dict(good, **{key: "changed"}))

    @unittest.skipUnless(os.name == "nt", "Windows kernel job ownership")
    def test_owned_job_cleanup_after_graceful_exit(self):
        child = r.OwnedProcess([sys.executable, "-c", "pass"])
        child.p.wait(timeout=5)
        child.close()
        self.assertIsNone(child.job)

    @unittest.skipUnless(os.name == "nt", "Windows kernel job ownership")
    def test_live_identity_mismatch_forbids_cleanup(self):
        child = r.OwnedProcess([sys.executable, "-c", "import time;time.sleep(10)"])
        try:
            with patch.object(child, "identity", return_value=dict(child.owner, created="wrong")):
                with self.assertRaises(ValueError):
                    child.close()
            self.assertIsNone(child.p.poll())
        finally:
            child.close()


if __name__ == "__main__":
    unittest.main()
