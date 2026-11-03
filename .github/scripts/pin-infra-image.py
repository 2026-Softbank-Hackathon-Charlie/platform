#!/usr/bin/env python3
"""Pin a published platform image in infra main with one direct commit.

The commit changes only images.platform in the management cluster's values
and that image's images.lock.json entry (infra ADR 0025); Argo CD deploys
infra main from there. The ref update is fast-forward only: if infra main
moved since it was read, the pin starts again from the new main.
"""

import base64
import json
import os
from pathlib import Path
import re
import subprocess


VALUES_PATH = "clusters/aws-dev-management/values/hopcharlie-platform.yaml"
LOCK_PATH = "helm/images.lock.json"
ATTEMPTS = 5
IMAGE_PATTERN = re.compile(
    r"(?P<repository>[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/"
    r"[a-z0-9]+(?:[._/-][a-z0-9]+)*):(?P<tag>[0-9a-f]{7})@(?P<digest>sha256:[0-9a-f]{64})"
)


class NotFastForward(Exception):
    """infra main moved between reading it and updating it."""


def api(method, endpoint, payload=None):
    command = ["gh", "api", "--method", method, endpoint, "-H", "Accept: application/vnd.github+json"]
    if payload is not None:
        command.extend(["--input", "-"])
    try:
        result = subprocess.run(
            command, input=json.dumps(payload) if payload is not None else None,
            text=True, capture_output=True, check=True,
        )
    except subprocess.CalledProcessError as error:
        if method == "PATCH" and "HTTP 422" in (error.stderr or ""):
            raise NotFastForward(error.stderr.strip()) from error
        raise
    return json.loads(result.stdout)


def check_env(env):
    for key in ("GH_TOKEN", "INFRA_REPOSITORY", "IMAGE", "SOURCE_SHA", "GITHUB_REPOSITORY", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        if not env.get(key):
            raise ValueError(f"Missing environment variable: {key}")
    for key in ("INFRA_REPOSITORY", "GITHUB_REPOSITORY"):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", env[key]):
            raise ValueError(f"Invalid repository: {key}")
    for key in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        if not re.fullmatch(r"[1-9][0-9]*", env[key]):
            raise ValueError(f"Invalid workflow identifier: {key}")
    image = IMAGE_PATTERN.fullmatch(env["IMAGE"])
    if not re.fullmatch(r"[0-9a-f]{40}", env["SOURCE_SHA"]) or not image or image["tag"] != env["SOURCE_SHA"][:7]:
        raise ValueError("IMAGE must pin the source commit's 7-character tag and a sha256 digest")
    return image


def pin(env, request=api):
    """Return the infra commit sha, or None when infra main already pins IMAGE."""
    image = check_env(env)
    repo = f"repos/{env['INFRA_REPOSITORY']}"
    source = f"https://github.com/{env['GITHUB_REPOSITORY']}"
    message = (
        f"chore: 플랫폼 이미지를 {image['tag']}로 교체\n\n"
        f"{env['IMAGE']}\n\n"
        f"platform 커밋: {source}/commit/{env['SOURCE_SHA']}\n"
        f"빌드: {source}/actions/runs/{env['GITHUB_RUN_ID']}/attempts/{env['GITHUB_RUN_ATTEMPT']}\n"
    )
    for _ in range(ATTEMPTS):
        base = request("GET", f"{repo}/git/ref/heads/main")["object"]["sha"]
        files = {}
        for path in (VALUES_PATH, LOCK_PATH):
            content = request("GET", f"{repo}/contents/{path}?ref={base}")["content"]
            files[path] = json.loads(base64.b64decode(content).decode("utf-8"))
        values, lock = files[VALUES_PATH], files[LOCK_PATH]
        old = IMAGE_PATTERN.fullmatch(values["images"]["platform"])
        if not old or old["repository"] != image["repository"]:
            raise ValueError("The image must use the existing infra platform repository")
        old_name = f"{old['repository']}:{old['tag']}"
        if lock["images"].get(old_name) != old["digest"]:
            raise ValueError("The existing platform image and images.lock.json disagree")
        if values["images"]["platform"] == env["IMAGE"]:
            return None

        values["images"]["platform"] = env["IMAGE"]
        new_name = f"{image['repository']}:{image['tag']}"
        if new_name != old_name:
            del lock["images"][old_name]
        lock["images"][new_name] = image["digest"]
        base_tree = request("GET", f"{repo}/git/commits/{base}")["tree"]["sha"]
        tree = request("POST", f"{repo}/git/trees", {
            "base_tree": base_tree,
            "tree": [{"path": path, "mode": "100644", "type": "blob", "content": json.dumps(data, indent=2, ensure_ascii=False) + "\n"}
                     for path, data in files.items()],
        })
        commit = request("POST", f"{repo}/git/commits", {"message": message, "tree": tree["sha"], "parents": [base]})
        try:
            request("PATCH", f"{repo}/git/refs/heads/main", {"sha": commit["sha"], "force": False})
        except NotFastForward:
            continue
        return commit["sha"]
    raise RuntimeError(f"infra main moved during {ATTEMPTS} attempts; run the workflow again")


def main():
    sha = pin(os.environ)
    summary = f"## Platform image published\n\nImage: `{os.environ['IMAGE']}`\n\n"
    if sha:
        summary += f"Pinned in infra main: https://github.com/{os.environ['INFRA_REPOSITORY']}/commit/{sha}\n"
    else:
        summary += "Infra main already pins this image; nothing was committed.\n"
    summary += "\nArgo CD deploys infra main; this workflow does not check the running release.\n"
    print(summary)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a", encoding="utf-8") as output:
            output.write(summary)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.stderr.strip() or str(error)) from error
