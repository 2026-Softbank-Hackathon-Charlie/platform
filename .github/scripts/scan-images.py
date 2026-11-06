#!/usr/bin/env python3
"""Scans the platform's running image digests with Grype (SEC-04, ADR 0034).

Reads the running images from GET /v1/security/vulnerabilities, scans each
digest whose stored scan is missing, failed or older than RESCAN_HOURS with
`grype registry:<repository>@<digest> -o json`, and posts every result to
POST /v1/security/scans. A digest that no reference could be scanned from is
posted as failed with the scanner's error: never as zero vulnerabilities.

The API never scans; this script runs in the image scan workflow.

Environment:
  PLATFORM_API_URL   e.g. https://api.hopcharlie.dev
  SCANNER_TOKEN      a PLATFORM_TOKENS entry of role agent (never printed)
  GRYPE              the pinned grype binary
  DOCKER_CONFIG      the directory whose config.json this writes (registry logins)
  ECR_PASSWORD       optional: `aws ecr get-login-password` for ECR_REGION
  ECR_REGION         optional: the region of ECR_PASSWORD
  GAR_TOKEN          optional: a Google access token that reads Artifact Registry
  MAX_DIGESTS        optional: digests per run, default 40
"""

import base64
import datetime
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

RESCAN_HOURS = 24
RETRY_FAILED_HOURS = 1
GRYPE_TIMEOUT = 600
DIGEST = re.compile(r'^sha256:[0-9a-f]{64}$')


def repository(image):
    """The repository of an image reference, without tag or digest."""
    image = image.split('@', 1)[0]
    slash = image.rfind('/')
    colon = image.rfind(':')
    if colon > slash:
        image = image[:colon]
    return image


def registry(repo):
    first = repo.split('/', 1)[0]
    if '/' in repo and ('.' in first or ':' in first or first == 'localhost'):
        return first
    return 'docker.io'


def ecr_host(host, region):
    return re.fullmatch(r'[0-9]{12}\.dkr\.ecr\.' + re.escape(region) + r'\.amazonaws\.com', host) is not None


def gar_host(host):
    return re.fullmatch(r'[a-z0-9-]+-docker\.pkg\.dev', host) is not None


def parse_time(s):
    if not s:
        return None
    return datetime.datetime.fromisoformat(s.replace('Z', '+00:00'))


def targets(report, now):
    """Digests to scan, oldest need first: {digest: [repository@digest, ...]}.

    ECR references come first, so a digest copied to another registry is
    scanned from the one the scanner can read."""
    scans = {s['digest']: s for s in report.get('scans', [])}
    refs = {}
    for img in report.get('runningImages', []):
        digest = img.get('digest')
        if not digest or not DIGEST.match(digest):
            continue
        ref = repository(img['image']) + '@' + digest
        refs.setdefault(digest, [])
        if ref not in refs[digest]:
            refs[digest].append(ref)
    out = {}
    for digest in sorted(refs):
        s = scans.get(digest)
        finished = parse_time(s.get('finishedAt')) if s else None
        if s and finished:
            age = now - finished
            if s['status'] == 'succeeded' and age < datetime.timedelta(hours=RESCAN_HOURS):
                continue
            if s['status'] != 'succeeded' and age < datetime.timedelta(hours=RETRY_FAILED_HOURS):
                continue
        out[digest] = sorted(refs[digest], key=lambda r: ('.dkr.ecr.' not in r, r))
    return out


def docker_config(hosts, password, region, gar_token=''):
    """config.json for the ECR registries of region and the Artifact Registry
    hosts among hosts."""
    auths = {}
    ecr = base64.b64encode(f'AWS:{password}'.encode()).decode() if password and region else ''
    gar = base64.b64encode(f'oauth2accesstoken:{gar_token}'.encode()).decode() if gar_token else ''
    for host in sorted(hosts):
        if ecr and ecr_host(host, region):
            auths[host] = {'auth': ecr}
        elif gar and gar_host(host):
            auths[host] = {'auth': gar}
    return {'auths': auths}


def scan_body(ref, digest, status, started, finished, version, report=None, message=None):
    """The request body; the report is embedded as Grype wrote it."""
    head = {'image': ref, 'digest': digest, 'status': status, 'startedAt': started, 'finishedAt': finished}
    if version:
        head['scannerVersion'] = version
    if message:
        head['message'] = message
    body = json.dumps(head)
    if report is not None:
        body = body[:-1] + ', "report": ' + report.decode() + '}'
    return body.encode()


class API:
    def __init__(self, url, token):
        self.url, self.token = url.rstrip('/'), token

    def call(self, method, path, body=None):
        req = urllib.request.Request(self.url + path, data=body, method=method, headers={
            'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json', 'User-Agent': 'hopcharlie-image-scan'})
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()


def now_iso():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='seconds').replace('+00:00', 'Z')


def grype_version(grype):
    out = subprocess.run([grype, 'version', '-o', 'json'], capture_output=True, text=True, timeout=60)
    try:
        return json.loads(out.stdout).get('version', '')
    except ValueError:
        return ''


def scan(grype, ref):
    """(report bytes, None) or (None, error)."""
    try:
        out = subprocess.run([grype, 'registry:' + ref, '-o', 'json', '-q'], capture_output=True, timeout=GRYPE_TIMEOUT)
    except subprocess.TimeoutExpired:
        return None, f'grype timed out after {GRYPE_TIMEOUT}s'
    if out.returncode != 0:
        err = out.stderr.decode(errors='replace').strip().splitlines()
        return None, (err[-1] if err else f'grype exited {out.returncode}')[:400]
    try:
        json.loads(out.stdout)
    except ValueError:
        return None, 'grype wrote no JSON report'
    return out.stdout, None


def main():
    api = API(os.environ['PLATFORM_API_URL'], os.environ['SCANNER_TOKEN'])
    grype = os.environ['GRYPE']
    limit = int(os.environ.get('MAX_DIGESTS', '40'))

    code, body = api.call('GET', '/v1/security/vulnerabilities')
    if code != 200:
        print(f'::error::GET /v1/security/vulnerabilities: {code} {body[:300]!r}')
        return 1
    report = json.loads(body)
    todo = targets(report, datetime.datetime.now(datetime.timezone.utc))
    running = {i['digest'] for i in report.get('runningImages', []) if i.get('digest')}
    print(f'{len(running)} running digests, {len(todo)} to scan, scanning up to {limit}')

    hosts = {registry(repository(r)) for refs in todo.values() for r in refs}
    cfg_dir = os.environ['DOCKER_CONFIG']
    os.makedirs(cfg_dir, exist_ok=True)
    cfg = docker_config(hosts, os.environ.get('ECR_PASSWORD', ''), os.environ.get('ECR_REGION', ''), os.environ.get('GAR_TOKEN', ''))
    with open(os.path.join(cfg_dir, 'config.json'), 'w', encoding='utf-8') as f:
        json.dump(cfg, f)
    print(f'registry logins: {", ".join(sorted(cfg["auths"])) or "none"}')

    version = grype_version(grype)
    failures = 0
    for digest, refs in list(todo.items())[:limit]:
        started, errors = now_iso(), []
        for ref in refs:
            out, err = scan(grype, ref)
            if out is not None:
                code, resp = api.call('POST', '/v1/security/scans', scan_body(ref, digest, 'succeeded', started, now_iso(), version, report=out))
                print(f'{digest[:19]} {ref.split("@")[0]}: scanned, POST {code}')
                break
            errors.append(f'{ref.split("@")[0]}: {err}')
        else:
            msg = '; '.join(errors)
            missing = sorted({registry(repository(r)) for r in refs} - set(cfg['auths']) - {'docker.io'})
            if missing:
                msg += f' (the scanner has no registry credentials for {", ".join(missing)})'
            code, resp = api.call('POST', '/v1/security/scans', scan_body(refs[0], digest, 'failed', started, now_iso(), version, message=msg[:480]))
            print(f'{digest[:19]} failed: {msg[:200]}, POST {code}')
        if code != 201:
            failures += 1
            print(f'::warning::POST /v1/security/scans for {digest}: {code} {resp[:300]!r}')
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main())
