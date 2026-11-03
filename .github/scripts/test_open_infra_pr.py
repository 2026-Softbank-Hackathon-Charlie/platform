import base64
import copy
import importlib.util
import json
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("open_infra_pr", Path(__file__).with_name("open-infra-pr.py"))
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
    "GITHUB_SHA": SHA,
    "GITHUB_REPOSITORY": "example/platform",
    "GITHUB_RUN_ID": "42",
    "GITHUB_RUN_ATTEMPT": "1",
}


class FakeAPI:
    def __init__(self, image=OLD, fail=None):
        self.values = {"images": {"platform": image, "console": "keep-console"}, "deploy": {"builder": "codebuild"}}
        name, digest = image.split("@")
        self.lock = {"schema_version": 1, "images": {name: digest, "keep:tag": "keep-digest"}}
        self.calls = []
        self.fail = fail

    def __call__(self, method, endpoint, payload=None):
        self.calls.append((method, endpoint, copy.deepcopy(payload)))
        if self.fail == (method, endpoint.rsplit("/", 1)[-1]):
            raise RuntimeError("API failed")
        if method == "GET":
            if endpoint.endswith("git/ref/heads/main"):
                return {"object": {"sha": "main-sha"}}
            if endpoint.endswith("git/commits/main-sha"):
                return {"tree": {"sha": "base-tree"}}
            for path, data in [(MODULE.VALUES_PATH, self.values), (MODULE.LOCK_PATH, self.lock)]:
                if endpoint.endswith(f"contents/{path}?ref=main-sha"):
                    return {"content": base64.b64encode(json.dumps(data).encode()).decode()}
        elif method == "POST":
            return {"sha": "created-sha", "html_url": "https://github.com/example/infra/pull/7"}
        raise AssertionError(f"Unexpected API request: {method} {endpoint}")

    @property
    def writes(self):
        return [c for c in self.calls if c[0] != "GET"]


class InfraPRTests(unittest.TestCase):
    def test_updates_only_the_platform_pin_in_one_commit_on_a_new_branch(self):
        api = FakeAPI()
        original_values, original_lock = copy.deepcopy(api.values), copy.deepcopy(api.lock)
        self.assertEqual(MODULE.create_pull_request(ENV, api), "https://github.com/example/infra/pull/7")
        self.assertEqual([c[1].split("/git/")[-1] for c in api.writes[:3]], ["trees", "commits", "refs"])
        tree = api.writes[0][2]
        self.assertEqual(tree["base_tree"], "base-tree")
        changes = {item["path"]: json.loads(item["content"]) for item in tree["tree"]}
        expected_values = copy.deepcopy(original_values)
        expected_values["images"]["platform"] = IMAGE
        self.assertEqual(changes[MODULE.VALUES_PATH], expected_values)
        expected_lock = copy.deepcopy(original_lock)
        del expected_lock["images"][OLD.split("@")[0]]
        expected_lock["images"][IMAGE.split("@")[0]] = IMAGE.split("@")[1]
        self.assertEqual(changes[MODULE.LOCK_PATH], expected_lock)
        self.assertEqual(api.writes[1][2]["parents"], ["main-sha"])
        self.assertEqual(api.writes[2][2], {"ref": "refs/heads/ci/platform-image-1234567-42-1", "sha": "created-sha"})
        pull = api.writes[3][2]
        self.assertEqual((pull["base"], pull["head"]), ("main", "ci/platform-image-1234567-42-1"))
        self.assertIn(SHA, pull["body"])
        self.assertFalse(any("refs/heads/main" in json.dumps(payload) for _, _, payload in api.writes))

    def test_written_files_keep_the_infra_json_layout(self):
        api = FakeAPI()
        MODULE.create_pull_request(ENV, api)
        for item in api.writes[0][2]["tree"]:
            self.assertTrue(item["content"].endswith("}\n"))
            self.assertIn('\n  "', item["content"])

    def test_already_current_image_writes_nothing(self):
        api = FakeAPI(IMAGE)
        self.assertIsNone(MODULE.create_pull_request(ENV, api))
        self.assertEqual(api.writes, [])

    def test_bad_input_fails_before_any_api_call(self):
        for change in [{"IMAGE": IMAGE.replace("sha256:", "md5:")}, {"GITHUB_SHA": "f" * 40}, {"GITHUB_RUN_ID": "42\nmain"}, {"GH_TOKEN": ""}]:
            with self.subTest(change=change):
                api = FakeAPI()
                with self.assertRaises(ValueError):
                    MODULE.create_pull_request({**ENV, **change}, api)
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
                    MODULE.create_pull_request(ENV, api)
                self.assertEqual(api.writes, [])

    def test_api_failure_stops_before_the_branch_or_pr(self):
        api = FakeAPI(fail=("POST", "commits"))
        with self.assertRaises(RuntimeError):
            MODULE.create_pull_request(ENV, api)
        self.assertEqual([c[1].rsplit("/", 1)[-1] for c in api.writes], ["trees", "commits"])


if __name__ == "__main__":
    unittest.main()
