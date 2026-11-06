# SEC-09 OCSF Export Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** `GET /v1/security/ocsf` also exports vulnerabilities (OCSF Vulnerability Finding 2002) and control evaluations (Compliance Finding 2003), and the console shows the export in a new tab.

**Spec:** `docs/superpowers/specs/2026-11-07-sec09-ocsf-export-design.md`

## Global Constraints

- OCSF version stays `1.9.0` (`OCSFVersion`). Every event written must validate against the official class schema in `internal/security/testdata/ocsf-1.9.0/` (cloud profile). Only fields the schema defines.
- Response keeps `ocsfVersion`, `generatedAt`, `findings` unchanged; adds required arrays `vulnerabilities` and `controls` (always present, possibly empty).
- Vulnerability: status New(1) or Suppressed(3) with `status_detail` = exception reason when an active exception is attached.
- KEV: only when `exploitation.kev.status == listed` write `is_exploit_available: true` and `exploit_ref_url: "https://www.cisa.gov/known-exploited-vulnerabilities-catalog"`. Otherwise omit both. EPSS: only when a score is present write `cve.epss` {score as string, percentile}. Otherwise omit.
- Controls: result pass → compliance status Pass(1), fail → Fail(3), unknown → Unknown(0); not_applicable is not exported.
- `severity` query filter applies to `findings` only.
- If vulnerabilities/controls cannot be built, return those arrays empty and still 200.
- Exceptions never change severity.
- Korean UI copy exactly as in Task 3.
- Platform worktree `.worktrees/platform-security-sec05`, branch `codex/sec09-ocsf-export`. Frontend worktree `.worktrees/frontend-security-sec06`, branch `codex/sec09-ocsf-tab` from origin/main.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never use `git stash`.

## Order

Task 1 (platform) → Task 2 (frontend; needs Task 1's openapi.yaml). Task 2 may start once Task 1's contract commit exists.

---

### Task 1: Platform — OCSF vulnerabilities and controls

**Files:**
- Already added (uncommitted): `internal/security/testdata/ocsf-1.9.0/vulnerability_finding.json` (official, unmodified, fetched 2026-11-07 from `https://schema.ocsf.io/schema/classes/vulnerability_finding?profiles=cloud`). Add a row for it to `testdata/ocsf-1.9.0/README.md`.
- Modify `internal/security/ocsf.go` (+ `ocsf_test.go`), `internal/api/handlers_security.go` (`securityOCSF`), `api/openapi.yaml` (`SecurityOCSFExport`: add `vulnerabilities`, `controls` arrays, same free-form item style and description as `findings`), regenerate `pkg/apiv1` (`go generate ./pkg/apiv1`; no `$ref` siblings), API test in the existing security API test file.

**Interfaces:**

```go
const ocsfVulnerability = 2002

// OCSFVulnerabilityFinding: one SecurityVulnerability as OCSF 2002.
// Same common fields as OCSFFinding (activity, category 2 Findings, class, type_uid = class*100+activity,
// severity, status, status_detail, time, cloud, finding_info, metadata, resources) plus
// Vulnerabilities []OCSFVulnerability `json:"vulnerabilities"` (exactly one element).
type OCSFVulnerability struct { /* cve{uid, cvss[], epss?}, title, severity, affected_packages[{name, version, purl?, type?, fixed_in_version?}],
   is_fix_available, is_exploit_available?, exploit_ref_url?, references[] */ }

func OCSFVulnerabilityEvent(v apiv1.SecurityVulnerability, at time.Time) OCSFVulnerabilityFinding
func OCSFControlEvent(e apiv1.SecurityControlEvaluation, c apiv1.SecurityControl) OCSFFinding // class 2003, compliance{control, status_id, status}
// ToOCSF keeps its signature for findings; add:
func ToOCSFAll(rep apiv1.SecurityReport, vulns *apiv1.SecurityVulnerabilityReport, controls *apiv1.SecurityControlReport) OCSFExport
```

Mapping notes (verify each against the schema file; the schema wins if this text is wrong):
- `finding_info.uid` = vulnerability id / evaluation id; `finding_info.title` = advisory + package name / control id + title; `resources` = image digest (uid) for a vulnerability, asset for a control (skip `*`? no — keep it as uid `*` only if the schema allows; otherwise omit resources).
- `cve.uid` = the CVE (`VulnerabilityCVE`); when there is no CVE, put the advisory id in `cve.uid` only if the schema accepts any string (it does: `uid` is a string); `cve.cvss` from `v.cvss` (base_score, version, vector_string).
- `time` = vulnerability's scan time if available in the report, else report generatedAt; controls use `evaluatedAt`. Epoch ms.
- Severity mapping reuses the existing helper; `unknown`/`negligible` vulnerability severities → map to OCSF Unknown(0)/Informational(1) consistently and test it.
- `metadata` same as findings (version 1.9.0, product HopCharlie, profiles ["cloud"]).

**Handler:** `securityOCSF` builds the report as today, then the collection (as `securityControls`/`securityVulnerabilities` do, without environment filter), builds `VulnerabilityReport` + `CompareDeployed` + `AttachExploitation` + `AttachVulnerabilityExceptions`, and `ControlReport`; then `ToOCSFAll`. Factor the vulnerability-report assembly into one helper used by both handlers (no duplication). Failure to get the collection → empty arrays (log).

**Tests (TDD):** extend `ocsfSchemas` to load class 2002; every event from `ToOCSFAll` over samples (with/without CVE, KEV listed/not, EPSS present/absent, exception, fixed/not fixed; controls pass/fail/unknown/not_applicable) validates; mapping assertions per Global Constraints; validator still rejects invalid events; API test: GET /v1/security/ocsf returns 200 with the three arrays and a suppressed vulnerability when an exception is active.

Run: `go build ./... && go vet ./... && go test ./internal/security/ ./internal/api/ -count=1`.

Commits: "feat(api): OCSF 내보내기 계약에 취약점·통제 추가 (SEC-09)" (yaml + generated) then "feat(security): 취약점·통제 평가 OCSF 내보내기 (SEC-09)".

---

### Task 2: Frontend — `OCSF 내보내기` tab

**Files:** `src/api/schema.d.ts` (regenerate: `PLATFORM_OPENAPI=../platform-security-sec05/api/openapi.yaml npm run gen:api`), `src/api/client.ts` (`getSecurityOCSF()` following existing helpers), demo data source used by other security tabs (add a small OCSF demo export: 2 findings, 2 vulnerabilities incl. one Suppressed with KEV, 3 controls Pass/Fail/Unknown), `src/pages/SecurityWorkspace.tsx` (+ a new component file `src/components/SecurityOCSF.tsx`), `src/styles/security.css`, tests.

- Tab: add `{ id: "ocsf", label: "OCSF 내보내기" }` as the last tab; route `#security/ocsf`.
- Header line: `OCSF {ocsfVersion} · 생성 {MM/DD HH:mm KST}` and a button `JSON 내려받기` (downloads the whole response as `hopcharlie-ocsf-YYYYMMDD-HHmm.json`, via Blob + object URL).
- Summary table, three rows: `발견 항목` (class `2004 / 2003`), `취약점` (`2002`), `통제 평가` (`2003`); columns `종류`, `OCSF 클래스`, `건수`, `상태` (counts by `status`: e.g. `New 39 · Suppressed 3`; for controls by `compliance.status`: `Pass 20 · Fail 5 · Unknown 2`).
- Event viewer: select `종류` (발견 항목/취약점/통제 평가) and a select of events of that kind labelled by `finding_info.title` (fallback uid); below, `<pre>` with `JSON.stringify(event, null, 2)`. Default: first vulnerability event if any, else first finding.
- Empty kind: `내보낼 이벤트가 없습니다.` Load failure: show the server message with role="alert" (follow existing error pattern).
- Demo mode shows the same screen from demo data.
- Tests: tab exists and routes; summary counts and status breakdown; selecting an event shows its JSON; download creates a blob with the response; empty and error states; demo mode renders.
- Run `npx vitest run` and `npx tsc -b --noEmit`. Commit "feat(security): OCSF 내보내기 탭 (SEC-09)".
