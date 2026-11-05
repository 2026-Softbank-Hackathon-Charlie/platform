#!/usr/bin/env bash
# Imports a public repository (an open-source app) into the org as a PRIVATE
# copy the platform's GitHub App can read, so it can be deployed from a branch.
#
# GitHub keeps forks of public repositories public, so this is a copy, not a
# fork: the upstream default branch with its full history and the tags in it.
# The copy's description records the upstream URL for later syncs
# (git remote add upstream <url>; git fetch upstream).
#
# Steps:
#   1. bare-clone the upstream default branch (fails early on a bad URL)
#   2. create <org>/<name>, private
#   3. turn GitHub Actions off in it, so upstream workflows never run on our
#      runners or minutes
#   4. push the clone
#   5. add the repository to the GitHub App installation, unless the
#      installation already sees all repositories
#   6. add the topic hopcharlie-imported, last, so a repository that has it
#      was imported completely
#
# The platform issues push tokens (POST /v1/github/push-tokens, an hour,
# contents write on this repository only) only for repositories with the
# topic hopcharlie-imported, so the AI agent can push its hopcharlie branch
# here and nowhere else (ADR 0023).
#
# It stops if <org>/<name> already exists, so it never touches a team repo.
#
# Needs git and gh, logged in as an org owner. Step 5 needs INSTALL_TOKEN, a
# personal access token (classic) with the repo scope, used for that one call
# only: GitHub refuses the call with the gh CLI's own login token (403 "You
# do not have permission to modify this app"). Without it the script stops
# before creating anything.
#   INSTALL_TOKEN=ghp_... scripts/import-repo.sh https://github.com/owner/app          # name: app
#   INSTALL_TOKEN=ghp_... scripts/import-repo.sh https://github.com/owner/app my-app   # name: my-app
#   ORG=... INSTALLATION_ID=... scripts/import-repo.sh <url>    # another org or App
set -euo pipefail

org=${ORG:-2026-Softbank-Hackathon-Charlie}
installation=${INSTALLATION_ID:-178050051} # hopcharlie-platform on the org

if [[ $# -lt 1 || $# -gt 2 ]]; then
	echo "usage: $0 UPSTREAM_GIT_URL [NAME]" >&2
	exit 2
fi
upstream=${1%/}
name=${2:-$(basename "${upstream%.git}")}
if [[ ! $name =~ ^[A-Za-z0-9._-]+$ ]]; then
	echo "name must be letters, digits and . _ -, got '$name'" >&2
	exit 2
fi
repo=$org/$name

if gh repo view "$repo" --json name >/dev/null 2>&1; then
	echo "$repo already exists; pick another NAME" >&2
	exit 1
fi

selection=$(gh api "orgs/$org/installations" --jq ".installations[] | select(.id == $installation) | .repository_selection")
if [[ -z $selection ]]; then
	echo "no GitHub App installation $installation on $org" >&2
	exit 1
fi
if [[ $selection != all && -z ${INSTALL_TOKEN:-} ]]; then
	echo "INSTALL_TOKEN (a classic personal access token with the repo scope) is required to add $repo to the installation" >&2
	exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "==> cloning $upstream (default branch and its tags)"
git clone --quiet --bare --single-branch "$upstream" "$work/src.git"
branch=$(git -C "$work/src.git" symbolic-ref --short HEAD)

echo "==> creating $repo (private)"
gh repo create "$repo" --private --description "Imported from $upstream" >/dev/null

echo "==> turning GitHub Actions off in $repo"
gh api -X PUT "repos/$repo/actions/permissions" -F enabled=false >/dev/null

echo "==> pushing $branch and tags"
# gh supplies the credentials for this push only; no token in the URL.
git -C "$work/src.git" -c credential.helper= -c 'credential.helper=!gh auth git-credential' \
	push --quiet --mirror "https://github.com/$repo.git"

if [[ $selection == all ]]; then
	echo "==> the GitHub App installation $installation sees every repository; nothing to add"
else
	echo "==> adding $repo to the GitHub App installation $installation"
	id=$(gh api "repos/$repo" --jq .id)
	# No leading slash: Git Bash on Windows would turn it into a file path.
	if ! GH_TOKEN=$INSTALL_TOKEN gh api -X PUT "user/installations/$installation/repositories/$id" >/dev/null; then
		echo "$repo is imported, but could not be added to the installation." >&2
		echo "Add it at https://github.com/organizations/$org/settings/installations/$installation" >&2
		echo "then mark it imported: gh repo edit $repo --add-topic hopcharlie-imported" >&2
		exit 1
	fi
fi

echo "==> marking $repo with the topic hopcharlie-imported"
gh repo edit "$repo" --add-topic hopcharlie-imported >/dev/null

cat <<EOF

Imported $upstream as $repo (default branch $branch).
Next: on a new branch, add a Dockerfile and platform.yaml, push it, then
  charlie app validate --remote --repo $repo --branch BRANCH
  charlie app add --repo $repo --branch BRANCH --env eks --watch
EOF
