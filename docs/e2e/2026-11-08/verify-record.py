#!/usr/bin/env python3
"""Validate stored E2E evidence without contacting any service."""
import hashlib
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parent / 'evidence'
documents = {}
patterns = [r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----',
            r'AKIA[0-9A-Z]{16}', r'sk-[A-Za-z0-9_-]{24,}',
            r'onp_[A-Za-z0-9_-]{30,}', r'eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.']
for path in ROOT.glob('*.json'):
    text = path.read_text(encoding='utf-8')
    assert not any(re.search(pattern, text) for pattern in patterns), 'credential-like content: ' + path.name
    documents[path.name] = json.loads(text)

for entry in documents['manifest.json']:
    assert entry['sha256Scope'] == 'UTF-8 text normalized to LF'
    assert hashlib.sha256((ROOT / entry['file']).read_text(encoding='utf-8').encode()).hexdigest() == entry['sha256'], 'evidence checksum mismatch'

db = documents['db-pipeline.json']
assert db['pipeline_passed'] and db['exit_code'] == 0 and not db['output_truncated']
assert {x['name'] for x in db['scenarios']} == {'lost-response', 'canary-fault', 'observe-rollback', 'migrate'}
assert all(x['passed'] and x['writes_failed'] == 0 for x in db['scenarios'])
assert sum(x['writes_ok'] for x in db['scenarios']) == 11125
assert documents['db-cleanup.json'] == {'kind_nodes_remaining': 0, 'fixture_stores_remaining': 0, 'cleanup_status': 'verified'}

files = documents['file-pipeline.json']
assert files['pipeline_passed'] and files['exit_code'] == 0 and not files['output_truncated']
assert len(files['scenarios']) == 10 and all(x['passed'] for x in files['scenarios'])
assert documents['file-cleanup.json'] == {'fixture_containers_remaining': 0, 'cleanup_status': 'verified'}

browsers = [d for name, d in documents.items() if name.startswith('browser-')]
for report in browsers:
    assert not report['security']['har'] and not report['security']['trace'] and not report['security']['rawResponseBody']
    assert report['cleanup']['browserClosed'] and not report['cleanup']['persistentProfileUsed']
    assert report['cleanup']['credentialPersistedToNativeStorage'] is False
    assert all(r['method'] in {'GET', 'HEAD', 'OPTIONS'} for r in report['requests'])
assert sum(r['paidEntryRequestsAttempted'] for r in browsers) == 1
assert documents['recommendation-jobs.json']['metadataOnly'] and documents['recommendation-jobs.json']['jobCount'] == 1
assert documents['onprem-configuration.json']['centralSignalReceiving'] == 'not_measured'
assert documents['component-ci.json']['artifactContainsRawLogs'] is False
if 'browser-final.json' in documents:
    final = documents['browser-final.json']
    assert not any(x['result'] == 'FAIL' for x in final['scenarios']), 'final UI failure is unresolved'
    assert not final['pageErrors'], 'uncaught browser JavaScript error'
    assert final['paidEntryRequestsAttempted'] == 0
    assert final['security']['blockedMutationAttempts'] == 0
    assert len(final['scenarios']) == 10 and all(x['result'] == 'PASS' for x in final['scenarios'])
    mobile = next(x for x in final['scenarios'] if x['id'] == 'B01-mobile-layout')
    assert mobile['documentWidth'] <= mobile['clientWidth'] + 1
    assert mobile['freshFocused']['documentWidth'] <= mobile['freshFocused']['clientWidth'] + 1
    assert mobile['tableKeyboard']['after'] > mobile['tableKeyboard']['before']
    assert mobile['tableKeyboard']['hiddenLabelCount'] == 12 and mobile['tableKeyboard']['hiddenLabelsContained']
    assert mobile['japaneseLayout']['lang'] == 'ja' and mobile['japaneseLayout']['actionOverlapCount'] == 0
    assert mobile['japaneseLayout']['documentWidth'] <= mobile['japaneseLayout']['clientWidth'] + 1
    rollout = documents['console-final-rollout.json']
    assert rollout['reviewedImageReady'] and rollout['readyReplicas'] == 1
    assert rollout['generation'] == rollout['observedGeneration'] == 82
    assert 'eb9cc64@sha256:c91920ebe96ce7e8b4ac39fb03a8eb78731c10d3f8d76f9e38639bd5eb31d70c' in rollout['image']
else:
    print('Final UI retest pending; historical failures are preserved.')
print(f'Validated {len(documents)} JSON artifacts, source-copy checksums, test scope and credential patterns; no network access.')
