#!/usr/bin/env python3
"""Open an infra review PR that pins a published platform image; never deploy it.

Same flow as frontend's open-console-pr.py, for images.platform: one commit on
a new branch that changes only the platform pin in the management cluster's
values and its images.lock.json entry (infra ADR 0024).
"""

import base64
import json
import os
from pathlib import Path
import re
import subprocess


VALUES_PATH = "clusters/aws-dev-management/values/hopcharlie-platform.yaml"
LOCK_PATH = "helm/images.lock.json"
IMAGE_PATTERN = re.compile(
    r"(?P<repository>[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/"
    r"[a-z0-9]+(?:[._/-][a-z0-9]+)*):(?P<tag>[0-9a-f]{7})@(?P<digest>sha256:[0-9a-f]{64})"
)


def api(method, endpoint, payload=None):
    command = ["gh", "api", "--method", method, endpoint, "-H", "Accept: application/vnd.github+json"]
    if payload is not None:
        command.extend(["--input", "-"])
    result = subprocess.run(
        command, input=json.dumps(payload) if payload is not None else None,
        text=True, capture_output=True, check=True,
    )
    return json.loads(result.stdout)


def create_pull_request(env, request=api):
    for key in ("GH_TOKEN", "INFRA_REPOSITORY", "IMAGE", "GITHUB_SHA", "GITHUB_REPOSITORY", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        if not env.get(key):
            raise ValueError(f"Missing environment variable: {key}")
    for key in ("INFRA_REPOSITORY", "GITHUB_REPOSITORY"):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", env[key]):
            raise ValueError(f"Invalid repository: {key}")
    for key in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        if not re.fullmatch(r"[1-9][0-9]*", env[key]):
            raise ValueError(f"Invalid workflow identifier: {key}")
    image = IMAGE_PATTERN.fullmatch(env["IMAGE"])
    if not re.fullmatch(r"[0-9a-f]{40}", env["GITHUB_SHA"]) or not image or image["tag"] != env["GITHUB_SHA"][:7]:
        raise ValueError("IMAGE must pin the source commit's 7-character tag and a sha256 digest")

    repo = f"repos/{env['INFRA_REPOSITORY']}"
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
    title = f"chore: 플랫폼 이미지를 {image['tag']}로 교체"
    commit = request("POST", f"{repo}/git/commits", {"message": title, "tree": tree["sha"], "parents": [base]})
    branch = f"ci/platform-image-{image['tag']}-{env['GITHUB_RUN_ID']}-{env['GITHUB_RUN_ATTEMPT']}"
    request("POST", f"{repo}/git/refs", {"ref": f"refs/heads/{branch}", "sha": commit["sha"]})
    source = f"https://github.com/{env['GITHUB_REPOSITORY']}"
    pull = request("POST", f"{repo}/pulls", {
        "title": title, "base": "main", "head": branch,
        "body": (
            f"플랫폼 이미지(API, 게이트웨이)와 lock 항목을 `{env['IMAGE']}`로 고정합니다.\n\n"
            f"platform 커밋: {source}/commit/{env['GITHUB_SHA']}\n\n"
            f"빌드: {source}/actions/runs/{env['GITHUB_RUN_ID']}/attempts/{env['GITHUB_RUN_ATTEMPT']}\n\n"
            "workflow는 이미지를 올리고 이 PR을 열기만 합니다. 머지한 뒤 플랫폼 Helm 릴리스를 upgrade하거나 "
            "Argo CD Application을 sync해야 반영되며, 실행 중인 버전은 workflow가 확인하지 않습니다(infra ADR 0024)."
        ),
    })
    return pull["html_url"]


def main():
    url = create_pull_request(os.environ)
    summary = f"## Platform image published\n\nImage: `{os.environ['IMAGE']}`\n\n"
    summary += f"Infra review PR: {url}\n" if url else "Infra main already pins this image; no PR was created.\n"
    summary += "\nThe running platform was not changed. Review, merge and the Helm upgrade or Argo CD sync are separate steps.\n"
    print(summary)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a", encoding="utf-8") as output:
            output.write(summary)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.stderr.strip() or str(error)) from error
