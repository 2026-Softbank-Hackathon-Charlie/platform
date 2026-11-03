# OCSF 1.9.0 class schemas

Official JSON Schemas of the Open Cybersecurity Schema Framework (Apache License 2.0),
used by `ocsf_test.go` to validate every event `ToOCSF` writes. Not used at runtime.

| File | Source |
| --- | --- |
| `detection_finding.json` | `https://schema.ocsf.io/schema/classes/detection_finding?profiles=cloud` |
| `compliance_finding.json` | `https://schema.ocsf.io/schema/classes/compliance_finding?profiles=cloud` |

Fetched 2026-11-03, when `https://schema.ocsf.io/api/version` answered `1.9.0`.
The files are unmodified. To move to a newer OCSF version, fetch both again into
`testdata/ocsf-<version>/` and change `OCSFVersion`.
