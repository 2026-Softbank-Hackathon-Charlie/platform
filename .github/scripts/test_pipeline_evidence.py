"""The public evidence must never copy untrusted runner output."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("pipeline-evidence.py")
SCENARIOS = ("lost-response", "canary-fault", "observe-rollback", "migrate")


class PipelineEvidenceTests(unittest.TestCase):
    def summarize(self, content, exit_code=0, suite="migration"):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "private.log"
            output = Path(directory) / "summary.json"
            log.write_text(content, encoding="utf-8")
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "--log", str(log),
                 "--output", str(output), "--exit-code", str(exit_code), "--suite", suite],
                capture_output=True, text=True, check=False)
            self.assertTrue(output.exists(), result.stderr)
            return result.returncode, json.loads(output.read_text(encoding="utf-8"))

    def successful_log(self):
        lines = []
        for name in SCENARIOS:
            lines += [f"== scenario {name} (under load)",
                      "12:34:56 FAULT injected on the target app: fixture only",
                      "client writes  ok=42 failed=0",
                      "write retries  first attempt failed=2, completed by a retry=2, retry attempts=2",
                      "== E2E passed"]
        return "\n".join(lines + ["== E2E passed"])

    def test_complete_execution_keeps_only_structured_values(self):
        code, summary = self.summarize(self.successful_log())
        self.assertEqual(code, 0)
        self.assertTrue(summary["pipeline_passed"])
        self.assertEqual(len(summary["scenarios"]), 4)
        self.assertEqual(summary["scenarios"][0]["writes_ok"], 42)

    def test_secrets_and_arbitrary_errors_are_never_exported(self):
        secret = "fixture-password-do-not-export"
        code, summary = self.summarize(
            self.successful_log() + f"\nFAIL: postgres://user:{secret}@localhost/db\n"
            f"Authorization: Bearer {secret}\n::warning::{secret}", 1)
        self.assertNotEqual(code, 0)
        self.assertNotIn(secret, json.dumps(summary))
        self.assertEqual(summary["failure_assertions"], 1)

    def test_success_exit_without_all_scenarios_is_failure(self):
        code, summary = self.summarize("== scenario migrate (under load)\n== E2E passed\n")
        self.assertNotEqual(code, 0)
        self.assertFalse(summary["pipeline_passed"])

    def test_nonzero_exit_cannot_be_hidden_by_pass_markers(self):
        code, summary = self.summarize(self.successful_log(), 1)
        self.assertNotEqual(code, 0)
        self.assertFalse(summary["pipeline_passed"])

    def test_oversized_output_fails_closed_even_after_pass_markers(self):
        code, summary = self.summarize(self.successful_log() + "\n" + "x" * (32 * 1024 * 1024))
        self.assertNotEqual(code, 0)
        self.assertTrue(summary["output_truncated"])
        self.assertFalse(summary["pipeline_passed"])

    def test_remote_minio_requires_each_real_scenario_and_final_marker(self):
        names = ("forward_copy", "reverse_copy_cache_reuse", "same_size_source_change",
                 "target_corruption_detected", "target_corruption_repaired", "object_add_delete",
                 "authorization_rechecked", "metadata_rechecked", "cancelled_no_upload",
                 "fixture_bucket_cleanup")
        log = "\n".join(f"FILEGUARD_MINIO_SCENARIO {name} passed" for name in names)
        code, summary = self.summarize(log + "\nFILEGUARD_MINIO_RESULT passed\n", suite="minio")
        self.assertEqual(code, 0)
        self.assertTrue(summary["pipeline_passed"])
        self.assertEqual([s["name"] for s in summary["scenarios"]], list(names))
        code, summary = self.summarize(log, suite="minio")
        self.assertNotEqual(code, 0)
        self.assertFalse(summary["pipeline_passed"])
        incomplete = log.replace("FILEGUARD_MINIO_SCENARIO metadata_rechecked passed", "")
        code, summary = self.summarize(incomplete + "\nFILEGUARD_MINIO_RESULT passed\n", suite="minio")
        self.assertNotEqual(code, 0)
        self.assertFalse(summary["pipeline_passed"])

    def test_remote_minio_skip_is_not_remote_success(self):
        code, summary = self.summarize("--- SKIP: TestDisposableMinIOFileguard (0.00s)\nPASS\n", suite="minio")
        self.assertNotEqual(code, 0)
        self.assertFalse(summary["pipeline_passed"])


if __name__ == "__main__":
    unittest.main()
