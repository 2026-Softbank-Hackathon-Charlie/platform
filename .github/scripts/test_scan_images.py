import base64
import datetime
import importlib.util
import json
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("scan_images", Path(__file__).with_name("scan-images.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

D1, D2, D3 = "sha256:" + "1" * 64, "sha256:" + "2" * 64, "sha256:" + "3" * 64
ECR = "525091405886.dkr.ecr.ap-northeast-2.amazonaws.com/hopcharlie/apps/shop-web"
AR = "asia-northeast3-docker.pkg.dev/p/hopcharlie-apps/shop-web"
NOW = datetime.datetime(2026, 11, 6, 12, 0, tzinfo=datetime.timezone.utc)


def running(image, digest, env="eks"):
    return {"environment": env, "namespace": "shop", "workload": "Deployment/web", "container": "web", "image": image, "digest": digest, "scanState": "not_scanned"}


class ScanImagesTest(unittest.TestCase):
    def test_repository(self):
        self.assertEqual(MODULE.repository(ECR + ":abc1234@" + D1), ECR)
        self.assertEqual(MODULE.repository("localhost:5000/app:v1"), "localhost:5000/app")
        self.assertEqual(MODULE.repository("nginx:1.27"), "nginx")
        self.assertEqual(MODULE.registry("nginx"), "docker.io")
        self.assertEqual(MODULE.registry(ECR), ECR.split("/")[0])

    def test_targets_skip_fresh_scans_and_prefer_ecr(self):
        report = {
            "runningImages": [running(AR + ":v1", D1, "gke"), running(ECR + ":v1", D1), running(ECR + ":v2", D2),
                              running("quay.io/x/y:v1", D3), {"image": "no-digest:v1"}],
            "scans": [
                {"digest": D2, "status": "succeeded", "finishedAt": "2026-11-06T02:00:00Z"},  # 10h old: fresh
                {"digest": D3, "status": "failed", "finishedAt": "2026-11-06T11:30:00Z"},     # failed 30 min ago: wait
            ],
        }
        self.assertEqual(MODULE.targets(report, NOW), {D1: [ECR + "@" + D1, AR + "@" + D1]})
        report["scans"][0]["finishedAt"] = "2026-11-05T11:00:00Z"  # 25h old: rescan
        report["scans"][1]["finishedAt"] = "2026-11-06T10:00:00Z"  # failed 2h ago: retry
        self.assertEqual(sorted(MODULE.targets(report, NOW)), [D1, D2, D3])

    def test_docker_config_only_ecr_of_the_region(self):
        hosts = {ECR.split("/")[0], "602401143452.dkr.ecr.ap-northeast-2.amazonaws.com", "1.dkr.ecr.us-east-1.amazonaws.com", "docker.io"}
        cfg = MODULE.docker_config(hosts, "pw", "ap-northeast-2")
        self.assertEqual(sorted(cfg["auths"]), ["525091405886.dkr.ecr.ap-northeast-2.amazonaws.com", "602401143452.dkr.ecr.ap-northeast-2.amazonaws.com"])
        self.assertEqual(base64.b64decode(cfg["auths"][ECR.split("/")[0]]["auth"]), b"AWS:pw")
        self.assertEqual(MODULE.docker_config(hosts, "", "ap-northeast-2"), {"auths": {}})

    def test_scan_body_keeps_the_report_bytes(self):
        report = b'{"matches": [],  "descriptor": {"name": "grype", "version": "0.121.0"}}'
        body = MODULE.scan_body(ECR + "@" + D1, D1, "succeeded", "a", "b", "0.121.0", report=report)
        self.assertIn(report, body)
        self.assertEqual(json.loads(body)["report"]["descriptor"]["name"], "grype")
        failed = json.loads(MODULE.scan_body(ECR + "@" + D1, D1, "failed", "a", "b", "", message="denied"))
        self.assertEqual((failed["status"], failed["message"], "report" in failed, "scannerVersion" in failed), ("failed", "denied", False, False))


if __name__ == "__main__":
    unittest.main()
