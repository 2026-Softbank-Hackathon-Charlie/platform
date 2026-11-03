import base64
import copy
import importlib.util
import json
from pathlib import Path
import unittest
import unittest.mock


SPEC = importlib.util.spec_from_file_location("pin_infra_image", Path(__file__).with_name("pin-infra-image.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

REPOSITORY = "525091405886.dkr.ecr.ap-northeast-2.amazonaws.com/hopcharlie/platform"
SHA = "1234567" + "a" * 33
OLD = f"{REPOSITORY}:7654321@sha256:" + "b" * 64
IMAGE = f"{REPOSITORY}:{SHA[:7]}@sha256:" + "c" * 64
ENV = {
    "GH_TOKEN": "test-token",
    "INFRA_REPOSITORY": "example/infra",
    "IMAGE": IMAGE,
    "SOURCE_SHA": SHA,
    "GITHUB_REPOSITORY": "example/platform",
    "GITHUB_RUN_ID": "42",
    "GITHUB_RUN_ATTEMPT": "1",
}


class FakeAPI:
    """infra main at main-sha; each failed ref update moves main once."""

    def __init__(self, image=OLD, rejections=0):
        self.values = {"images": {"platform": image, "console": "keep-console"}, "deploy": {"builder": "codebuild"}}
        name, digest = image.split("@")
        self.lock = {"schema_version": 1, "images": {name: digest, "keep:tag": "keep-digest"}}
        self.calls = []
        self.rejections = rejections
        self.main = "main-sha"

    def __call__(self, method, endpoint, payload=None):
        self.calls.append((method, endpoint, copy.deepcopy(payload)))
        if method == "GET":
            if endpoint.endswith("git/ref/heads/main"):
                return {"object": {"sha": self.main}}
            if "/git/commits/" in endpoint:
                return {"tree": {"sha": "base-tree"}}
            for path, data in [(MODULE.VALUES_PATH, self.values), (MODULE.LOCK_PATH, self.lock)]:
                if f"contents/{path}?ref=" in endpoint:
                    return {"content": base64.b64encode(json.dumps(data).encode()).decode()}
        elif method == "POST":
            return {"sha": "created-sha"}
        elif method == "PATCH" and endpoint.endswith("git/refs/heads/main"):
            if self.rejections:
                self.rejections -= 1
                self.main = f"moved-{self.rejections}"
                raise MODULE.NotFastForward("Update is not a fast forward (HTTP 422)")
            return {"object": {"sha": payload["sha"]}}
        raise AssertionError(f"Unexpected API request: {method} {endpoint}")

    @property
    def writes(self):
        return [c for c in self.calls if c[0] != "GET"]


class PinTests(unittest.TestCase):
    def test_commits_only_the_platform_pin_to_main_fast_forward(self):
        api = FakeAPI()
        original_values, original_lock = copy.deepcopy(api.values), copy.deepcopy(api.lock)
        self.assertEqual(MODULE.pin(ENV, api), "created-sha")
        self.assertEqual([(m, e.rsplit("/", 1)[-1]) for m, e, _ in api.writes], [("POST", "trees"), ("POST", "commits"), ("PATCH", "main")])
        changes = {item["path"]: json.loads(item["content"]) for item in api.writes[0][2]["tree"]}
        expected_values = copy.deepcopy(original_values)
        expected_values["images"]["platform"] = IMAGE
        self.assertEqual(changes[MODULE.VALUES_PATH], expected_values)
        expected_lock = copy.deepcopy(original_lock)
        del expected_lock["images"][OLD.split("@")[0]]
        expected_lock["images"][IMAGE.split("@")[0]] = IMAGE.split("@")[1]
        self.assertEqual(changes[MODULE.LOCK_PATH], expected_lock)
        commit = api.writes[1][2]
        self.assertEqual(commit["parents"], ["main-sha"])
        self.assertTrue(commit["message"].startswith("chore: 플랫폼 이미지를 1234567로 교체\n"))
        self.assertIn(SHA, commit["message"])
        self.assertEqual(api.writes[2][2], {"sha": "created-sha", "force": False})

    def test_written_files_keep_the_infra_json_layout(self):
        api = FakeAPI()
        MODULE.pin(ENV, api)
        for item in api.writes[0][2]["tree"]:
            self.assertTrue(item["content"].endswith("}\n"))
            self.assertIn('\n  "', item["content"])

    def test_already_current_image_writes_nothing(self):
        api = FakeAPI(IMAGE)
        self.assertIsNone(MODULE.pin(ENV, api))
        self.assertEqual(api.writes, [])

    def test_moved_main_is_read_again_and_the_pin_rebuilt_on_it(self):
        api = FakeAPI(rejections=2)
        self.assertEqual(MODULE.pin(ENV, api), "created-sha")
        commits = [payload for method, endpoint, payload in api.writes if endpoint.endswith("/git/commits")]
        self.assertEqual([c["parents"] for c in commits], [["main-sha"], ["moved-1"], ["moved-0"]])

    def test_gives_up_when_main_keeps_moving(self):
        api = FakeAPI(rejections=MODULE.ATTEMPTS)
        with self.assertRaises(RuntimeError):
            MODULE.pin(ENV, api)

    def test_bad_input_fails_before_any_api_call(self):
        for change in [{"IMAGE": IMAGE.replace("sha256:", "md5:")}, {"SOURCE_SHA": "f" * 40}, {"GITHUB_RUN_ID": "42\nmain"}, {"GH_TOKEN": ""}]:
            with self.subTest(change=change):
                api = FakeAPI()
                with self.assertRaises(ValueError):
                    MODULE.pin({**ENV, **change}, api)
                self.assertEqual(api.calls, [])

    def test_wrong_repository_or_inconsistent_lock_fails_before_writes(self):
        for mutate in ["repository", "missing-lock", "wrong-digest"]:
            with self.subTest(mutate=mutate):
                api = FakeAPI()
                if mutate == "repository":
                    api.values["images"]["platform"] = OLD.replace("/platform:", "/console:")
                elif mutate == "missing-lock":
                    api.lock["images"].pop(OLD.split("@")[0])
                else:
                    api.lock["images"][OLD.split("@")[0]] = "sha256:" + "e" * 64
                with self.assertRaises(ValueError):
                    MODULE.pin(ENV, api)
                self.assertEqual(api.writes, [])


class ApiTests(unittest.TestCase):
    def test_only_a_rejected_ref_update_means_main_moved(self):
        error = MODULE.subprocess.CalledProcessError(1, ["gh"], stderr="gh: Update is not a fast forward (HTTP 422)")
        with unittest.mock.patch.object(MODULE.subprocess, "run", side_effect=error):
            with self.assertRaises(MODULE.NotFastForward):
                MODULE.api("PATCH", "repos/example/infra/git/refs/heads/main", {"sha": "x", "force": False})
            with self.assertRaises(MODULE.subprocess.CalledProcessError):
                MODULE.api("POST", "repos/example/infra/git/trees", {})


if __name__ == "__main__":
    unittest.main()
