# SEC-08 Exceptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Operators can put a time-limited, audited exception on a security finding or a vulnerability; reports, OCSF and the console show it.

**Architecture:** A `security_exceptions` table behind the store interface (memory + Postgres + conformance), three API routes, and report-time attachment of active exceptions (`exception` field). The console adds chips, a filter, sort-last and register/revoke in the detail panels.

**Spec:** `docs/superpowers/specs/2026-11-07-sec08-exceptions-design.md`

## Global Constraints

- Only `operator` may create or revoke; `viewer` and `agent` get 403. Listing is `viewer`.
- `reason`: required, 1–500 characters after trimming.
- `expiresAt`: required, after now, at most 30 days (720h) after now; for a vulnerability whose CVE is KEV-listed in the server's KEV cache, at most 7 days (168h).
- An exception is active when `revokedAt` is unset and `expiresAt` is after now. One active exception per (targetType, targetId); otherwise 409.
- Severity changes never end an exception; expiry and revoke do.
- Exceptions never change `severity`. OCSF: Suppressed (status_id 3) with status_detail = reason.
- If the exception store cannot be read while building a report, the report is returned without exceptions and the error is logged.
- Korean UI copy exactly as written in Task 3.
- Platform worktree `.worktrees/platform-security-sec05`, branch `codex/sec08-exceptions`. Frontend worktree `.worktrees/frontend-security-sec06`, branch `codex/sec08-exceptions-ui` from origin/main.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never use `git stash` (shared across worktrees).

## Order

Task 1 (contract) → then Task 2 (platform) and Task 3 (frontend) in parallel.

---

### Task 1: API contract

**Files:** `api/openapi.yaml`, regenerate `pkg/apiv1/apiv1.gen.go` (`go generate ./pkg/apiv1`).

- [ ] Add schemas (near `SecurityVulnerability`):

```yaml
    SecurityException:
      description: |
        An operator's time-limited exception for one finding or vulnerability
        (SEC-08, ADR 0034). It never changes severity. Active while not revoked
        and before expiresAt; a severity change does not end it.
      type: object
      required: [id, targetType, targetId, reason, expiresAt, createdBy, createdAt]
      properties:
        id: { type: string, example: sx_3f2a9c1b }
        targetType: { $ref: "#/components/schemas/SecurityExceptionTarget" }
        targetId: { type: string, description: "a SecurityFinding id or a SecurityVulnerability id" }
        reason: { type: string }
        expiresAt: { type: string, format: date-time }
        createdBy: { type: string }
        createdAt: { type: string, format: date-time }
        revokedAt: { type: string, format: date-time }
        revokedBy: { type: string }

    SecurityExceptionTarget:
      type: string
      enum: [finding, vulnerability]

    SecurityExceptionRequest:
      type: object
      required: [targetType, targetId, reason, expiresAt]
      properties:
        targetType: { $ref: "#/components/schemas/SecurityExceptionTarget" }
        targetId: { type: string }
        reason: { type: string, minLength: 1, maxLength: 500 }
        expiresAt: { type: string, format: date-time, description: "after now and at most 30 days later; at most 7 days for a KEV-listed vulnerability" }

    SecurityExceptionList:
      type: object
      required: [exceptions]
      properties:
        exceptions: { type: array, items: { $ref: "#/components/schemas/SecurityException" } }
```

- [ ] Add optional `exception: { $ref: "#/components/schemas/SecurityException" }` to `SecurityFinding.properties` and `SecurityVulnerability.properties` (description: "the active exception, if any").
- [ ] Add paths, following the style of the existing `/v1/security/scans` path (tags, security, error responses):
  - `GET /v1/security/exceptions` → 200 `SecurityExceptionList` (viewer). operationId `listSecurityExceptions`.
  - `POST /v1/security/exceptions` body `SecurityExceptionRequest` → 201 `SecurityException`; 400 invalid; 403; 409 an active exception exists (operator). operationId `createSecurityException`.
  - `DELETE /v1/security/exceptions/{id}` → 200 `SecurityException`; 404; 409 already revoked (operator). operationId `revokeSecurityException`.
- [ ] `go generate ./pkg/apiv1 && go build ./... && go vet ./pkg/apiv1`. Report the generated names (`SecurityException`, `SecurityExceptionTarget` constants, `SecurityExceptionRequest`, `SecurityExceptionList`, the new `Exception *SecurityException` fields).
- [ ] Commit "feat(api): 보안 예외 계약 추가 (SEC-08)".

---

### Task 2: Platform store, API routes, report attachment, OCSF

**Files:**
- Create `internal/store/migrations/0013_security_exceptions.sql`, `internal/store/exceptions.go` (type + interface), `internal/store/postgres_exceptions.go`, `internal/store/storetest/memory_exceptions.go`, `internal/store/storetest/conformance_exceptions.go` (follow `scans.go` / `postgres_scans.go` / `memory_scans.go` / `conformance_scans.go` exactly, including how conformance is wired into memory and Postgres tests and how `Store` embeds `ScanStore` in store.go:163).
- Create `internal/security/exceptions.go` (+ `_test.go`).
- Modify `internal/security/ocsf.go`, `internal/security/exploit.go` (one read-only method), `internal/api/server.go` (routes), `internal/api/handlers_security.go`, plus an API test file `internal/api/security_exceptions_test.go` following existing API test harness patterns.

**Interfaces:**

```go
// store
type SecurityException struct {
	ID, TargetType, TargetID, Reason, CreatedBy string
	ExpiresAt, CreatedAt time.Time
	RevokedAt *time.Time
	RevokedBy string
}
type ExceptionStore interface {
	// CreateSecurityException stores e. ErrConflict (wrapped) when the target has
	// an exception that is not revoked and expires after e.CreatedAt.
	CreateSecurityException(ctx context.Context, e SecurityException) error
	// RevokeSecurityException sets revoked_at/by. ErrNotFound for an unknown id,
	// ErrConflict when it is already revoked. Returns the updated row.
	RevokeSecurityException(ctx context.Context, id, by string, at time.Time) (SecurityException, error)
	// ListSecurityExceptions returns all exceptions, newest created first.
	ListSecurityExceptions(ctx context.Context) ([]SecurityException, error)
}
```
Embed `ExceptionStore` in `Store`. Postgres create must be one statement: `INSERT ... SELECT ... WHERE NOT EXISTS (SELECT 1 FROM security_exceptions WHERE target_type=$ AND target_id=$ AND revoked_at IS NULL AND expires_at > $created_at)`; 0 rows → ErrConflict. Check the existing names of ErrNotFound/ErrConflict in store.go.

```go
// security
func ActiveExceptions(all []apiv1.SecurityException, now time.Time) map[string]apiv1.SecurityException // key: targetType+"/"+targetId
func AttachFindingExceptions(rep *apiv1.SecurityReport, active map[string]apiv1.SecurityException)
func AttachVulnerabilityExceptions(rep *apiv1.SecurityVulnerabilityReport, active map[string]apiv1.SecurityException)
func (x *ExploitIntel) KEVListed(cve string) bool // cached catalog only, never fetches; false when nil/unknown/older than 72h
```
Also export a helper to get a vulnerability's CVE (`vulnCVE` exists; export as `VulnerabilityCVE` or reuse inside package).

OCSF: in `OCSF(f)`, when `f.Exception != nil` → `StatusID: 3, Status: "Suppressed"` and a new `StatusDetail string \`json:"status_detail,omitempty"\`` = reason.

API:
- Routes: `GET /v1/security/exceptions` viewer, `POST /v1/security/exceptions` operator, `DELETE /v1/security/exceptions/{id}` operator. When `s.d.Store` is nil → 503 like other store-backed routes.
- POST validation per Global Constraints (400 `invalid_request` with a message saying what is wrong). For `vulnerability`: find the id in `s.d.ImageScans.Latest()` vulnerabilities (400 "unknown vulnerability" if absent); if `s.d.Exploits.KEVListed(cve)` the limit is 7 days. ID: `"sx_" + 16 hex chars` random. createdBy = principal name. `noteAudit(r, "exception", map{targetType,targetId,reason,expiresAt})`.
- DELETE: revokedBy = principal name; `noteAudit(r, "exception", map{id,targetType,targetId})`.
- Reports: in `security`, `securityOCSF` (after `securityReport`) and `securityVulnerabilities` (after `AttachExploitation`), list exceptions from the store, compute active at `time.Now()`, attach. On a store error: log and skip attaching.
- Store row ↔ apiv1 conversion in one place (handlers or security package).

Tests (TDD): conformance cases (create; active duplicate → conflict; re-create after expiry allowed; revoke; revoke twice → conflict; unknown id → not found; list newest first) run for memory and Postgres; security unit tests (active filter: revoked/expired excluded; attach to findings/vulnerabilities; OCSF Suppressed + status_detail; KEVListed from cache); API tests (viewer/agent 403 on POST/DELETE; reason empty/too long 400; expiresAt past/31 days 400; KEV vulnerability 8 days 400 and 7 days 201 — set up a fake ExploitIntel cache via an in-package helper or by injecting a KEV server; unknown vulnerability 400; 409 duplicate; DELETE 404/409; GET /v1/security/vulnerabilities shows `exception` only while active; audit entry has the note).

Run: `go build ./... && go vet ./... && go test ./internal/security/ ./internal/store/... ./internal/api/ -count=1` (Postgres tests need PLATFORM_TEST_DATABASE_URL; skip locally if absent and say so; CI runs them). `-race` cannot run locally.

Commits: one for store, one for security+API ("feat(security): 발견 항목·취약점 예외 등록과 감사 (SEC-08)").

---

### Task 3: Frontend

**Files:** `src/api/schema.d.ts` (regenerate: `PLATFORM_OPENAPI=../platform-security-sec05/api/openapi.yaml npm run gen:api`), `src/api/types.ts`, `src/api/client.ts` (three functions, following existing request helpers), `src/data/security.ts`, `src/data/vulnPriority.ts`, `src/pages/SecurityWorkspace.tsx`, `src/styles/security.css`, tests next to them.

- Data: `VulnerabilityFinding.exception?: { id: string; reason: string; expiresAt: string; createdBy: string }`; same on `SecurityFindingRow`. Map from API `exception`.
- Client: `listSecurityExceptions()`, `createSecurityException(body)`, `revokeSecurityException(id)`.
- Sort: `sortByPriority` puts groups whose first member has an active `exception` after all others (keep the existing order inside each part).
- Lists (vulnerabilities and findings): chip text exactly `예외 · MM/DD까지` (KST date of expiresAt); excepted row gets class `sp-excepted` (opacity .6). Filter select labeled `예외` with options `제외` (default), `포함`, `예외만`.
- Detail panels (`VulnerabilityDetail`, `FindingDetail`), live mode only:
  - No exception: button `예외 등록` opens an inline form: textarea `사유` (required, max 500), date input `만료일` (default today+30 days; max today+30 days, or today+7 days when `kev === true` for a vulnerability), buttons `등록` / `취소`. Hint under the form: `예외는 심각도를 바꾸지 않습니다. 만료일이 지나면 다시 미처리로 보입니다.` (for KEV add: ` KEV 등재 취약점은 최대 7일입니다.`). Send expiresAt as the chosen date at 23:59:59 KST in ISO.
  - With exception: text `예외 · MM/DD까지 · {createdBy}` and the reason, button `예외 해제`.
  - On success call the page's existing `reload`; on failure show the server's error message inline (role="alert").
- Demo mode: no buttons, no form.
- Tests: chip and muted class; filter three options; sort-last; form validation (empty reason disables 등록; max date for KEV is +7); create/revoke call the client with the right body and then reload (mock fetch like existing tests); demo mode shows no button.
- Run `npx vitest run` and `npx tsc -b --noEmit`. Commit "feat(security): 예외 등록·해제와 예외 표시 (SEC-08)".
