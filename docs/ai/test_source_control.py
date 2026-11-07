import importlib.util
import pathlib
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("source_control", pathlib.Path(__file__).with_name("source-control.py"))
s = importlib.util.module_from_spec(spec)
spec.loader.exec_module(s)


class SourceControl(unittest.TestCase):
    def test_changed_owner_never_stops(self):
        with patch.object(s, "snapshot", return_value={"pid": 2}), patch.object(s.subprocess, "run") as run:
            with self.assertRaises(ValueError):
                s.stop({"pid": 1}, {"drainVerified": True}, approved=True)
            run.assert_not_called()

    def test_stop_requires_explicit_operation_and_complete_drain(self):
        with patch.object(s, "snapshot", return_value={"pid": 1}), patch.object(s.subprocess, "run") as run:
            for approved, evidence in [(False, {}), (True, {}), (True, {"sourceServiceStopped": False, "drain": {"route": "laptop", "active": {"gcp": 0}, "requests": []}})]:
                with self.assertRaises(ValueError):
                    s.stop({"pid": 1}, evidence, approved=approved)
            run.assert_not_called()

    def test_unit_drift_fails_closed(self):
        raw = "User=other\nExecStart=/wrong serve\nKillMode=process\n"
        with patch.object(s.subprocess, "check_output", return_value=raw):
            with self.assertRaises(ValueError):
                s.properties()

    def test_complete_approved_stop_targets_only_fixed_unit(self):
        owner = {"pid": 1}
        evidence = {"status": "PASS", "mode": "live", "actualOverlap": True,
                    "gatewayOwner": {"created": "known-instance"},
                    "appResults": [{"requestId": "old-001", "backend": "gcp", "status": 200}],
                    "freshDrainConfirmedByOperator": True,
                    "drain": {"route": "laptop", "active": {"gcp": 0},
                              "requests": [{"requestId": "old-001", "backend": "gcp", "status": 200,
                                            "outcome": "complete", "finishedAt": "now"}]}}
        with patch.object(s, "snapshot", return_value=owner), patch.object(s, "properties", return_value={"ActiveState": "inactive", "MainPID": "0"}), patch.object(s.subprocess, "run") as run:
            self.assertEqual(s.stop(owner, evidence, approved=True)["status"], "stopped")
            run.assert_called_once_with(["systemctl", "stop", "hopcharlie-ollama.service"], check=True, timeout=20)


if __name__ == "__main__":
    unittest.main()
